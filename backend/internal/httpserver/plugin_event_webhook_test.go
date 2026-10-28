package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

func eventHookDetailHTTP(t *testing.T, f *subscriptionRunFixture) pluginConfigDetail {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/plugins/Webhook", f.token, "")
	var out struct{ Data pluginConfigDetail }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Data.Meta.Fingerprint) != 64 {
		t.Fatal(w.Code, w.Body.String())
	}
	return out.Data
}
func eventHookInstallHTTP(t *testing.T, f *subscriptionRunFixture) {
	t.Helper()
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/Webhook/install", f.token, "{}")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func eventHookSaveHTTP(t *testing.T, f *subscriptionRunFixture, detail pluginConfigDetail, values map[string]any) int {
	t.Helper()
	raw, _ := json.Marshal(pluginConfigRequest{Fingerprint: detail.Meta.Fingerprint, Values: values})
	w := nativeJSONRequest(f.handler, "PUT", "/api/v1/plugins/Webhook", f.token, string(raw))
	return w.Code
}
func eventHookEnableHTTP(t *testing.T, f *subscriptionRunFixture) {
	t.Helper()
	eventHookInstallHTTP(t, f)
	detail := eventHookDetailHTTP(t, f)
	if status := eventHookSaveHTTP(t, f, detail, map[string]any{"webhook_url": "https://user:secret@event-receiver.test/private?token=private-query", "method": "post", "enable": true, "go_confirm": true, "go_payload": true, "go_old_stopped": true}); status != 200 {
		t.Fatal("enable", status)
	}
}
func eventHookRowsHTTP(t *testing.T, f *subscriptionRunFixture) []bus.EventWebhookReceipt {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/event-webhooks/receipts", f.token, "")
	var out struct{ Data []bus.EventWebhookReceipt }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-query", "event-receiver.test", "configDigest", "definition", "fingerprint"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("public receipt leaked subscriber", w.Body.String())
		}
	}
	return out.Data
}
func eventHookPublishHTTP(t *testing.T, f *subscriptionRunFixture, key string) {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := bus.PublishSignal(t.Context(), tx, "subscribe.add", key, "subscription:"+key, map[string]any{"rssid": 1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestEventWebhookPluginLifecycleSeparateConsentsAndLegacyTakeover(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	original := `{"webhook_url":"https://old.test/secret?token=old","method":"get","unknown":{"retain":true}}`
	if err := f.runner.download.system.Set(t.Context(), "plugin.Webhook", original); err != nil {
		t.Fatal(err)
	}
	eventHookInstallHTTP(t, f)
	detail := eventHookDetailHTTP(t, f)
	if detail.Values["enable"] != false || detail.Values["go_confirm"] != false || detail.Values["go_payload"] != false || detail.Values["go_old_stopped"] != false {
		t.Fatal(detail)
	}
	raw, _ := json.Marshal(detail)
	if strings.Contains(string(raw), "old.test") || strings.Contains(string(raw), "secret") {
		t.Fatal("old URL echoed")
	}
	for _, flags := range []map[string]any{
		{"enable": true}, {"enable": true, "go_confirm": true}, {"enable": true, "go_confirm": true, "go_payload": true},
		{"enable": true, "go_confirm": true, "go_old_stopped": true},
		{"enable": true, "go_confirm": true, "go_payload": true, "go_old_stopped": true, "webhook_url": "http://event-receiver.test/plain"},
	} {
		if status := eventHookSaveHTTP(t, f, detail, flags); status != 400 {
			t.Fatal("consent bypass", flags, status)
		}
	}
	if status := eventHookSaveHTTP(t, f, detail, map[string]any{"enable": true, "go_confirm": true, "go_payload": true, "go_old_stopped": true}); status != 200 {
		t.Fatal(status)
	}
	if status := eventHookSaveHTTP(t, f, detail, map[string]any{"enable": false}); status != 409 {
		t.Fatal("stale settings accepted", status)
	}
	saved, err := f.runner.download.system.Get(t.Context(), "plugin.Webhook")
	if err != nil || !strings.Contains(saved, `"unknown":{"retain":true}`) || !strings.Contains(saved, "old.test") {
		t.Fatal(saved, err)
	}
	for _, path := range []string{"/api/v1/plugins/Webhook/page", "/api/v1/event-webhooks", "/api/v1/event-webhooks/receipts"} {
		if w := nativeJSONRequest(f.handler, "GET", path, f.token, ""); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if w := nativeJSONRequest(f.handler, "GET", path, "", ""); w.Code != 401 {
			t.Fatal("anonymous webhook read", path, w.Code)
		}
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"event-viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Code, created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "event-viewer", "strong-password")
	for _, path := range []string{"/api/v1/event-webhooks", "/api/v1/event-webhooks/receipts", "/api/v1/plugins/Webhook"} {
		if w := nativeJSONRequest(f.handler, "GET", path, viewer, ""); w.Code != 403 {
			t.Fatal("non-administrator accessed sender", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/plugin/status", "/api/v1/plugin/page"} {
		if w := performFormRequest(f.handler, path, f.token, url.Values{"id": {"Webhook"}}); w.Code != 200 {
			t.Fatal("legacy route used Python", path, w.Code, w.Body.String())
		}
	}
	// Old clients can preserve/edit the original full config, but cannot
	// silently manufacture the new independent native sender grant.
	w := performFormRequest(f.handler, "/api/v1/plugin/config", f.token, url.Values{"id": {"Webhook"}, "config": {original}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	_, _, active, err := f.runner.eventWebhook.queue.EventWebhookScope(t.Context())
	if err != nil || active {
		t.Fatal("old form enabled sender", active, err)
	}
	w = performFormRequest(f.handler, "/api/v1/plugin/list", f.token, url.Values{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"Webhook"`) || strings.Contains(w.Body.String(), "old.test") {
		t.Fatal(w.Code, w.Body.String())
	}
	// Original URL and unknown fields survive uninstall/reinstall; grant does not.
	if w := nativeJSONRequest(f.handler, "DELETE", "/api/v1/plugins/Webhook", f.token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	eventHookInstallHTTP(t, f)
	detail = eventHookDetailHTTP(t, f)
	if detail.Values["enable"] != false {
		t.Fatal("reinstall restored grant")
	}
	ids, _ := f.runner.download.system.Get(t.Context(), "UserInstalledPlugins")
	if !strings.Contains(ids, "Prowlarr") {
		t.Fatal("unrelated plugin lost", ids)
	}
}
func TestEventWebhookActualSubscriptionProducerAndUncertainDeliveryNeverReplay(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	base := f.transport
	var calls atomic.Int64
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "event-receiver.test" {
			return base.RoundTrip(r)
		}
		calls.Add(1)
		var intent int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_EVENT_WEBHOOK_OUTBOX WHERE STATE='intent'`).Scan(&intent); err != nil || intent < 1 {
			t.Error("original intent missing", intent, err)
		}
		raw, _ := io.ReadAll(r.Body)
		var out struct {
			Type string
			Data map[string]any
		}
		if json.Unmarshal(raw, &out) != nil || out.Data["media_info"] == nil || out.Data["rssid"] == nil {
			t.Error("real subscription payload missing", string(raw))
		}
		stage, _ := out.Data["native_event"].(map[string]any)
		if stage["stage"] != "committed" {
			t.Error("accepted event phase lost", stage)
		}
		return nil, errors.New("receiver accepted then private-query response lost")
	})
	monitorRebuild(t, f)
	eventHookEnableHTTP(t, f)
	if status, body := f.run("MOV", "1"); status != 200 || f.adds != 1 {
		t.Fatal(status, body, f.adds)
	}
	rows := eventHookRowsHTTP(t, f)
	if len(rows) != 2 {
		t.Fatal("download + finished subscriber events", rows)
	}
	kinds := map[string]bool{}
	for _, p := range rows {
		kinds[p.Type] = true
		if p.State != "pending" {
			t.Fatal(p)
		}
	}
	if !kinds["download.add"] || !kinds["subscribe.finished"] {
		t.Fatal(kinds)
	}
	if err := f.runner.eventWebhook.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("lost response delivery count", calls.Load())
	}
	monitorRebuild(t, f)
	for i := 0; i < 3; i++ {
		if err := f.runner.eventWebhook.tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	rows = eventHookRowsHTTP(t, f)
	for _, p := range rows {
		if p.State != "uncertain" || strings.Contains(p.Reason, "private-query") {
			t.Fatal(p)
		}
	}
	if calls.Load() != 2 || f.adds != 1 {
		t.Fatal("restart retried external effect", calls.Load(), f.adds)
	}
	id := rows[0].ID
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/event-webhooks/receipts/"+id+"/dismiss", f.token, `{"confirm":false,"reason":"reviewed"}`)
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/event-webhooks/receipts/"+id+"/dismiss", f.token, `{"confirm":true,"reason":"receiver reviewed; keep original"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"dismissed"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = nativeJSONRequest(f.handler, "GET", "/api/v1/event-webhooks/receipts/"+id, f.token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"dismissed"`) || calls.Load() != 2 {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
}
func TestEventWebhookStopAuthorityChangeAndUninstallBlockOldWork(t *testing.T) {
	for _, change := range []string{"stop", "uninstall", "api-key"} {
		t.Run(change, func(t *testing.T) {
			f := newSubscriptionRunFixture(t)
			eventHookEnableHTTP(t, f)
			eventHookPublishHTTP(t, f, "old")
			switch change {
			case "stop":
				detail := eventHookDetailHTTP(t, f)
				if status := eventHookSaveHTTP(t, f, detail, map[string]any{"enable": false}); status != 200 {
					t.Fatal(status)
				}
			case "uninstall":
				if w := nativeJSONRequest(f.handler, "DELETE", "/api/v1/plugins/Webhook", f.token, ""); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "api-key":
				if err := config.NewStore(f.path).Update(map[string]any{"security.api_key": "new authority"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.runner.eventWebhook.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := eventHookRowsHTTP(t, f)
			if len(rows) != 1 || rows[0].State != "blocked" {
				t.Fatal(rows)
			}
		})
	}
}
func TestEventWebhookRawMediaCallbacksAndNotificationProjectionStayDistinct(t *testing.T) {
	for _, server := range []string{"emby", "jellyfin", "plex"} {
		t.Run(server, func(t *testing.T) {
			f := newWebhookHTTPFixture(t, server)
			base := f.transport
			var calls atomic.Int64
			var payloads []string
			f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "event-receiver.test" {
					return base.RoundTrip(r)
				}
				calls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				payloads = append(payloads, string(raw))
				return jsonResponse(r, `{}`), nil
			})
			monitorRebuild(t, f.subscriptionRunFixture)
			// Receiver authority alone does not authorize the external subscriber.
			webhookAuthorizeHTTP(t, f)
			webhookIDHTTP(t, webhookReceiveHTTP(f, server, webhookWireBody(server), "before-hook"))
			eventHookEnableHTTP(t, f.subscriptionRunFixture)
			webhookIDHTTP(t, webhookReceiveHTTP(f, server, webhookWireBody(server), "before-hook"))
			if rows := eventHookRowsHTTP(t, f.subscriptionRunFixture); len(rows) != 0 {
				t.Fatal("callback history backfilled", rows)
			}
			webhookIDHTTP(t, webhookReceiveHTTP(f, server, webhookWireBody(server), "after-hook"))
			body := map[string]string{"emby": `{"Event":"playback.pause","EventId":"paused-1","Private":"private-raw","Item":{"Id":"one","Type":"Movie"}}`, "jellyfin": `{"NotificationType":"PlaybackPause","EventId":"paused-1","Private":"private-raw","ItemType":"Movie"}`, "plex": `{"event":"media.pause","EventId":"paused-1","Private":"private-raw","Metadata":{"type":"movie","title":"M"}}`}[server]
			for i := 0; i < 2; i++ {
				w := webhookReceiveHTTP(f, server, body, "")
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"ignored":true`) {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			rows := eventHookRowsHTTP(t, f.subscriptionRunFixture)
			if len(rows) != 2 {
				t.Fatal("raw event count", rows)
			}
			var notifications int
			f.db.QueryRow(`SELECT COUNT(*) FROM GO_MEDIA_WEBHOOK_RECEIPTS`).Scan(&notifications)
			if notifications != 2 || f.notifyCalls.Load() != 0 {
				t.Fatal("pause fabricated notification", notifications, f.notifyCalls.Load())
			}
			if err := f.runner.eventWebhook.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 {
				t.Fatal("callback duplicate sent", calls.Load())
			}
			seenPrivate := false
			for _, raw := range payloads {
				var out struct {
					Type string
					Data map[string]any
				}
				if json.Unmarshal([]byte(raw), &out) != nil || out.Type != server+".webhook" {
					t.Fatal(raw)
				}
				if out.Data["Private"] == "private-raw" {
					seenPrivate = true
				}
				if out.Data["native_event"] != nil {
					t.Fatal("raw callback replaced with notification", raw)
				}
			}
			if !seenPrivate {
				t.Fatal("authorized original raw data lost")
			}
		})
	}
}
func TestEventWebhookSaveIgnoredDatabaseWriteAndMixedModeNeverStartSender(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	eventHookInstallHTTP(t, f)
	detail := eventHookDetailHTTP(t, f)
	if _, err := f.db.Exec(`CREATE TRIGGER DROP_EVENT_GRANT BEFORE UPDATE ON SYSTEM_DICT WHEN NEW.KEY='GoEventWebhook' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if status := eventHookSaveHTTP(t, f, detail, map[string]any{"webhook_url": "https://event-receiver.test/", "method": "post", "enable": true, "go_confirm": true, "go_payload": true, "go_old_stopped": true}); status != 409 {
		t.Fatal("ignored grant reported active", status)
	}
	_, _, active, err := f.runner.eventWebhook.queue.EventWebhookScope(t.Context())
	if err != nil || active {
		t.Fatal(active, err)
	}
	mixed, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: "http://legacy.test:3000"}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/event-webhooks", "/api/v1/plugins/Webhook", "/api/v1/event-webhooks/receipts"} {
		w := nativeJSONRequest(mixed, "GET", path, f.token, "")
		if w.Code != 501 {
			t.Fatal("mixed mode used legacy sender", path, w.Code, w.Body.String())
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	wait := startEventWebhookWorker(ctx, f.runner.eventWebhook)
	wait()
	if f.runner.eventWebhook.workers.Load() != 0 {
		t.Fatal("native sender worker survived shutdown")
	}
}
