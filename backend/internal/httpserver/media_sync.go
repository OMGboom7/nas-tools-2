package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/mediasync"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type mediaSyncAPI struct {
	auth          *nativeAuthentication
	config        *config.Store
	store         *mediasync.Store
	guards        *organization.Store
	transport     http.RoundTripper
	pureGo        bool
	workerRunning *atomic.Bool
}

func (api mediaSyncAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api.auth == nil || api.config == nil {
		writeAPIError(w, 503, 503, "media configuration unavailable")
		return "", false
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return "", false
	}
	if !api.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission required")
		return "", false
	}
	if !api.pureGo || api.store == nil || api.guards == nil {
		writeAPIError(w, 501, 501, "media synchronization requires disabled legacy backend")
		return "", false
	}
	return claims.Username, true
}
func (api mediaSyncAPI) connection() (*mediaserver.Client, string, string, string, error) {
	snapshot, err := api.config.Snapshot()
	if err != nil {
		return nil, "", "", "", err
	}
	kind := strings.ToLower(strings.TrimSpace(text(objectValue(snapshot["media"])["media_server"])))
	if kind == "" {
		kind = "emby"
	}
	settings := objectValue(snapshot[kind])
	credential := text(settings["api_key"])
	if kind == "plex" {
		credential = text(settings["token"])
	}
	client, err := mediaserver.New(kind, text(settings["host"]), credential, api.transport)
	if err != nil {
		return nil, "", "", "", err
	}
	raw, err := json.Marshal(map[string]any{"kind": kind, "settings": settings})
	if err != nil {
		return nil, "", "", "", err
	}
	digest := sha256.Sum256(raw)
	return client, kind, text(settings["username"]), hex.EncodeToString(digest[:]), nil
}
func (api mediaSyncAPI) status(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	_, kind, _, digest, err := api.connection()
	if err != nil {
		writeAPIError(w, 503, 503, "media server configuration unavailable")
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 1000 {
			writeAPIError(w, 400, 400, "invalid snapshot page")
			return
		}
	}
	view, err := api.store.View(r.Context(), kind, page)
	if err != nil {
		writeAPIError(w, 503, 503, "media snapshot unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"snapshot": view, "current": view.ConfigDigest == digest, "displayOnly": true}})
}
func (api mediaSyncAPI) catalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	client, kind, username, digest, err := api.connection()
	if err != nil {
		writeAPIError(w, 503, 503, "media server configuration unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	libraries, err := client.SyncLibraries(ctx, username)
	if err != nil {
		writeAPIError(w, 502, 502, "media library list unavailable; no snapshot changed")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"server": kind, "fingerprint": digest, "libraries": libraries}})
}
func (api mediaSyncAPI) synchronize(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	var input struct {
		Confirm       bool     `json:"confirm"`
		LegacyStopped bool     `json:"legacyStopped"`
		Fingerprint   string   `json:"fingerprint"`
		Libraries     []string `json:"libraries"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid media sync request") {
		return
	}
	if !input.Confirm || !input.LegacyStopped || len(input.Libraries) == 0 || len(input.Libraries) > 100 {
		writeAPIError(w, 400, 400, "confirm selected libraries and that the legacy sync worker is stopped")
		return
	}
	client, kind, username, digest, err := api.connection()
	if err != nil {
		writeAPIError(w, 503, 503, "media server configuration unavailable")
		return
	}
	if input.Fingerprint != digest {
		writeAPIError(w, 409, 409, "media configuration changed; reload library list")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	guard, err := api.guards.AcquireJob(ctx, "media-library-sync")
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	items, err := client.CollectSnapshot(ctx, username, input.Libraries)
	if err != nil {
		writeAPIError(w, 502, 502, "media inventory incomplete; previous snapshot retained")
		return
	}
	// Revalidate scope and authorization after the potentially long remote read.
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	_, currentKind, _, currentDigest, err := api.connection()
	if err != nil || currentKind != kind || currentDigest != digest {
		writeAPIError(w, 409, 409, "media configuration changed during sync; previous snapshot retained")
		return
	}
	result, err := api.store.Publish(ctx, kind, digest, actor, input.Libraries, items)
	if err != nil {
		writeAPIError(w, 503, 503, "media snapshot not confirmed; read status before retrying")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}
