package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func signinRemoteFixture(t *testing.T, webhook bool) (*loginFixture, *siteSigninAPI, sitesignin.Control) {
	t.Helper()
	f, api := newSigninQuietFixture(t)
	if e := f.api.config.Update(map[string]any{"app.domain": "https://callback.fixture.test/base", "laboratory.telegram_domain": "http://telegram.fixture.test"}); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(map[string]any{"token": "12345:fixture-token", "chat_id": "999", "admin_ids": "42", "webhook": webhook})
	if _, e := api.channels.Save(t.Context(), notificationconfig.Channel{Name: "Fixture Telegram", Type: "telegram", Config: string(raw), Switches: `[]`, Enabled: 1, Interactive: 1}); e != nil {
		t.Fatal(e)
	}
	r := f.request(t, "POST", "/api/v1/site-signin/review", map[string]any{"mode": "activate", "options": map[string]bool{"remote": true}})
	var p struct{ Data siteSigninReviewView }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	r = f.request(t, "POST", "/api/v1/site-signin/activate", map[string]any{"id": p.Data.ID, "fingerprint": p.Data.Fingerprint, "confirm": true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	c, e := api.store.Control(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	return f, api, c
}

func TestSiteSigninTelegramEstablishesOriginalWebhookOrPollingMode(t *testing.T) {
	for _, webhook := range []bool{false, true} {
		t.Run(map[bool]string{false: "remove-old-webhook-before-poll", true: "register-original-callback"}[webhook], func(t *testing.T) {
			f, api, c := signinRemoteFixture(t, webhook)
			current := "https://old.fixture.test/telegram"
			deletes, sets, polls := 0, 0, 0
			f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "telegram.fixture.test" {
					t.Fatalf("unexpected endpoint %s", r.URL)
				}
				method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				switch method {
				case "getWebhookInfo":
					raw, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"url": current, "pending_update_count": 1}})
					return jsonResponse(r, string(raw)), nil
				case "deleteWebhook":
					deletes++
					if webhook {
						t.Error("registering callback need not drop the original pending input")
					}
					if r.URL.Query().Get("drop_pending_updates") == "true" {
						t.Fatal("pending messages discarded")
					}
					current = ""
					return jsonResponse(r, `{"ok":true,"result":true}`), nil
				case "setWebhook":
					sets++
					if e := r.ParseForm(); e != nil {
						t.Fatal(e)
					}
					want := "https://callback.fixture.test/base/telegram?apikey=native-site-login-fixture"
					if r.Form.Get("url") != want || r.Form.Get("drop_pending_updates") == "true" {
						t.Fatal("original callback/pending input changed", r.Form)
					}
					var updates []string
					if json.Unmarshal([]byte(r.Form.Get("allowed_updates")), &updates) != nil || len(updates) != 1 || updates[0] != "message" {
						t.Fatal("wrong original update selection", r.Form)
					}
					current = want
					return jsonResponse(r, `{"ok":true,"result":true}`), nil
				case "getUpdates":
					polls++
					if current != "" {
						return &http.Response{StatusCode: 409, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":409}`))}, nil
					}
					return jsonResponse(r, `{"ok":true,"result":[]}`), nil
				default:
					t.Fatalf("unexpected Telegram operation %s", method)
					return nil, io.EOF
				}
			})
			for i := 0; i < 3; i++ {
				if e := api.pollTelegram(t.Context(), c, time.Now().Add(time.Duration(i*6)*time.Second)); e != nil {
					t.Fatal(e)
				}
			}
			if webhook {
				if sets != 1 || polls != 0 || !strings.HasPrefix(current, "https://callback.fixture.test/") {
					t.Fatalf("webhook not established: sets=%d polls=%d current=%s", sets, polls, current)
				}
			} else if deletes != 1 || polls < 1 {
				t.Fatalf("polling never removed old webhook: deletes=%d polls=%d", deletes, polls)
			}
		})
	}
}

func TestSiteSigninTelegramUnknownModeMutationIsReconciledWithoutRepeatingIt(t *testing.T) {
	f, api, c := signinRemoteFixture(t, false)
	current := "https://old.fixture.test/telegram"
	deletes, polls := 0, 0
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			raw, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"url": current}})
			return jsonResponse(r, string(raw)), nil
		case strings.HasSuffix(r.URL.Path, "/deleteWebhook"):
			deletes++
			current = ""
			return nil, io.ErrUnexpectedEOF
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			polls++
			return jsonResponse(r, `{"ok":true,"result":[]}`), nil
		default:
			t.Fatalf("unexpected operation %s", r.URL)
			return nil, io.EOF
		}
	})
	for i := 0; i < 3; i++ {
		if e := api.pollTelegram(t.Context(), c, time.Now().Add(time.Duration(i*6)*time.Second)); e != nil {
			t.Fatal(e)
		}
	}
	if deletes != 1 || polls < 1 {
		t.Fatalf("unconfirmed mode mutation was lost or repeated: deletes=%d polls=%d", deletes, polls)
	}
}

func TestSiteSigninTelegramPollKeepsDurableOffsetAndSingleOriginalSenderReceipt(t *testing.T) {
	f, api, c := signinRemoteFixture(t, false)
	message := map[string]any{"update_id": 70, "message": map[string]any{"message_id": 570, "date": time.Now().Unix(), "text": "/pts", "from": map[string]any{"id": 42, "is_bot": false, "username": "fixture-user"}, "chat": map[string]any{"id": 42, "type": "private"}}}
	polls := 0
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			return jsonResponse(r, `{"ok":true,"result":{"url":""}}`), nil
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			polls++
			want := "0"
			if polls > 1 {
				want = "71"
			}
			if r.URL.Query().Get("offset") != want {
				t.Fatalf("offset was not committed before acknowledgment: %s", r.URL.Query().Get("offset"))
			}
			raw, _ := json.Marshal(map[string]any{"ok": true, "result": []any{message}})
			return jsonResponse(r, string(raw)), nil
		default:
			t.Fatalf("unexpected request %s", r.URL)
			return nil, io.EOF
		}
	})
	for i := 0; i < 2; i++ {
		if e := api.pollTelegram(t.Context(), c, time.Now().Add(time.Duration(i*6)*time.Second)); e != nil {
			t.Fatal(e)
		}
	}
	signals, e := api.store.Signals(t.Context(), true)
	if e != nil || len(signals) != 1 {
		t.Fatalf("duplicate or missing command: %+v %v", signals, e)
	}
	sources, _, e := api.remoteSources(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	state, e := api.store.Remote(t.Context(), sources[0].digest)
	if e != nil || state.Offset != 71 {
		t.Fatal("durable cursor missing", state, e)
	}
}
