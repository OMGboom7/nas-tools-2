package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type mediaRefreshAPI struct {
	organizer      *organizationAPI
	system         *systemconfig.Store
	connection     mediaSyncAPI
	queue          *mediaserver.RefreshStore
	workers        atomic.Int32
	connectionMu   sync.Mutex
	accountEntries map[string]mediaRefreshAccountEntry
}

var mediaRefreshKeys = []string{"UserInstalledPlugins", mediaserver.RefreshPluginKey, mediaserver.RefreshControlKey}

const mediaRefreshGuard = "native-media-server-refresh"

func refreshNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func refreshID(v string, n int) bool {
	b, e := hex.DecodeString(v)
	return e == nil && len(v) == n && len(b)*2 == n && hex.EncodeToString(b) == v
}
func (api *mediaRefreshAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.organizer == nil {
		writeAPIError(w, 503, 503, "native media refresh unavailable")
		return "", false
	}
	if !api.organizer.jobsAuthorized(w, r) {
		return "", false
	}
	if api.system == nil || api.queue == nil {
		writeAPIError(w, 503, 503, "native media refresh unavailable")
		return "", false
	}
	claims, err := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return "", false
	}
	if _, err := api.authority(claims.Username); err != nil {
		writeAPIError(w, 403, 403, "current administrator authority required")
		return "", false
	}
	return claims.Username, true
}
func (api *mediaRefreshAPI) authority(actor string) (string, error) {
	snapshot, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	app := objectValue(snapshot["app"])
	if actor == "" || text(app["login_user"]) != actor || !api.organizer.auth.service.IsAdministrator(actor) {
		return "", mediaserver.ErrRefreshState
	}
	return mediaserver.RefreshDigest([]any{actor, app["login_password"], objectValue(snapshot["security"])["api_key"]}), nil
}

type mediaRefreshSnapshot struct {
	raw       map[string]string
	values    map[string]any
	control   mediaserver.RefreshControl
	installed bool
}

func (api *mediaRefreshAPI) read(ctx context.Context) (mediaRefreshSnapshot, error) {
	s := mediaRefreshSnapshot{}
	raw, err := api.system.ReadMany(ctx, mediaRefreshKeys)
	if err != nil {
		return s, err
	}
	s.raw = raw
	s.installed, err = indexerInstalled(raw["UserInstalledPlugins"], mediaserver.RefreshPluginID)
	if err != nil {
		return s, err
	}
	s.values, err = decodeMetadataConfig(raw[mediaserver.RefreshPluginKey])
	if err != nil {
		return s, err
	}
	s.control, err = mediaserver.DecodeRefreshControl(raw[mediaserver.RefreshControlKey])
	return s, err
}
func (api *mediaRefreshAPI) scope(ctx context.Context, expected mediaserver.RefreshControl) (*mediaserver.Client, error) {
	if api == nil || api.organizer == nil || !api.organizer.pureGo {
		return nil, mediaserver.ErrRefreshState
	}
	s, err := api.read(ctx)
	if err != nil {
		return nil, err
	}
	c := s.control
	if !c.Authorized || !s.installed || s.values["enable"] != true || c != expected || mediaserver.RefreshDigest(s.values) != c.PluginDigest {
		return nil, mediaserver.ErrRefreshState
	}
	spec, err := api.spec()
	if err != nil || spec.kind != c.Server || spec.digest != c.ServerDigest || spec.cloud && !c.PlexAccount {
		return nil, mediaserver.ErrRefreshState
	}
	authority, err := api.authority(c.Actor)
	if err != nil || authority != c.AuthorityDigest {
		return nil, mediaserver.ErrRefreshState
	}
	return spec.client, nil
}
func (api *mediaRefreshAPI) fingerprint(raw map[string]string, actor string) string {
	spec, err := api.spec()
	if err != nil {
		spec.digest = "unavailable"
	}
	auth, err := api.authority(actor)
	if err != nil {
		auth = "unavailable"
	}
	return mediaserver.RefreshDigest([]any{raw, actor, spec.kind, spec.digest, auth})
}
func (api *mediaRefreshAPI) status(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	s, err := api.read(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "media refresh control unavailable")
		return
	}
	jobs, err := api.queue.List(r.Context(), false, time.Now())
	if err != nil {
		writeAPIError(w, 503, 503, "media refresh receipts unavailable")
		return
	}
	_, scopeErr := api.scope(r.Context(), s.control)
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{
		"installed": s.installed, "enabled": s.values["enable"] == true, "active": scopeErr == nil,
		"server": s.control.Server, "serverId": s.control.ServerID, "actor": s.control.Actor,
		"delay": s.control.Delay, "workerRunning": api.workers.Load() > 0, "jobs": jobs,
	}})
}
func (api *mediaRefreshAPI) record(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	j, err := api.queue.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
}
func mediaRefreshError(w http.ResponseWriter, err error) {
	if errors.Is(err, mediaserver.ErrRefreshMissing) {
		writeAPIError(w, 404, 404, "media refresh receipt not found")
		return
	}
	writeAPIError(w, 409, 409, "media refresh scope or receipt could not be established; readonly refresh before any new request")
}

type mediaRefreshInput struct {
	JobID                      string `json:"jobId"`
	Whole                      bool   `json:"whole"`
	RequestID                  string `json:"requestId"`
	Fingerprint                string `json:"fingerprint"`
	Confirm                    bool   `json:"confirm"`
	ConfirmRead                bool   `json:"confirmRead"`
	ConfirmScan                bool   `json:"confirmScan"`
	ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
}
type mediaRefreshReview struct {
	ID          string                  `json:"id"`
	RequestID   string                  `json:"requestId"`
	JobID       string                  `json:"jobId"`
	Whole       bool                    `json:"whole"`
	ItemCount   int                     `json:"itemCount"`
	Actor       string                  `json:"actor"`
	Fingerprint string                  `json:"fingerprint"`
	Plan        mediaserver.RefreshPlan `json:"plan"`
	definition  mediaserver.RefreshDefinition
}

func (api *mediaRefreshAPI) review(ctx context.Context, actor string, in mediaRefreshInput) (mediaRefreshReview, error) {
	v := mediaRefreshReview{JobID: in.JobID, Whole: in.Whole}
	if !in.ConfirmRead {
		return v, mediaserver.ErrRefreshState
	}
	if in.Whole && in.JobID != "" || !in.Whole && !refreshID(in.JobID, 32) {
		return v, mediaserver.ErrRefreshState
	}
	if in.RequestID == "" {
		var err error
		in.RequestID, err = refreshNonce()
		if err != nil {
			return v, err
		}
	}
	if !refreshID(in.RequestID, 32) {
		return v, mediaserver.ErrRefreshState
	}
	s, err := api.read(ctx)
	if err != nil {
		return v, err
	}
	client, err := api.connect(ctx, s.control)
	if err != nil {
		return v, err
	}
	d := mediaserver.RefreshDefinition{Actor: actor, RequestID: in.RequestID, Source: "refresh_media_server", Reference: in.JobID, Whole: in.Whole, Control: s.control, Items: []mediaserver.RefreshItem{}}
	if !in.Whole {
		d.Items, err = api.organizer.jobs.MediaRefreshSource(ctx, in.JobID)
		if err != nil {
			return v, err
		}
		d, err = api.queue.BindSource(ctx, d)
		if err != nil {
			return v, err
		}
		ready, err := api.queue.SourceReady(ctx, d)
		if err != nil || !ready {
			return v, mediaserver.ErrRefreshState
		}
	}
	p, err := client.PlanRefresh(ctx, d.Items, d.Whole)
	if err != nil || p.ServerID != s.control.ServerID {
		return v, mediaserver.ErrRefreshState
	}
	d.Reviewed = &p
	v.definition, v.ID, v.RequestID, v.ItemCount, v.Fingerprint, v.Plan = d, mediaserver.RefreshJobID(d), d.RequestID, len(d.Items), mediaserver.RefreshDigest(d), p
	v.Actor = actor
	return v, nil
}
func (api *mediaRefreshAPI) manual(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	var in mediaRefreshInput
	if !decodeOrganizationRequest(w, r, &in, "invalid media server refresh request") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if r.PathValue("action") == "preview" {
		v, err := api.review(ctx, actor, in)
		if err != nil {
			mediaRefreshError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.PathValue("action") != "submit" || !in.ConfirmRead || !in.Confirm || !in.ConfirmScan || !in.ConfirmOldExecutorsStopped || !refreshID(in.RequestID, 32) || !refreshID(in.Fingerprint, 64) {
		writeAPIError(w, 400, 400, "fresh review and independent scan, server-side effects and old-executor consents required")
		return
	}
	id := mediaserver.RefreshJobID(mediaserver.RefreshDefinition{Actor: actor, RequestID: in.RequestID, Source: "refresh_media_server"})
	if j, err := api.queue.Get(ctx, id); err == nil {
		if j.Fingerprint != in.Fingerprint || j.Definition.Reference != in.JobID || j.Definition.Whole != in.Whole {
			mediaRefreshError(w, mediaserver.ErrRefreshState)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
		return
	} else if !errors.Is(err, mediaserver.ErrRefreshMissing) {
		mediaRefreshError(w, err)
		return
	}
	v, err := api.review(ctx, actor, in)
	if err != nil || v.Fingerprint != in.Fingerprint {
		mediaRefreshError(w, mediaserver.ErrRefreshState)
		return
	}
	// Recheck YAML/account authority after planning. SQLite performs the exact
	// installed/plugin/control comparison in the same create transaction.
	if _, err := api.scope(ctx, v.definition.Control); err != nil {
		mediaRefreshError(w, err)
		return
	}
	j, err := api.queue.Create(ctx, v.definition)
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
}
func (api *mediaRefreshAPI) stop(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid refresh stop request") {
		return
	}
	if !in.Confirm {
		writeAPIError(w, 400, 400, "explicit stop confirmation required")
		return
	}
	if err := api.queue.Finish(r.Context(), r.PathValue("id"), "stopped", "administrator stopped further requests; an already accepted server scan cannot be recalled"); err != nil {
		mediaRefreshError(w, err)
		return
	}
	j, err := api.queue.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
}

func (api *mediaRefreshAPI) tick(ctx context.Context, now time.Time) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, mediaRefreshGuard)
	if err != nil {
		return err
	}
	defer guard.Close()
	jobs, err := api.queue.List(ctx, true, now)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := api.scope(ctx, job.Definition.Control); err != nil {
			if err := api.queue.Finish(ctx, job.ID, "blocked", "original administrator, plugin or server scope is no longer current; no request sent"); err != nil {
				return err
			}
			continue
		}
		ready, err := api.queue.SourceReady(ctx, job.Definition)
		if err != nil {
			if err := api.queue.Finish(ctx, job.ID, "blocked", "original intake or enrichment receipt changed or is uncertain; no scan request sent"); err != nil {
				return err
			}
			continue
		}
		if !ready {
			next := now.Add(10 * time.Second).UTC().Format("2006-01-02T15:04:05.000000000Z")
			if err := api.queue.WaitForSource(ctx, job.ID, next); err != nil {
				return err
			}
			continue
		}
		if err := api.run(ctx, job); err != nil {
			return err
		}
	}
	return nil
}
func (api *mediaRefreshAPI) run(ctx context.Context, job mediaserver.RefreshJob) error {
	finish := func(state, reason string) error {
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return api.queue.Finish(save, job.ID, state, reason)
	}
	client, err := api.connect(ctx, job.Definition.Control)
	if err != nil {
		return finish("blocked", "original plugin, administrator or server scope is no longer current; no request retried")
	}
	started, err := api.queue.Start(ctx, job.ID)
	if err != nil {
		return finish("blocked", "original queue scope changed before planning; no request retried")
	}
	job = started
	call, closeWatch := recoveryWatch(ctx, func(c context.Context) error {
		if _, err := api.scope(c, job.Definition.Control); err != nil {
			return err
		}
		ready, err := api.queue.SourceReady(c, job.Definition)
		if err != nil || !ready {
			return mediaserver.ErrRefreshState
		}
		active, err := api.queue.Active(c, job.ID)
		if err != nil {
			return err
		}
		if !active {
			return mediaserver.ErrRefreshState
		}
		return nil
	})
	defer closeWatch()
	plan, err := client.PlanRefresh(call, job.Definition.Items, job.Definition.Whole)
	if err != nil || plan.ServerID != job.Definition.Control.ServerID {
		return finish("blocked", "readonly server planning or original server identity could not be verified; no scan request sent")
	}
	if err := api.queue.Plan(call, job.ID, plan); err != nil {
		return finish("blocked", "reviewed targets or authority changed before publishing the plan; no scan request sent")
	}
	for i, target := range plan.Targets {
		if _, err := api.scope(call, job.Definition.Control); err != nil {
			return finish("blocked", "authority changed before the next request; prior receipts retained")
		}
		ready, err := api.queue.SourceReady(call, job.Definition)
		if err != nil || !ready {
			return finish("blocked", "original intake/enrichment authority changed before the next request; prior receipts retained")
		}
		serverID, err := client.RefreshIdentity(call)
		if err != nil || serverID != plan.ServerID {
			return finish("blocked", "server identity changed before the next request; prior receipts retained")
		}
		if err := api.queue.Intent(call, job.ID, i); err != nil {
			return finish("blocked", "could not commit the exact request intent; prior receipts retained")
		}
		err = client.SendRefresh(call, target)
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		persistErr := api.queue.ActionResult(save, job.ID, i, err == nil)
		cancel()
		if persistErr != nil {
			return persistErr
		} // Intent remains; never auto-reclaim.
		if err != nil {
			return finish("uncertain", "server acceptance could not be established; do not resend automatically")
		}
	}
	if len(plan.Targets) == 0 {
		return finish("not_needed", "no refresh target required; no scan request sent")
	}
	return finish("accepted", "server accepted all listed requests; scan completion is not proven")
}
func startMediaRefreshWorker(ctx context.Context, api *mediaRefreshAPI) func() {
	if api == nil || api.organizer == nil || !api.organizer.pureGo {
		return func() {}
	}
	api.workers.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer api.workers.Add(-1)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				run, cancel := context.WithTimeout(ctx, 6*time.Minute)
				err := api.tick(run, now)
				cancel()
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, organization.ErrBusy) {
					slog.Warn("native media refresh dispatch interrupted; retained receipts require review")
				}
			}
		}
	}()
	return func() { <-done }
}
