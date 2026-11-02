package httpserver

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSiteSigninSummaryCommitFailureReconcilesWithoutProviderReplay(t *testing.T) {
	f, api := newSigninQuietFixture(t)
	db, e := sql.Open("sqlite", f.path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`CREATE TRIGGER fixture_summary_outage BEFORE UPDATE OF DOCUMENT ON GO_SITE_SIGNIN_BATCHES WHEN json_extract(NEW.DOCUMENT,'$.Notified')=1 BEGIN SELECT RAISE(ABORT,'fixture summary outage'); END`); e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	f.api.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("签到成功"))}, nil
	})
	p := signinTestReview(t, f, "manual")
	r := f.request(t, "POST", "/api/v1/site-signin/start", map[string]any{"id": p.ID, "fingerprint": p.Fingerprint, "confirm": true})
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	api.wait.Wait()
	before := signinTestBatch(t, f, p.ID)
	if before.Notified || len(before.Items) != 1 || before.Items[0].Outcome.State != "signed" || calls.Load() != 1 {
		t.Fatalf("fixture did not interrupt only summary: %+v calls=%d", before, calls.Load())
	}
	if _, e = db.Exec(`DROP TRIGGER fixture_summary_outage`); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e = api.tick(t.Context(), time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	after := signinTestBatch(t, f, p.ID)
	if !after.Notified || after.Summary == "" || calls.Load() != 1 || after.Items[0].ID != before.Items[0].ID {
		t.Fatalf("summary did not recover independently: %+v calls=%d", after, calls.Load())
	}
}

func TestSiteSigninSlackReplyKeepsOriginalUserIDWithoutExtraScopes(t *testing.T) {
	posts, opens := 0, 0
	service := notificationService{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "slack.com" {
			t.Fatalf("unexpected endpoint %s", r.URL)
		}
		switch r.URL.Path {
		case "/api/conversations.open":
			opens++
			return jsonResponse(r, `{"ok":false,"error":"missing_scope","needed":"im:write"}`), nil
		case "/api/chat.postMessage":
			posts++
			var payload struct {
				Channel string `json:"channel"`
			}
			if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Channel != "U123FIXTURE" {
				t.Fatal("reply did not address original user", payload.Channel)
			}
			return jsonResponse(r, `{"ok":true,"channel":"D123FIXTURE","ts":"1760000000.1"}`), nil
		default:
			t.Fatalf("unexpected Slack request %s", r.URL)
			return nil, io.EOF
		}
	})}}
	sent, e := service.sendNativeReply(t.Context(), "slack", map[string]any{"bot_token": "xoxb-fixture", "channel": "original-general"}, "U123FIXTURE", "fixture title", "fixture text")
	if e != nil || !sent || opens != 0 || posts != 1 {
		t.Fatalf("original reply unexpectedly needs im:write: sent=%v opens=%d posts=%d err=%v", sent, opens, posts, e)
	}
}
