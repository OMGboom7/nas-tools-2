package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

type webhookHTTPFixture struct {
	*subscriptionRunFixture
	mediaCalls, notifyCalls atomic.Int64
	lost                    bool
	beforeMedia             func()
}

func newWebhookHTTPFixture(t *testing.T, server string) *webhookHTTPFixture {
	t.Helper()
	f := &webhookHTTPFixture{subscriptionRunFixture: newSubscriptionRunFixture(t)}
	fields := map[string]any{"media.media_server": server, "security.check_apikey": true, "security.media_server_webhook_allow_ip": map[string]any{"ipv4": "192.0.2.0/24", "ipv6": "2001:db8::/32"}, server + ".host": "https://hook-media.test/prefix", server + ".play_host": "https://play.test/view"}
	if server == "plex" {
		fields[server+".token"] = "private-media-key"
	} else {
		fields[server+".api_key"] = "private-media-key"
	}
	if err := config.NewStore(f.path).Update(fields); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO MESSAGE_CLIENT(ID,NAME,TYPE,CONFIG,SWITCHS,ENABLED,INTERACTIVE) VALUES(88,'media-channel','webhook','{"url":"https://hook-notify.test/endpoint","method":"POST","token":"private-notify-token"}','["mediaserver_message"]',1,0)`); err != nil {
		t.Fatal(err)
	}
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "hook-media.test":
			f.mediaCalls.Add(1)
			if r.Method != "GET" || r.Header.Get("X-Emby-Token")+r.Header.Get("X-Plex-Token") != "private-media-key" || strings.Contains(r.URL.String(), "private-media-key") {
				t.Errorf("wrong configured lookup: %s", r.URL)
			}
			if f.beforeMedia != nil {
				f.beforeMedia()
			}
			if server == "plex" {
				return jsonResponse(r, `<MediaContainer machineIdentifier="hook-machine"/>`), nil
			}
			return jsonResponse(r, `{"Id":"hook-server"}`), nil
		case "hook-notify.test":
			f.notifyCalls.Add(1)
			raw, _ := io.ReadAll(r.Body)
			var data map[string]any
			if json.Unmarshal(raw, &data) != nil || !strings.Contains(text(data["title"]), "开始播放") || !strings.Contains(text(data["text"]), "viewer") || !strings.Contains(text(data["url"]), "https://play.test/view/") {
				t.Errorf("incomplete native notification: %s", raw)
			}
			for _, secret := range []string{"private-media-key", "private-notify-token", "raw-secret", "evil.test"} {
				if strings.Contains(string(raw), secret) {
					t.Errorf("notification leaked callback/config secret: %s", raw)
				}
			}
			var intent int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_NOTIFICATION_DELIVERIES WHERE STATE='sending' AND CHANNEL_ID=88`).Scan(&intent); err != nil || intent != 1 {
				t.Errorf("sending intent missing: %d %v", intent, err)
			}
			if f.lost {
				return nil, errors.New("accepted then private-notify-token response lost")
			}
			return jsonResponse(r, `{}`), nil
		default:
			t.Errorf("media webhook reached unexpected host %s", r.URL.Hostname())
			return nil, errors.New("unexpected test destination")
		}
	})
	monitorRebuild(t, f.subscriptionRunFixture)
	return f
}
func webhookReviewHTTP(t *testing.T, f *webhookHTTPFixture) mediaWebhookStatus {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/media-webhooks", f.token, "")
	var out struct {
		Data mediaWebhookStatus `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return out.Data
}
func webhookAuthorizeHTTP(t *testing.T, f *webhookHTTPFixture) mediaWebhookStatus {
	t.Helper()
	v := webhookReviewHTTP(t, f)
	raw, _ := json.Marshal(map[string]any{"fingerprint": v.Fingerprint, "confirm": true, "confirmNotifications": true, "legacyStopped": true})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/media-webhooks/authorize", f.token, string(raw))
	var out struct {
		Data mediaWebhookStatus `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.Data.Enabled || !out.Data.Current {
		t.Fatal(w.Code, w.Body.String())
	}
	return out.Data
}
func webhookWireBody(server string) string {
	switch server {
	case "emby":
		return `{"Event":"playback.start","Item":{"Id":"item-1","Type":"Movie","Name":"电影","ProductionYear":2026,"RunTimeTicks":100},"PlaybackInfo":{"PositionTicks":25},"User":{"Name":"viewer"},"Session":{"DeviceName":"客厅","Client":"Web"},"ApiKey":"raw-secret","ServerUrl":"https://evil.test"}`
	case "jellyfin":
		return `{"NotificationType":"PlaybackStart","ItemId":"item-1","ItemType":"Movie","Name":"电影","Year":2026,"RunTimeTicks":100,"PlaybackPositionTicks":25,"NotificationUsername":"viewer","DeviceName":"客厅","ClientName":"Web","Token":"raw-secret","ServerUrl":"https://evil.test"}`
	default:
		return `{"event":"media.play","Metadata":{"ratingKey":"item-1","type":"movie","title":"电影","year":2026,"duration":100,"viewOffset":25},"Account":{"title":"viewer","thumb":"https://evil.test/raw-secret"},"Player":{"title":"Web"},"token":"raw-secret"}`
	}
}
func webhookReceiveHTTP(f *webhookHTTPFixture, server, body, identity string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/"+server+"?apikey=native-services-secret", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if identity != "" {
		r.Header.Set("X-NASTool-Event-ID", identity)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func webhookIDHTTP(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		Data struct {
			ID       string `json:"id"`
			Received bool   `json:"received"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || !v.Data.Received || len(v.Data.ID) != 32 {
		t.Fatal(w.Code, w.Body.String())
	}
	return v.Data.ID
}
func webhookReceiptHTTP(t *testing.T, f *webhookHTTPFixture, id string) mediaWebhookPublicReceipt {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/media-webhooks/receipts/"+id, f.token, "")
	var v struct {
		Data mediaWebhookPublicReceipt `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Data.ID != id {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-media-key", "private-notify-token", "raw-secret", "evil.test", "configDigest"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private callback data escaped", w.Body.String())
		}
	}
	return v.Data
}
func TestMediaWebhookAllProvidersReceiveNotifyAndFindOriginalAfterRestart(t *testing.T) {
	for _, server := range []string{"emby", "jellyfin", "plex"} {
		t.Run(server, func(t *testing.T) {
			f := newWebhookHTTPFixture(t, server)
			v := webhookReviewHTTP(t, f)
			if v.Enabled || !v.Ready || f.mediaCalls.Load() != 0 || f.notifyCalls.Load() != 0 {
				t.Fatal(v)
			}
			webhookAuthorizeHTTP(t, f)
			body := webhookWireBody(server)
			id := webhookIDHTTP(t, webhookReceiveHTTP(f, server, body, "original"))
			p := webhookReceiptHTTP(t, f, id)
			if p.Event.Server != server || p.Event.Percentage == nil || *p.Event.Percentage != 25 || len(p.Deliveries) != 1 || p.Deliveries[0].State != "pending" || f.notifyCalls.Load() != 0 {
				t.Fatal(p)
			}
			if duplicate := webhookIDHTTP(t, webhookReceiveHTTP(f, server, body, "original")); duplicate != id {
				t.Fatal("duplicated acceptance", duplicate, id)
			}
			if f.mediaCalls.Load() != 1 {
				t.Fatal("duplicate looked up current server again", f.mediaCalls.Load())
			}
			if err := f.runner.notifications.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			monitorRebuild(t, f.subscriptionRunFixture)
			if err := f.runner.notifications.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			p = webhookReceiptHTTP(t, f, id)
			if f.notifyCalls.Load() != 1 || p.Deliveries[0].State != "sent" {
				t.Fatal(f.notifyCalls.Load(), p)
			}
			var n int
			f.db.QueryRow(`SELECT COUNT(*) FROM GO_MEDIA_WEBHOOK_RECEIPTS`).Scan(&n)
			if n != 1 {
				t.Fatal(n)
			}
		})
	}
}
func TestMediaWebhookUnknownNotificationResponseRetainsIntentAndNeverResends(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	f.lost = true
	webhookAuthorizeHTTP(t, f)
	id := webhookIDHTTP(t, webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "lost"))
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f.subscriptionRunFixture)
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	p := webhookReceiptHTTP(t, f, id)
	if f.notifyCalls.Load() != 1 || p.Deliveries[0].State != "uncertain" {
		t.Fatal(p, f.notifyCalls.Load())
	}
}
func TestMediaWebhookFreshAuthorityIndependentConsentStopAndChangedConfiguration(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	v := webhookReviewHTTP(t, f)
	for _, field := range []string{"confirm", "confirmNotifications", "legacyStopped"} {
		in := map[string]any{"fingerprint": v.Fingerprint, "confirm": true, "confirmNotifications": true, "legacyStopped": true}
		delete(in, field)
		raw, _ := json.Marshal(in)
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/media-webhooks/authorize", f.token, string(raw))
		if w.Code != 409 {
			t.Fatal(field, w.Code, w.Body.String())
		}
	}
	webhookAuthorizeHTTP(t, f)
	id := webhookIDHTTP(t, webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "before-change"))
	if err := config.NewStore(f.path).Update(map[string]any{"emby.host": "https://changed.test"}); err != nil {
		t.Fatal(err)
	}
	if w := webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "after-change"); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	v = webhookReviewHTTP(t, f)
	if v.Current || !v.Enabled {
		t.Fatal(v)
	}
	raw, _ := json.Marshal(map[string]any{"fingerprint": v.Fingerprint, "confirm": true})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/media-webhooks/stop", f.token, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "after-stop"); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if p := webhookReceiptHTTP(t, f, id); p.ID != id {
		t.Fatal(p)
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='mediaserver_message'`).Scan(&n)
	if n != 1 || f.notifyCalls.Load() != 0 {
		t.Fatal(n, f.notifyCalls.Load())
	}
}
func TestMediaWebhookAPIKeyPeerRangesAndForwardedSpoofing(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	if err := config.NewStore(f.path).Update(map[string]any{"security.api_key": "key.with.dots"}); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f.subscriptionRunFixture)
	f.token = loginForTest(t, f.handler, "admin", "password")
	webhookAuthorizeHTTP(t, f)
	for _, tc := range []struct {
		name, remote, key, query string
		status                   int
	}{
		{"missing", "192.0.2.3:80", "", "", 401},
		{"JWT-is-not-API-key", "192.0.2.3:80", f.token, "", 401},
		{"dotted-header-key", "192.0.2.3:80", "Bearer key.with.dots", "", 200},
		{"query-key", "192.0.2.3:80", "", "?apikey=key.with.dots", 200},
		{"v6", "[2001:db8::5]:80", "key.with.dots", "", 200},
		{"mapped-v4", "[::ffff:192.0.2.3]:80", "key.with.dots", "", 200},
		{"spoofed-forwarded", "203.0.113.7:80", "key.with.dots", "", 403},
		{"duplicate-query", "192.0.2.3:80", "", "?apikey=key.with.dots&apikey=other", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/emby"+tc.query, strings.NewReader(webhookWireBody("emby")))
			r.RemoteAddr = tc.remote
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-For", "192.0.2.3")
			if tc.key != "" {
				r.Header.Set("Authorization", tc.key)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}
func TestMediaWebhookPreservesOriginalOptionalAPIKeyRule(t *testing.T) {
	f := newWebhookHTTPFixture(t, "jellyfin")
	if err := config.NewStore(f.path).Update(map[string]any{"security.check_apikey": false}); err != nil {
		t.Fatal(err)
	}
	webhookAuthorizeHTTP(t, f)
	r := httptest.NewRequest("POST", "/jellyfin", strings.NewReader(webhookWireBody("jellyfin")))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	webhookIDHTTP(t, w)
}
func TestMediaWebhookLegacyEmbyFormsGETAndPlexMultipartWithoutFileWrites(t *testing.T) {
	for _, format := range []string{"emby-form", "emby-get", "plex-multipart"} {
		t.Run(format, func(t *testing.T) {
			server := "emby"
			if format == "plex-multipart" {
				server = "plex"
			}
			f := newWebhookHTTPFixture(t, server)
			webhookAuthorizeHTTP(t, f)
			var r *http.Request
			switch format {
			case "emby-form":
				r = httptest.NewRequest("POST", "/emby?apikey=native-services-secret", strings.NewReader(url.Values{"data": {webhookWireBody("emby")}}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			case "emby-get":
				r = httptest.NewRequest("GET", "/emby?"+url.Values{"apikey": {"native-services-secret"}, "Event": {"playback.start"}, "Title": {"Old title"}}.Encode(), nil)
			case "plex-multipart":
				var b bytes.Buffer
				mw := multipart.NewWriter(&b)
				mw.WriteField("payload", webhookWireBody("plex"))
				p, _ := mw.CreateFormFile("thumb", "thumb.jpg")
				p.Write([]byte("synthetic thumbnail"))
				mw.Close()
				r = httptest.NewRequest("POST", "/plex?apikey=native-services-secret", &b)
				r.Header.Set("Content-Type", mw.FormDataContentType())
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			webhookIDHTTP(t, w)
			if r.MultipartForm != nil && len(r.MultipartForm.File) != 0 {
				t.Fatal("multipart callback used temporary-file parsing")
			}
		})
	}
}
func TestMediaWebhookDefaultsStrictRoutesUnsupportedEventsAndMixedModeNeverUsePython(t *testing.T) {
	f := newWebhookHTTPFixture(t, "plex")
	if w := webhookReceiveHTTP(f, "plex", webhookWireBody("plex"), ""); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	webhookAuthorizeHTTP(t, f)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/plex", "", 405}, {"POST", "/plex/unknown", "{}", 404},
		{"POST", "/plex", `{"event":"media.pause","Metadata":{"type":"movie","title":"M"}}`, 200},
		{"POST", "/plex", `{"event":"media.play","Metadata":{"type":"movie","title":"M","live":true}}`, 200},
		{"POST", "/plex", `{"event":"media.play","event":"media.stop"}`, 400},
		{"POST", "/plex", `{} {}`, 400},
	} {
		r := httptest.NewRequest(tc.method, tc.path+"?apikey=native-services-secret", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_MEDIA_WEBHOOK_RECEIPTS'`).Scan(&n)
	if n != 0 {
		t.Fatal("ignored events created accepted receipt", n)
	}
	mixed, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: "http://legacy.test:3000"}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/plex", strings.NewReader(webhookWireBody("plex")))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mixed.ServeHTTP(w, r)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestMediaWebhookConcurrentExplicitIdentityAndRealRepeatedPlayback(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	webhookAuthorizeHTTP(t, f)
	var wg sync.WaitGroup
	type result struct {
		status int
		body   string
	}
	out := make(chan result, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "same")
			out <- result{w.Code, w.Body.String()}
		}()
	}
	wg.Wait()
	close(out)
	for v := range out {
		if v.status != 200 {
			t.Fatal(v)
		}
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_MEDIA_WEBHOOK_RECEIPTS`).Scan(&n)
	if n != 1 {
		t.Fatal("concurrent identity not unique", n)
	}
	for i := 0; i < 2; i++ {
		webhookIDHTTP(t, webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), ""))
	}
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_MEDIA_WEBHOOK_RECEIPTS`).Scan(&n)
	if n != 3 {
		t.Fatal("distinct playback collapsed", n)
	}
	changed := strings.Replace(webhookWireBody("emby"), "电影", "Changed", 1)
	if w := webhookReceiveHTTP(f, "emby", changed, "same"); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestMediaWebhookConfigurationChangeDuringLookupAndStaleAuthorizationCannotCommit(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	v := webhookAuthorizeHTTP(t, f)
	f.beforeMedia = func() {
		if err := config.NewStore(f.path).Update(map[string]any{"emby.api_key": "changed-key"}); err != nil {
			t.Error(err)
		}
	}
	if w := webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "race"); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, _ := json.Marshal(map[string]any{"fingerprint": v.Fingerprint, "confirm": true, "confirmNotifications": true, "legacyStopped": true})
	if w := nativeJSONRequest(f.handler, "POST", "/api/v1/media-webhooks/authorize", f.token, string(raw)); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_MEDIA_WEBHOOK_RECEIPTS'`).Scan(&n)
	if n != 0 || f.notifyCalls.Load() != 0 {
		t.Fatal(n, f.notifyCalls.Load())
	}
}
func TestMediaWebhookControlRejectsUnknownFieldsAndReadonlyStableLookupDoesNotDispatch(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	for _, body := range []string{`{"confirm":true,"unknown":true}`, `{} {}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/media-webhooks/authorize", f.token, body)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	webhookAuthorizeHTTP(t, f)
	id := webhookIDHTTP(t, webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "stable"))
	for i := 0; i < 3; i++ {
		webhookReceiptHTTP(t, f, id)
		w := nativeJSONRequest(f.handler, "GET", "/api/v1/media-webhooks/receipts?page=1", f.token, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if f.notifyCalls.Load() != 0 || f.mediaCalls.Load() != 1 {
		t.Fatal(f.notifyCalls.Load(), f.mediaCalls.Load())
	}
	for _, path := range []string{"/api/v1/media-webhooks/receipts?page=0", "/api/v1/media-webhooks/receipts?page=10001"} {
		w := nativeJSONRequest(f.handler, "GET", path, f.token, "")
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if p := webhookReceiptHTTP(t, f, id); p.Event.Validate() != nil {
		t.Fatal(p)
	}
	if _, err := mediaserver.WebhookPage("1.5"); err == nil {
		t.Fatal("fractional page")
	}
}

func TestMediaWebhookExactLargeChannelIDsAndFrozenRecipients(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	if _, err := f.db.Exec(`INSERT INTO MESSAGE_CLIENT(ID,NAME,TYPE,CONFIG,SWITCHS,ENABLED,INTERACTIVE) VALUES(9007199254740993,'large-channel','webhook','{}','["mediaserver_message"]',1,0)`); err != nil {
		t.Fatal(err)
	}
	webhookAuthorizeHTTP(t, f)
	id := webhookIDHTTP(t, webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "large"))
	if _, err := f.db.Exec(`INSERT INTO MESSAGE_CLIENT(ID,NAME,TYPE,CONFIG,SWITCHS,ENABLED,INTERACTIVE) VALUES(89,'late','webhook','{}','["mediaserver_message"]',1,0)`); err != nil {
		t.Fatal(err)
	}
	p := webhookReceiptHTTP(t, f, id)
	if len(p.Deliveries) != 2 || p.Deliveries[1].ChannelID != "9007199254740993" {
		t.Fatal(p)
	}
	if _, err := f.db.Exec(`UPDATE MESSAGE_CLIENT SET ENABLED=0 WHERE ID=88`); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.notifyCalls.Load() != 0 {
		t.Fatal("changed or invalid channel configuration sent")
	}
	p = webhookReceiptHTTP(t, f, id)
	if p.Deliveries[0].State != "blocked" {
		t.Fatal(p)
	}
}
func TestMediaWebhookInvalidOriginalSafetyPolicyCannotBecomeUnrestricted(t *testing.T) {
	for _, bad := range []any{"192.0.2.0/24", map[string]any{"ipv4": []any{}}, map[string]any{"ipv4": "bad-range"}, map[string]any{"ipv4": "2001:db8::/32"}, map[string]any{"ipv4": "192.0.2.7/24"}, map[string]any{"ipv6": "2001:db8::7/64"}} {
		f := newWebhookHTTPFixture(t, "emby")
		if err := config.NewStore(f.path).Update(map[string]any{"security.media_server_webhook_allow_ip": bad}); err != nil {
			t.Fatal(err)
		}
		v := webhookReviewHTTP(t, f)
		if v.Ready {
			t.Fatal("invalid original policy became unrestricted", bad, v)
		}
	}
}
func TestMediaWebhookStoppedReceiverKeepsOldReceiptWithUnavailableServer(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	webhookAuthorizeHTTP(t, f)
	id := webhookIDHTTP(t, webhookReceiveHTTP(f, "emby", webhookWireBody("emby"), "old"))
	if err := config.NewStore(f.path).Update(map[string]any{"emby.host": "javascript:invalid"}); err != nil {
		t.Fatal(err)
	}
	v := webhookReviewHTTP(t, f)
	if v.Ready || v.Current || !v.Enabled {
		t.Fatal(v)
	}
	raw, _ := json.Marshal(map[string]any{"fingerprint": v.Fingerprint, "confirm": true})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/media-webhooks/stop", f.token, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	webhookReceiptHTTP(t, f, id)
}
func TestMediaWebhookNestedGETDuplicatesAndMultipartDuplicatePayloadAreRejected(t *testing.T) {
	f := newWebhookHTTPFixture(t, "emby")
	webhookAuthorizeHTTP(t, f)
	q := url.Values{"apikey": {"native-services-secret"}, "Event": {"playback.start"}, "Item": {`{"Name":"A","Name":"B","Type":"Movie"}`}}
	r := httptest.NewRequest("GET", "/emby?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("data", webhookWireBody("emby"))
	mw.WriteField("data", webhookWireBody("emby"))
	mw.Close()
	r = httptest.NewRequest("POST", "/emby?apikey=native-services-secret", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if f.mediaCalls.Load() != 0 || f.notifyCalls.Load() != 0 {
		t.Fatal("rejected callback had effects")
	}
}
