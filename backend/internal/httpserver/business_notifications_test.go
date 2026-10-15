package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

func businessChannel(t *testing.T, f *subscriptionRunFixture) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT INTO MESSAGE_CLIENT(ID,NAME,TYPE,CONFIG,SWITCHS,ENABLED,INTERACTIVE) VALUES(77,'business','webhook','{"url":"https://notify.test/private-token","method":"POST","token":"private-auth"}','["transfer_finished","download_start","rss_added","rss_finished"]',1,0)`); err != nil {
		t.Fatal(err)
	}
}
func businessWire(t *testing.T, f *subscriptionRunFixture, lost bool, before func()) *int {
	t.Helper()
	calls := new(int)
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "notify.test" {
			return base.RoundTrip(r)
		}
		*calls++
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		var sending int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_NOTIFICATION_DELIVERIES d JOIN GO_BUSINESS_EVENTS e ON e.ID=d.EVENT_ID WHERE d.STATE='sending' AND d.CHANNEL_ID=77 AND e.TITLE=? AND e.BODY=?`, payload.Title, payload.Text).Scan(&sending); err != nil || sending < 1 {
			t.Fatal("this message's sending intent missing", sending, err)
		}
		if strings.Contains(string(body), "magnet:") || strings.Contains(string(body), "private-auth") {
			t.Fatal("private data in business payload", string(body))
		}
		if before != nil {
			before()
		}
		if lost {
			return nil, errors.New("private-token: accepted then response lost")
		}
		return jsonResponse(r, `{}`), nil
	})
	monitorRebuild(t, f)
	return calls
}
func businessList(t *testing.T, f *subscriptionRunFixture) []bus.Event {
	t.Helper()
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/notifications/events", f.token, "")
	var response struct {
		Data []bus.Event `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-token") || strings.Contains(w.Body.String(), "private-auth") || strings.Contains(w.Body.String(), "configDigest") {
		t.Fatal("receipt leaked private config", w.Body.String())
	}
	return response.Data
}
func TestBusinessNotificationOrganizationAllModesEmitsOnlyWholeSuccessfulGroup(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _, input, plan := organizationLinkFixture(t, mode)
			businessChannel(t, f)
			calls := businessWire(t, f, false, nil)
			job := organizationCreateJob(t, f, input, plan)
			if len(businessList(t, f)) != 0 {
				t.Fatal("draft notified")
			}
			status, completed, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || completed.State != "completed" {
				t.Fatal(status, body)
			}
			events := businessList(t, f)
			if len(events) != 1 || events[0].Kind != "transfer_finished" || len(events[0].Deliveries) != 1 || events[0].Deliveries[0].State != "pending" || *calls != 0 {
				t.Fatal(events, *calls)
			}
			if err := f.runner.notifications.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if *calls != 1 {
				t.Fatal(*calls)
			}
			if err := f.runner.notifications.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if *calls != 1 {
				t.Fatal("duplicate notification", *calls)
			}
			status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 {
				t.Fatal(status, body)
			}
			events = businessList(t, f)
			if len(events) != 1 || events[0].Deliveries[0].State != "sent" {
				t.Fatal(events)
			}
		})
	}
}
func TestBusinessNotificationLostResponseDoesNotRepeatBusinessOrDelivery(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, true, nil)
	if status, body := f.run("MOV", "1"); status != 200 || f.adds != 1 {
		t.Fatal(status, body, f.adds)
	}
	events := businessList(t, f)
	if len(events) != 2 {
		t.Fatal(events)
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || f.adds != 1 {
		t.Fatal(*calls, f.adds)
	}
	monitorRebuild(t, f)
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || f.adds != 1 {
		t.Fatal("replayed", *calls, f.adds)
	}
	for _, e := range businessList(t, f) {
		if e.Deliveries[0].State != "uncertain" {
			t.Fatal(e)
		}
	}
	if status, _ := f.run("MOV", "1"); status != 404 || f.adds != 1 {
		t.Fatal(status, f.adds)
	}
}
func TestBusinessNotificationUncertainSubmissionDoesNotEmitSuccess(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, false, nil)
	f.addError = true
	if status, body := f.run("MOV", "1"); status != 502 {
		t.Fatal(status, body)
	}
	if len(businessList(t, f)) != 0 {
		t.Fatal("uncertain add notified as accepted")
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 || f.adds != 1 {
		t.Fatal(*calls, f.adds)
	}
}
func TestBusinessNotificationChangedChannelBlocksAndDoesNotDeliverToNewEndpoint(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, false, nil)
	if status, body := f.run("MOV", "1"); status != 200 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`UPDATE MESSAGE_CLIENT SET CONFIG='{"url":"https://other.test","method":"POST"}' WHERE ID=77`); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Fatal(*calls)
	}
	for _, e := range businessList(t, f) {
		if e.Deliveries[0].State != "blocked" {
			t.Fatal(e)
		}
	}
}
func TestBusinessNotificationUnsupportedTemplateBlocksWithoutNetwork(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	if _, err := f.db.Exec(`UPDATE MESSAGE_CLIENT SET CONFIG='{"url":"https://notify.test","method":"POST","json_tpl":"unsupported"}' WHERE ID=77`); err != nil {
		t.Fatal(err)
	}
	calls := businessWire(t, f, false, nil)
	if status, body := f.run("MOV", "1"); status != 200 {
		t.Fatal(status, body)
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Fatal(*calls)
	}
	for _, e := range businessList(t, f) {
		if e.Deliveries[0].State != "blocked" {
			t.Fatal(e)
		}
	}
}
func TestBusinessNotificationSubscriptionAddedUsesInsertTransactionNotEdits(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, false, nil)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	input := subscriptionUpsertRequest{Type: "MOV", Name: "New subscription", Year: "2026"}
	id, err := upsertNativeSubscriptionTx(t.Context(), tx, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	events := businessList(t, f)
	if len(events) != 1 || events[0].Kind != "rss_added" {
		t.Fatal(events)
	}
	tx, err = f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	input.ID = strconv.FormatInt(id, 10)
	if _, err := upsertNativeSubscriptionTx(t.Context(), tx, input, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(businessList(t, f)) != 1 || *calls != 0 {
		t.Fatal("edit resent subscription added")
	}
}
func TestBusinessNotificationDismissalSharesWorkerLockAndRetainsOriginalEvent(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, true, nil)
	if status, body := f.run("MOV", "1"); status != 200 {
		t.Fatal(status, body)
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := businessList(t, f)
	id := events[0].Deliveries[0].ID
	path := "/api/v1/notifications/events/" + id + "/dismiss"
	if w := nativeJSONRequest(f.handler, "POST", path, f.token, `{"confirm":false,"reason":"reviewed"}`); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	guard, err := f.runner.organizer.jobs.AcquireJob(t.Context(), businessNotificationGuard)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", path, f.token, `{"confirm":true,"reason":"reviewed"}`)
	guard.Close()
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = nativeJSONRequest(f.handler, "POST", path, f.token, `{"confirm":true,"reason":"reviewed"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || len(businessList(t, f)) != 2 {
		t.Fatal(*calls)
	}
}
func TestBusinessNotificationSendingAfterRestartIsNotReplayed(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, false, nil)
	if status, body := f.run("MOV", "1"); status != 200 {
		t.Fatal(status, body)
	}
	pending, err := f.runner.notifications.queue.Pending(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if _, _, err := f.runner.notifications.queue.Claim(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	monitorRebuild(t, f)
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 0 {
		t.Fatal("sending replayed", *calls)
	}
	for _, e := range businessList(t, f) {
		if e.Deliveries[0].State != "sending" {
			t.Fatal(e)
		}
	}
}
func TestBusinessNotificationPermissionRuntimeAndWorkerCancellation(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, false, nil)
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	mixed, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/notifications/events", "/api/v1/notifications/events/" + strings.Repeat("a", 64) + "/dismiss"} {
		method := "GET"
		if strings.HasSuffix(path, "dismiss") {
			method = "POST"
		}
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, method, path, token, `{}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(w.Code, w.Body.String())
			}
		}
		w := nativeJSONRequest(mixed, method, path, f.token, `{}`)
		if w.Code != 501 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	wait := startBusinessNotificationWorker(ctx, f.runner.notifications)
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
	if *calls != 0 {
		t.Fatal(*calls)
	}
}
func TestBusinessNotificationPartialGroupDoesNotNotifyUntilExplicitRecovery(t *testing.T) {
	f, _, _, job := organizationPublicationFixture(t, "move")
	businessChannel(t, f)
	calls := businessWire(t, f, false, nil)
	if len(businessList(t, f)) != 0 {
		t.Fatal("prepared group notified")
	}
	status, _, body := organizationMoveRequest(t, f, job.ID, "resume-publication")
	if status != 200 {
		t.Fatal(status, body)
	}
	if len(businessList(t, f)) != 0 {
		t.Fatal("partial group notified")
	}
	status, complete, body := organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 || complete.State != "completed" {
		t.Fatal(status, body)
	}
	if len(businessList(t, f)) != 1 || *calls != 0 {
		t.Fatal("whole group event missing")
	}
}

func TestBusinessNotificationReceiptFailureNeverRetriesAcceptedSend(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, false, nil)
	if status, body := f.run("MOV", "1"); status != 200 {
		t.Fatal(status, body)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_sent BEFORE UPDATE OF STATE ON GO_NOTIFICATION_DELIVERIES WHEN NEW.STATE='sent' BEGIN SELECT RAISE(ABORT,'lost local receipt'); END`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := f.runner.notifications.tick(t.Context()); err == nil {
			t.Fatal("receipt failure hidden")
		}
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || f.adds != 1 {
		t.Fatal("accepted send retried", *calls, f.adds)
	}
	for _, e := range businessList(t, f) {
		if e.Deliveries[0].State != "sending" {
			t.Fatal(e)
		}
	}
}

func TestBusinessNotificationOutboxFailureKeepsAcceptedDownloadPendingWithoutResubmit(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	businessWire(t, f, false, nil)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Emit(t.Context(), tx, "rss_added", "seed", "seed", "seed", "seed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_business_event BEFORE INSERT ON GO_BUSINESS_EVENTS WHEN NEW.KIND='download_start' BEGIN SELECT RAISE(ABORT,'outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if status, body := f.run("MOV", "1"); status != 502 || f.adds != 1 {
		t.Fatal(status, body, f.adds)
	}
	var state string
	if err := f.db.QueryRow(`SELECT STATE FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&state); err != nil || state != "pending" {
		t.Fatal(state, err)
	}
	if status, body := f.run("MOV", "1"); status != 409 || f.adds != 1 {
		t.Fatal(status, body, f.adds)
	}
	for _, e := range businessList(t, f) {
		if e.Kind == "download_start" || e.Kind == "rss_finished" {
			t.Fatal("uncommitted success notified", e)
		}
	}
}

func TestBusinessNotificationNoRetargetDuringDispatchAndOtherChannelProgress(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	businessChannel(t, f)
	calls := businessWire(t, f, false, func() {
		if _, err := f.db.Exec(`UPDATE MESSAGE_CLIENT SET CONFIG='{"url":"https://changed.test","method":"POST"}' WHERE ID=77`); err != nil {
			t.Fatal(err)
		}
	})
	if status, body := f.run("MOV", "1"); status != 200 {
		t.Fatal(status, body)
	}
	if err := f.runner.notifications.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if *calls != 1 {
		t.Fatal(*calls)
	}
	states := map[string]int{}
	for _, e := range businessList(t, f) {
		states[e.Deliveries[0].State]++
	}
	if states["sent"] != 1 || states["blocked"] != 1 {
		t.Fatal(states)
	}
}
