package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

const reservationEndpoint = "/api/v1/organization/reservation-releases"

func reservationHTTPRequest(t *testing.T, f *subscriptionRunFixture, method, suffix string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	return nativeJSONRequest(f.handler, method, reservationEndpoint+suffix, f.token, raw)
}
func reservationHTTPGrants() map[string]any {
	return map[string]any{"confirmRead": true, "confirm": true, "confirmReleaseReservations": true, "confirmCloseRelated": true, "confirmNoContentProof": true, "confirmFutureOperations": true, "confirmOldExecutorsStopped": true}
}
func reservationHTTPInput(id string) map[string]any {
	return map[string]any{"seed": organization.ReservationOwnerRef{Kind: "organization", ID: id}, "decision": "independent complete target reservation release; no file operation", "confirmRead": true}
}
func reservationHTTPApplyInput(v organization.ReservationRelease) map[string]any {
	return mergeRecovery(reservationHTTPGrants(), map[string]any{"seed": v.Seed, "requestId": v.RequestID, "decision": v.Decision, "fingerprint": v.Fingerprint})
}
func TestReservationReleaseHTTPCompleteReviewOfflineRootsAndNoLegacyNetworkOrFileAction(t *testing.T) {
	f, source, target, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	var legacy, network atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legacy.Add(1)
		http.Error(w, "must not call Python", 500)
	}))
	defer old.Close()
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": "/offline/reservation-release-fixture-only"}); err != nil {
		t.Fatal(err)
	}
	var rebuildErr error
	f.handler, rebuildErr = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true, LegacyBackendURL: old.URL}, roundTripFunc(func(*http.Request) (*http.Response, error) { network.Add(1); return nil, context.Canceled }), &f.runner)
	if rebuildErr != nil {
		t.Fatal(rebuildErr)
	}
	v := recoveryData[organization.ReservationRelease](t, reservationHTTPRequest(t, f, "POST", "/preview", reservationHTTPInput(job.ID)))
	if v.State != "preview" || len(v.Owners) != 1 || v.RowCount == 0 || v.ClaimCount != 3 {
		t.Fatal(v)
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, job.ID).Scan(&count)
	if count != 2 {
		t.Fatal("preview released claims", count)
	}
	for key := range reservationHTTPGrants() {
		body := reservationHTTPApplyInput(v)
		body[key] = false
		if r := reservationHTTPRequest(t, f, "POST", "", body); r.Code != 400 {
			t.Fatal("missing independent grant accepted", key, r.Code, r.Body.String())
		}
	}
	out := recoveryData[organization.ReservationRelease](t, reservationHTTPRequest(t, f, "POST", "", reservationHTTPApplyInput(v)))
	if out.State != "released" || out.ID != v.ID {
		t.Fatal(out)
	}
	for _, item := range job.Items {
		b, err := os.ReadFile(filepath.Join(source, item.Source))
		if err != nil || string(b) != "original-media" {
			t.Fatal(string(b), err)
		}
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("release wrote media", entries, err)
	}
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, job.ID).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
	var state string
	f.db.QueryRow(`SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&state)
	if state != organization.RecoveryClosed {
		t.Fatal(state)
	}
	for _, suffix := range []string{"/" + v.ID} {
		receipt := recoveryData[organization.ReservationRelease](t, reservationHTTPRequest(t, f, "GET", suffix, nil))
		if receipt.ID != v.ID || receipt.Fingerprint != v.Fingerprint {
			t.Fatal(receipt)
		}
	}
	if r := reservationHTTPRequest(t, f, "POST", "", reservationHTTPApplyInput(v)); r.Code != 200 {
		t.Fatal("lost-response re-read rejected", r.Code, r.Body.String())
	}
	status := recoveryData[organization.ReservationReleaseStatus](t, reservationHTTPRequest(t, f, "GET", "", nil))
	if len(status.Owners) != 0 || len(status.Releases) != 1 {
		t.Fatal(status)
	}
	if legacy.Load() != 0 || network.Load() != 0 || f.adds != 0 {
		t.Fatal("ledger release contacted an executor", legacy.Load(), network.Load(), f.adds)
	}
	if strings.Contains(string(reservationHTTPRequest(t, f, "GET", "/"+v.ID, nil).Body.Bytes()), "\"document\"") {
		t.Fatal("private raw document leaked")
	}
}
func TestReservationReleaseHTTPStaleSnapshotStrictJSONAuthorizationAndWorkingGuards(t *testing.T) {
	f, _, _, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	in := reservationHTTPInput(job.ID)
	for _, name := range []string{"independent-recovery-disposal", "intake-enrichment-outbox", "library-scraper-publication", "rclone-namespace:NASTOOL", job.ID} {
		guard, err := f.runner.organizer.jobs.AcquireJob(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		r := reservationHTTPRequest(t, f, "POST", "/preview", in)
		guard.Close()
		if r.Code != 409 {
			t.Fatal("busy writer bypassed", name, r.Code, r.Body.String())
		}
	}
	v := recoveryData[organization.ReservationRelease](t, reservationHTTPRequest(t, f, "POST", "/preview", in))
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET REASON='changed after the review' WHERE JOB_ID=? AND ORDINAL=0`, job.ID); err != nil {
		t.Fatal(err)
	}
	if r := reservationHTTPRequest(t, f, "POST", "", reservationHTTPApplyInput(v)); r.Code != 409 {
		t.Fatal("stale review released", r.Code, r.Body.String())
	}
	for _, raw := range []string{`{"seed":{"kind":"organization","id":1},"confirmRead":true}`, `{"seed":{"kind":"organization","id":"` + job.ID + `"},"confirmRead":true,"decision":"bad","targetRoot":"/escape"}`, `{"seed":{"kind":"organization","id":"` + job.ID + `"},"confirmRead":true,"decision":"bad"} {}`} {
		if r := nativeJSONRequest(f.handler, "POST", reservationEndpoint+"/preview", f.token, raw); r.Code != 400 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if r := nativeJSONRequest(f.handler, "GET", reservationEndpoint, "", ""); r.Code != 401 {
		t.Fatal(r.Code)
	}
	viewer, err := f.runner.organizer.auth.service.IssueToken("viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r := nativeJSONRequest(f.handler, "GET", reservationEndpoint, viewer, ""); r.Code != 403 {
		t.Fatal("non-admin accessed ledger", r.Code)
	}
	if r := reservationHTTPRequest(t, f, "GET", "/"+strings.Repeat("f", 64), nil); r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := reservationHTTPRequest(t, f, "POST", "/preview", map[string]any{"seed": organization.ReservationOwnerRef{Kind: "organization", ID: strings.Repeat("f", 32)}, "decision": "missing owner", "confirmRead": true}); r.Code != 404 {
		t.Fatal(r.Code, r.Body.String())
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, job.ID).Scan(&count)
	if count != 2 {
		t.Fatal(count)
	}
}
func TestReservationReleaseHTTPClosedDeletionScopeIsReleasedWithoutDeletingItsFilesOrHistory(t *testing.T) {
	x := recoveryHTTPFixture(t, "")
	mediaRemovalHTTPHistory(t, x, 1)
	x.put(t, "Movie/movie.mkv", "keep media")
	in := mediaRemovalHTTPInput()
	in["flag"] = "del_dest"
	in["includeNfo"] = false
	removal := mediaRemovalHTTPCreate(t, x, in)
	f := x.f.f
	seed := map[string]any{"seed": organization.ReservationOwnerRef{Kind: "media-removal", ID: removal.ID}, "decision": "independent release of stopped removal range; preserve media", "confirmRead": true}
	v := recoveryData[organization.ReservationRelease](t, reservationHTTPRequest(t, f, "POST", "/preview", seed))
	if len(v.Owners) != 2 {
		t.Fatal(v.Owners)
	}
	if out := recoveryData[organization.ReservationRelease](t, reservationHTTPRequest(t, f, "POST", "", reservationHTTPApplyInput(v))); out.State != "released" {
		t.Fatal(out)
	}
	if x.read(t, "Movie/movie.mkv") != "keep media" {
		t.Fatal("media changed")
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
	if count != 1 {
		t.Fatal("history changed", count)
	}
	if r := mediaRemovalHTTPRequest(t, x, "POST", "/"+removal.ID+"/continue", mergeRecovery(mediaRemovalHTTPGrants(), map[string]any{"fingerprint": removal.Fingerprint})); r.Code < 400 {
		t.Fatal("closed old deletion ran", r.Code, r.Body.String())
	}
}
func TestReservationReleaseHTTPMixedModeNeverFallsBackToPython(t *testing.T) {
	f, _, _, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	var calls atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer old.Close()
	var rebuildErr error
	f.handler, rebuildErr = buildHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: old.URL}, f.transport, &f.runner)
	if rebuildErr != nil {
		t.Fatal(rebuildErr)
	}
	for _, spec := range []struct {
		method, suffix string
		body           any
	}{{"GET", "", nil}, {"POST", "/preview", reservationHTTPInput(job.ID)}, {"POST", "", reservationHTTPInput(job.ID)}, {"GET", "/" + strings.Repeat("a", 64), nil}, {"DELETE", "", nil}, {"PUT", "/" + strings.Repeat("a", 64), nil}, {"POST", "/misspelled/action", nil}} {
		if r := reservationHTTPRequest(t, f, spec.method, spec.suffix, spec.body); r.Code != 501 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("mixed mode invoked old backend", calls.Load())
	}
}
