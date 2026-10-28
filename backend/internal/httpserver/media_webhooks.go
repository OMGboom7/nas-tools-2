package httpserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

type mediaWebhookAPI struct {
	connection mediaSyncAPI
	store      *mediaserver.WebhookStore
	events     *bus.Store
}
type mediaWebhookStatus struct {
	Server         string `json:"server"`
	Endpoint       string `json:"endpoint"`
	Revision       string `json:"revision"`
	Enabled        bool   `json:"enabled"`
	Current        bool   `json:"current"`
	Ready          bool   `json:"ready"`
	Actor          string `json:"actor"`
	Changed        string `json:"changed"`
	APIKeyRequired bool   `json:"apiKeyRequired"`
	IPv4           string `json:"ipv4"`
	IPv6           string `json:"ipv6"`
	Fingerprint    string `json:"fingerprint"`
	Reason         string `json:"reason"`
}
type mediaWebhookScope struct {
	server, scope, playHost, apiKey, ipv4, ipv6 string
	checkKey                                    bool
	client                                      *mediaserver.Client
}

func webhookPeerRanges(raw string, ipv4 bool) ([]netip.Prefix, error) {
	if len(raw) > 16000 {
		return nil, mediaserver.ErrWebhook
	}
	out := []netip.Prefix{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 256 {
		return nil, mediaserver.ErrWebhook
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		p, err := netip.ParsePrefix(part)
		if err != nil {
			addr, e := netip.ParseAddr(part)
			if e != nil {
				return nil, mediaserver.ErrWebhook
			}
			p = netip.PrefixFrom(addr, addr.BitLen())
		}
		if p.Addr().Is4() != ipv4 || p.Addr().Is4In6() || p.Addr().Zone() != "" || p != p.Masked() {
			return nil, mediaserver.ErrWebhook
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
func webhookPeerAllowed(remote, v4, v6 string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	raw := v6
	if ip.Is4() {
		raw = v4
	}
	ranges, err := webhookPeerRanges(raw, ip.Is4())
	if err != nil {
		return false
	}
	if len(ranges) == 0 {
		return true
	}
	for _, p := range ranges {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
func (api *mediaWebhookAPI) scope(actor string) (mediaWebhookScope, error) {
	s := mediaWebhookScope{}
	snapshot, err := api.connection.config.Snapshot()
	if err != nil {
		return s, err
	}
	app, security := objectValue(snapshot["app"]), objectValue(snapshot["security"])
	s.server = strings.ToLower(strings.TrimSpace(text(objectValue(snapshot["media"])["media_server"])))
	if s.server == "" {
		s.server = "emby"
	}
	settings := objectValue(snapshot[s.server])
	allow := objectValue(security["media_server_webhook_allow_ip"])
	s.apiKey, s.ipv4, s.ipv6 = text(security["api_key"]), text(allow["ipv4"]), text(allow["ipv6"])
	s.checkKey = truthy(security["check_apikey"])
	s.scope = bus.Hash([]any{actor, app["login_user"], app["login_password"], s.server, settings, security["api_key"], security["check_apikey"], security["media_server_webhook_allow_ip"]})
	if v := security["media_server_webhook_allow_ip"]; v != nil {
		if _, ok := v.(map[string]any); !ok {
			return s, mediaserver.ErrWebhookState
		}
	}
	for _, family := range []string{"ipv4", "ipv6"} {
		if v := allow[family]; v != nil {
			if _, ok := v.(string); !ok {
				return s, mediaserver.ErrWebhookState
			}
		}
	}
	if v := security["check_apikey"]; v != nil {
		if _, ok := v.(bool); !ok {
			return s, mediaserver.ErrWebhookState
		}
	}
	if actor == "" || text(app["login_user"]) != actor || !api.connection.auth.service.IsAdministrator(actor) {
		return s, mediaserver.ErrWebhookState
	}
	if s.server != "emby" && s.server != "jellyfin" && s.server != "plex" {
		return s, mediaserver.ErrWebhookState
	}
	if _, err := webhookPeerRanges(s.ipv4, true); err != nil {
		return s, err
	}
	if _, err := webhookPeerRanges(s.ipv6, false); err != nil {
		return s, err
	}
	if s.checkKey && s.apiKey == "" {
		return s, mediaserver.ErrWebhookState
	}
	credential := text(settings["api_key"])
	if s.server == "plex" {
		credential = text(settings["token"])
	}
	s.client, err = mediaserver.New(s.server, text(settings["host"]), credential, api.connection.transport)
	if err != nil && !(s.server == "plex" && text(settings["username"]) != "" && text(settings["password"]) != "" && text(settings["servername"]) != "") {
		return s, err
	}
	// Receiving a Plex callback never initiates a plex.tv account login.
	s.playHost = text(settings["play_host"])
	return s, nil
}
func (api *mediaWebhookAPI) status(ctx context.Context, actor string) (mediaWebhookStatus, mediaserver.WebhookControl, mediaWebhookScope, error) {
	v := mediaWebhookStatus{}
	c, err := api.store.Control(ctx)
	if err != nil {
		return v, c, mediaWebhookScope{}, err
	}
	s, scopeErr := api.scope(actor)
	v.Server, v.Endpoint, v.Ready = s.server, "/"+s.server, scopeErr == nil
	if s.server != "emby" && s.server != "jellyfin" && s.server != "plex" {
		v.Server, v.Endpoint = c.Server, ""
		if c.Server != "" {
			v.Endpoint = "/" + c.Server
		}
	}
	v.Revision, v.Enabled, v.Actor, v.Changed = c.Revision, c.Enabled, c.Actor, c.Changed
	v.APIKeyRequired, v.IPv4, v.IPv6 = s.checkKey, s.ipv4, s.ipv6
	v.Fingerprint = bus.Hash([]any{c, s.scope, actor})
	if c.Enabled {
		old, err := api.scope(c.Actor)
		v.Current = err == nil && old.scope == c.Scope && old.server == c.Server
	}
	if scopeErr != nil {
		v.Reason = "原媒体服务器或回调安全配置不可用，请先核对配置"
	} else if c.Enabled && !v.Current {
		v.Reason = "原配置或管理员授权已变化，旧接收器不会接管新范围"
	}
	return v, c, s, nil
}
func (api *mediaWebhookAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.store == nil || api.events == nil {
		writeAPIError(w, 503, 503, "media webhook receipts unavailable")
		return "", false
	}
	if api.connection.auth == nil || api.connection.config == nil {
		writeAPIError(w, 503, 503, "media webhook authority unavailable")
		return "", false
	}
	claims, err := api.connection.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "current login required")
		return "", false
	}
	if !api.connection.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator authority required")
		return "", false
	}
	if !api.connection.pureGo {
		writeAPIError(w, 501, 501, "media webhook control requires disabled legacy backend")
		return "", false
	}
	return claims.Username, true
}

type mediaWebhookPublicReceipt struct {
	mediaserver.WebhookReceipt
	Deliveries []mediaWebhookDelivery `json:"deliveries"`
}
type mediaWebhookDelivery struct {
	ID          string `json:"id"`
	EventID     string `json:"eventId"`
	ChannelID   string `json:"channelId"`
	ChannelName string `json:"channelName"`
	ChannelType string `json:"channelType"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	Actor       string `json:"actor,omitempty"`
	Updated     string `json:"updated"`
}

func (api *mediaWebhookAPI) receipt(ctx context.Context, p mediaserver.WebhookReceipt) (mediaWebhookPublicReceipt, error) {
	out := mediaWebhookPublicReceipt{WebhookReceipt: p, Deliveries: []mediaWebhookDelivery{}}
	e, err := api.events.Get(ctx, p.EventID)
	title, body := p.Event.Message(p.Received)
	if err != nil || e.Kind != "mediaserver_message" || e.Reference != "media-webhook:"+p.ID || e.Title != bus.Bounded(title, 600) || e.Text != bus.Bounded(body, 10000) {
		return out, mediaserver.ErrWebhookState
	}
	var data mediaserver.WebhookEvent
	if json.Unmarshal(e.Data, &data) != nil || bus.Hash(data) != bus.Hash(p.Event) {
		return out, mediaserver.ErrWebhookState
	}
	for _, d := range e.Deliveries {
		out.Deliveries = append(out.Deliveries, mediaWebhookDelivery{d.ID, d.EventID, strconv.FormatInt(d.ChannelID, 10), d.ChannelName, d.ChannelType, d.State, d.Reason, d.Actor, d.Updated})
	}
	return out, nil
}
func (api *mediaWebhookAPI) manage(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	base := "/api/v1/media-webhooks"
	switch {
	case r.URL.Path == base && r.Method == http.MethodGet:
		v, _, _, err := api.status(r.Context(), actor)
		if err != nil {
			writeAPIError(w, 503, 503, "media webhook review unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	case r.URL.Path == base+"/receipts" && r.Method == http.MethodGet:
		page, err := mediaserver.WebhookPage(r.URL.Query().Get("page"))
		if err != nil {
			writeAPIError(w, 400, 400, "invalid receipt page")
			return
		}
		rows, err := api.store.List(r.Context(), page)
		out := []mediaWebhookPublicReceipt{}
		if err == nil {
			for _, p := range rows {
				v, e := api.receipt(r.Context(), p)
				if e != nil {
					err = e
					break
				}
				out = append(out, v)
			}
		}
		if err != nil {
			writeAPIError(w, 503, 503, "complete webhook receipts could not be verified")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": out})
		return
	case strings.HasPrefix(r.URL.Path, base+"/receipts/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, base+"/receipts/")
		p, err := api.store.Get(r.Context(), id)
		if errors.Is(err, mediaserver.ErrWebhookMissing) {
			writeAPIError(w, 404, 404, "webhook receipt not found")
			return
		}
		v, e := api.receipt(r.Context(), p)
		if err != nil || e != nil {
			writeAPIError(w, 503, 503, "original webhook receipt could not be verified")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	case (r.URL.Path == base+"/authorize" || r.URL.Path == base+"/stop") && r.Method == http.MethodPost:
		var in struct {
			Fingerprint          string `json:"fingerprint"`
			Confirm              bool   `json:"confirm"`
			ConfirmNotifications bool   `json:"confirmNotifications"`
			LegacyStopped        bool   `json:"legacyStopped"`
		}
		if !decodeOrganizationRequest(w, r, &in, "invalid webhook receiver action") {
			return
		}
		v, c, s, err := api.status(r.Context(), actor)
		enable := r.URL.Path == base+"/authorize"
		if err != nil || in.Fingerprint != v.Fingerprint || !in.Confirm || enable && (!v.Ready || !in.ConfirmNotifications || !in.LegacyStopped) {
			writeAPIError(w, 409, 409, "fresh original settings review and independent receiver/notification consent required")
			return
		}
		nonce, err := refreshNonce()
		if err != nil {
			writeAPIError(w, 503, 503, "receiver revision unavailable")
			return
		}
		server := s.server
		if server != "emby" && server != "jellyfin" && server != "plex" {
			server = c.Server
		}
		next := mediaserver.WebhookControl{Revision: nonce, Enabled: enable, Actor: actor, Server: server, Scope: s.scope, Changed: time.Now().UTC().Format(time.RFC3339Nano)}
		err = api.store.SetControl(r.Context(), c, next, func() error {
			current, e := api.scope(actor)
			if current.scope != s.scope || enable && e != nil {
				return mediaserver.ErrWebhookState
			}
			return nil
		})
		if err != nil {
			writeAPIError(w, 409, 409, "receiver scope changed; query current settings without retrying")
			return
		}
		out, _, _, err := api.status(r.Context(), actor)
		if err != nil {
			writeAPIError(w, 503, 503, "receiver action saved; query original settings without retrying")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": out})
		return
	}
	writeAPIError(w, 404, 404, "unknown native media webhook operation")
}

func mediaWebhookPath(path string) bool {
	return path == "/emby" || path == "/jellyfin" || path == "/plex"
}
func webhookRequestBody(w http.ResponseWriter, r *http.Request, server string) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	if r.Method == http.MethodGet {
		if len(r.URL.RawQuery) > mediaserver.WebhookBodyLimit {
			return nil, mediaserver.ErrWebhook
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return nil, mediaserver.ErrWebhook
		}
		m := map[string]any{}
		for k, v := range values {
			if k == "apikey" {
				continue
			}
			if len(v) != 1 {
				return nil, mediaserver.ErrWebhook
			}
			if k == "Item" || k == "User" || k == "Session" || k == "PlaybackInfo" || k == "TranscodingInfo" {
				// Keep raw object keys until the duplicate-key parser sees them.
				m[k] = json.RawMessage(v[0])
			} else {
				m[k] = v[0]
			}
		}
		return json.Marshal(m)
	}
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, mediaserver.ErrWebhook
	}
	if kind == "application/json" {
		return io.ReadAll(io.LimitReader(r.Body, mediaserver.WebhookBodyLimit+1))
	}
	field := "data"
	if server == "plex" {
		field = "payload"
	}
	if kind == "application/x-www-form-urlencoded" {
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		if len(r.PostForm[field]) != 1 {
			return nil, mediaserver.ErrWebhook
		}
		return []byte(r.PostForm.Get(field)), nil
	}
	if kind == "multipart/form-data" {
		reader, err := r.MultipartReader()
		if err != nil {
			return nil, err
		}
		var raw []byte
		for n := 0; ; n++ {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil || n >= 12 {
				return nil, mediaserver.ErrWebhook
			}
			if part.FormName() == field {
				if raw != nil || part.FileName() != "" {
					part.Close()
					return nil, mediaserver.ErrWebhook
				}
				raw, err = io.ReadAll(io.LimitReader(part, mediaserver.WebhookBodyLimit+1))
			} else {
				_, err = io.Copy(io.Discard, part)
			}
			part.Close()
			if err != nil {
				return nil, err
			}
		}
		if raw == nil {
			return nil, mediaserver.ErrWebhook
		}
		return raw, nil
	}
	return nil, mediaserver.ErrWebhook
}
func webhookRequestKey(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	match := func(value string) bool {
		if value == "" || len(value) > 4096 {
			return false
		}
		a, b := sha256.Sum256([]byte(expected)), sha256.Sum256([]byte(value))
		return subtle.ConstantTimeCompare(a[:], b[:]) == 1
	}
	header := r.Header.Values("Authorization")
	if len(header) == 1 {
		fields := strings.Fields(header[0])
		if len(fields) == 1 && match(fields[0]) || len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && match(fields[1]) {
			return true
		}
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	return err == nil && len(q["apikey"]) == 1 && match(q.Get("apikey"))
}
func (api *mediaWebhookAPI) receive(w http.ResponseWriter, r *http.Request) {
	server := strings.TrimPrefix(r.URL.Path, "/")
	if !mediaWebhookPath(r.URL.Path) {
		writeAPIError(w, 404, 404, "unknown media webhook")
		return
	}
	if r.Method != http.MethodPost && !(server == "emby" && r.Method == http.MethodGet) {
		w.Header().Set("Allow", "POST")
		if server == "emby" {
			w.Header().Set("Allow", "GET, POST")
		}
		writeAPIError(w, 405, 405, "unsupported webhook method")
		return
	}
	if api == nil || !api.connection.pureGo {
		writeAPIError(w, 501, 501, "native media webhook receiving requires disabled legacy backend")
		return
	}
	if api.store == nil {
		writeAPIError(w, 503, 503, "media webhook receiver unavailable")
		return
	}
	c, err := api.store.Control(r.Context())
	if err != nil || !c.Enabled {
		writeAPIError(w, 403, 403, "native media webhook receiver is not authorized")
		return
	}
	s, err := api.scope(c.Actor)
	if err != nil || s.scope != c.Scope || s.server != server || s.server != c.Server {
		writeAPIError(w, 409, 409, "original media webhook scope changed")
		return
	}
	if !webhookPeerAllowed(r.RemoteAddr, s.ipv4, s.ipv6) {
		writeAPIError(w, 403, 403, "media webhook peer is outside the configured range")
		return
	}
	if s.checkKey && !webhookRequestKey(r, s.apiKey) {
		writeAPIError(w, 401, 401, "media webhook API key required")
		return
	}
	raw, err := webhookRequestBody(w, r, server)
	if err != nil || len(raw) > mediaserver.WebhookBodyLimit {
		writeAPIError(w, 400, 400, "invalid or oversized media webhook body")
		return
	}
	e, identity, err := mediaserver.ParseWebhook(server, raw)
	if errors.Is(err, mediaserver.ErrWebhookIgnored) {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "ignored": true})
		return
	}
	if err != nil {
		writeAPIError(w, 400, 400, "media webhook notification fields are invalid")
		return
	}
	keys := r.Header.Values("X-NASTool-Event-ID")
	if len(keys) > 1 {
		writeAPIError(w, 400, 400, "ambiguous webhook event identity")
		return
	}
	if len(keys) == 1 && keys[0] != "" {
		identity = "header:" + keys[0]
	} else if identity != "" {
		identity = "payload:" + identity
	}
	nonce, err := refreshNonce()
	if err != nil {
		writeAPIError(w, 503, 503, "webhook receipt unavailable")
		return
	}
	id, err := mediaserver.WebhookReceiptID(c.Revision, server, identity, nonce)
	if err != nil {
		writeAPIError(w, 400, 400, "invalid webhook event identity")
		return
	}
	digest, err := mediaserver.WebhookPayloadDigest(server, raw)
	if err != nil {
		writeAPIError(w, 400, 400, "invalid webhook payload")
		return
	}
	// Enrichment only contacts the configured server with its stored credential;
	// callback ServerUrl, thumb, avatar and playback URLs are never followed.
	if _, err := api.store.Get(r.Context(), id); errors.Is(err, mediaserver.ErrWebhookMissing) && s.client != nil && e.ItemID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		link, err := s.client.PlaybackURL(ctx, e.ItemID, s.playHost)
		cancel()
		if err == nil {
			e.Link = link
		}
	}
	p := mediaserver.WebhookReceipt{ID: id, Received: time.Now().UTC().Format(time.RFC3339Nano), Event: e}
	p, err = api.store.Receive(r.Context(), c, p, digest, func() error {
		current, err := api.scope(c.Actor)
		if err != nil || current.scope != c.Scope || current.server != c.Server {
			return mediaserver.ErrWebhookState
		}
		return nil
	})
	if err != nil {
		writeAPIError(w, 409, 409, "webhook acceptance was not confirmed; query original receipts without replaying notification")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"id": p.ID, "eventId": p.EventID, "deduplicated": p.Deduplicated, "received": true}})
}
