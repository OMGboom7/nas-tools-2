package httpserver

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediasync"
)

func mediaScheduleRunner(t *testing.T, f *mediaSyncFixture) *mediaSyncAPI {
	t.Helper()
	var runner *rssRunAPI
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.calls++
		if r.URL.Host != "media.local" || r.Method != "GET" {
			t.Fatal("unexpected", r.URL)
		}
		switch r.URL.Path {
		case "/emby/Users":
			return jsonResponse(r, `[{"Id":"u","Name":"viewer","Policy":{"IsAdministrator":true}}]`), nil
		case "/emby/Users/u/Views":
			return jsonResponse(r, `{"Items":[{"Id":"films","Name":"Films","CollectionType":"movies"}],"TotalRecordCount":1}`), nil
		case "/emby/Users/u/Items":
			if f.afterRead != nil {
				f.afterRead()
			}
			if f.mode == "missing" {
				return jsonResponse(r, `{"Items":[],"TotalRecordCount":1}`), nil
			}
			return jsonResponse(r, `{"Items":[{"Id":"m","Name":"Movie","Type":"Movie"}],"TotalRecordCount":1}`), nil
		default:
			t.Fatal(r.URL)
			return nil, nil
		}
	})
	var err error
	f.handler, err = buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: filepath.Join(filepath.Dir(f.path), "config.yaml")}, transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	return runner.mediaSync
}
func mediaScheduleHTTPReview(t *testing.T, f *mediaSyncFixture) mediaScheduleReview {
	t.Helper()
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/schedule/review", f.token, `{"libraries":["films"],"hours":1,"configFingerprint":"`+f.fingerprint+`"}`)
	var envelope struct{ Data mediaScheduleReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return envelope.Data
}
func mediaScheduleHTTPActivate(t *testing.T, f *mediaSyncFixture, review mediaScheduleReview) mediasync.Schedule {
	t.Helper()
	raw, _ := json.Marshal(mediaScheduleInput{Libraries: review.Definition.Libraries, Hours: float64(review.Definition.IntervalSeconds) / 3600, ConfigFingerprint: review.Definition.ConfigDigest, Fingerprint: review.Fingerprint, Confirm: true, LegacyStopped: true})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/schedule/activate", f.token, string(raw))
	var envelope struct{ Data mediasync.Schedule }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return envelope.Data
}
func TestMediaScheduleHTTPReviewDoesNotRunAndActivationWaitsOnePeriod(t *testing.T) {
	f := newMediaSyncFixture(t)
	api := mediaScheduleRunner(t, f)
	ctx := context.Background()
	calls := f.calls
	if err := api.scheduleTick(ctx, time.Now().Add(100*time.Hour)); err != nil || f.calls != calls {
		t.Fatal("default worker reached remote", err)
	}
	review := mediaScheduleHTTPReview(t, f)
	view, _ := f.view(t)
	if view.Generation != "" {
		t.Fatal("review published cache")
	}
	state := mediaScheduleHTTPActivate(t, f, review)
	if !state.Enabled {
		t.Fatal(state)
	}
	calls = f.calls
	if err := api.scheduleTick(ctx, state.NextRun.Add(-time.Second)); err != nil || f.calls != calls {
		t.Fatal("early scheduled run", err)
	}
	if err := api.scheduleTick(ctx, state.NextRun); err != nil {
		t.Fatal(err)
	}
	view, _ = f.view(t)
	if view.Total != 1 || view.Generation == "" {
		t.Fatal(view)
	}
	runs, err := api.store.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != "completed" || runs[0].SnapshotGeneration != view.Generation {
		t.Fatal(runs, err)
	}
	calls = f.calls
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/library/sync/schedule", f.token, "")
	if w.Code != 200 || f.calls != calls || strings.Contains(w.Body.String(), "private-media-key") {
		t.Fatal(w.Code, w.Body.String())
	}
	var status struct{ Data struct{ WorkerRunning bool } }
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	if status.Data.WorkerRunning {
		t.Fatal("handler-only server claims live worker")
	}
}
func TestMediaScheduleFailureRetainsCacheAndNextPeriodReadsNewInventory(t *testing.T) {
	f := newMediaSyncFixture(t)
	api := mediaScheduleRunner(t, f)
	ctx := context.Background()
	if w := f.sync(); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	first, _ := f.view(t)
	state := mediaScheduleHTTPActivate(t, f, mediaScheduleHTTPReview(t, f))
	f.mode = "missing"
	if err := api.scheduleTick(ctx, state.NextRun); err != nil {
		t.Fatal(err)
	}
	view, _ := f.view(t)
	if view.Generation != first.Generation {
		t.Fatal("failed read cleared cache")
	}
	state, err := api.store.Schedule(ctx)
	if err != nil || state.LastState != "failed" || !state.Enabled {
		t.Fatal(state, err)
	}
	calls := f.calls
	api = mediaScheduleRunner(t, f)
	if err := api.scheduleTick(ctx, state.NextRun.Add(-time.Second)); err != nil || calls != f.calls {
		t.Fatal("restart replayed interval", err)
	}
	f.mode = ""
	if err := api.scheduleTick(ctx, state.NextRun); err != nil {
		t.Fatal(err)
	}
	runs, err := api.store.Runs(ctx)
	if err != nil || len(runs) != 2 || runs[0].State != "completed" || runs[1].State != "failed" {
		t.Fatal(runs, err)
	}
}
func TestMediaScheduleStopDuringRemoteReadPreventsSnapshotPublication(t *testing.T) {
	f := newMediaSyncFixture(t)
	api := mediaScheduleRunner(t, f)
	ctx := context.Background()
	if w := f.sync(); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	old, _ := f.view(t)
	state := mediaScheduleHTTPActivate(t, f, mediaScheduleHTTPReview(t, f))
	f.afterRead = func() {
		raw, _ := json.Marshal(map[string]any{"revision": state.Revision, "confirm": true})
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/schedule/stop", f.token, string(raw))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if err := api.scheduleTick(ctx, state.NextRun); err != mediasync.ErrSchedule {
		t.Fatal("stopped result not rejected", err)
	}
	view, _ := f.view(t)
	if view.Generation != old.Generation {
		t.Fatal("stopped collector published")
	}
	current, err := api.store.Schedule(ctx)
	if err != nil || current.Enabled || current.LastState != "stopped" {
		t.Fatal(current, err)
	}
}
func TestMediaScheduleScopeChangePausesAndNeverReactivatesOnConfigRestore(t *testing.T) {
	for _, field := range []string{"emby.username", "app.login_user", "media.mediasync_interval", "security.api_key"} {
		t.Run(field, func(t *testing.T) {
			f := newMediaSyncFixture(t)
			api := mediaScheduleRunner(t, f)
			ctx := context.Background()
			state := mediaScheduleHTTPActivate(t, f, mediaScheduleHTTPReview(t, f))
			configStore := config.NewStore(filepath.Join(filepath.Dir(f.path), "config.yaml"))
			original, err := configStore.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(field, ".")
			old := objectValue(original[parts[0]])[parts[1]]
			if err := configStore.Update(map[string]any{field: "changed"}); err != nil {
				t.Fatal(err)
			}
			calls := f.calls
			if err := api.scheduleTick(ctx, state.NextRun); err != nil || f.calls != calls {
				t.Fatal("revoked scope reached remote", err)
			}
			current, err := api.store.Schedule(ctx)
			if err != nil || current.Enabled || current.LastState != "paused" {
				t.Fatal(current, err)
			}
			if err := configStore.Update(map[string]any{field: old}); err != nil {
				t.Fatal(err)
			}
			if err := api.scheduleTick(ctx, state.NextRun.Add(time.Hour)); err != nil || f.calls != calls {
				t.Fatal("configuration restoration reactivated consent", err)
			}
		})
	}
}
func TestMediaScheduleConfigurationChangedMidScanRetainsPreviousSnapshot(t *testing.T) {
	f := newMediaSyncFixture(t)
	api := mediaScheduleRunner(t, f)
	ctx := context.Background()
	if w := f.sync(); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	old, _ := f.view(t)
	state := mediaScheduleHTTPActivate(t, f, mediaScheduleHTTPReview(t, f))
	f.afterRead = func() {
		if err := config.NewStore(filepath.Join(filepath.Dir(f.path), "config.yaml")).Update(map[string]any{"emby.username": "changed"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.scheduleTick(ctx, state.NextRun); err != nil {
		t.Fatal(err)
	}
	view, _ := f.view(t)
	if view.Generation != old.Generation {
		t.Fatal("scope drift published")
	}
	runs, err := api.store.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].State != "paused" {
		t.Fatal(runs, err)
	}
}
func TestMediaScheduleWorkerCancellationAndRuntimeLifecycle(t *testing.T) {
	f := newMediaSyncFixture(t)
	api := mediaScheduleRunner(t, f)
	state := mediaScheduleHTTPActivate(t, f, mediaScheduleHTTPReview(t, f))
	ctx, cancel := context.WithCancel(context.Background())
	f.afterRead = cancel
	if err := api.scheduleTick(ctx, state.NextRun); err != nil {
		t.Fatal(err)
	}
	runs, err := api.store.Runs(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != "interrupted" {
		t.Fatal(runs, err)
	}
	f.afterRead = nil
	calls := f.calls
	parent, stop := context.WithCancel(context.Background())
	wait := startMediaSyncWorker(parent, api)
	if !api.workerRunning.Load() {
		t.Fatal("worker absent")
	}
	stop()
	wait()
	if api.workerRunning.Load() || f.calls != calls {
		t.Fatal("shutdown did not join worker")
	}
}
func TestMediaScheduleStaleReviewAndMissingConfirmationsCannotEnable(t *testing.T) {
	f := newMediaSyncFixture(t)
	api := mediaScheduleRunner(t, f)
	review := mediaScheduleHTTPReview(t, f)
	state := mediaScheduleHTTPActivate(t, f, review)
	calls := f.calls
	for _, body := range []string{`{}`, `{"confirm":true}`, `{"legacyStopped":true}`} {
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/schedule/activate", f.token, body)
		if w.Code != 400 || f.calls != calls {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	raw, _ := json.Marshal(map[string]any{"confirm": true, "revision": state.Revision})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/schedule/stop", f.token, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	raw, _ = json.Marshal(mediaScheduleInput{Libraries: review.Definition.Libraries, Hours: 1, ConfigFingerprint: review.Definition.ConfigDigest, Fingerprint: review.Fingerprint, Confirm: true, LegacyStopped: true})
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/library/sync/schedule/activate", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal("stale review reactivated", w.Code, w.Body.String())
	}
	current, err := api.store.Schedule(context.Background())
	if err != nil || current.Enabled {
		t.Fatal(current, err)
	}
}
func TestMediaScheduleLegacyHoursAndBounds(t *testing.T) {
	for _, spec := range []struct {
		raw   any
		hours float64
		valid bool
	}{{nil, 0, true}, {"", 0, true}, {"0", 0, true}, {"1.5", 2, true}, {"2.5", 2, true}, {0.5, 0.5, true}, {"0.5", 0, true}, {-1, 0, false}, {"bad", 0, false}, {math.NaN(), 0, false}, {math.Inf(1), 0, false}, {9000, 0, false}, {true, 0, false}} {
		hours, err := legacyMediaScheduleHours(spec.raw)
		if (err == nil) != spec.valid || err == nil && hours != spec.hours {
			t.Fatal(spec, hours, err)
		}
	}
	for _, hours := range []float64{0, -1, 0.001, 8761, math.NaN(), math.Inf(1)} {
		if _, err := mediaScheduleSeconds(hours); err == nil {
			t.Fatal("accepted interval", hours)
		}
	}
}
func TestMediaScheduleProductionRuntimeStartsAndJoinsNativeWorker(t *testing.T) {
	f := newMediaSyncFixture(t)
	api := mediaScheduleRunner(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, wait, err := newRuntimeHandler(ctx, config.Config{DisableLegacy: true, ApplicationConfigPath: filepath.Join(filepath.Dir(f.path), "config.yaml")}, api.transport)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(h, "GET", "/api/v1/library/sync/schedule", f.token, "")
	var envelope struct{ Data struct{ WorkerRunning bool } }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || !envelope.Data.WorkerRunning {
		t.Fatal(w.Code, w.Body.String())
	}
	cancel()
	wait()
	w = nativeJSONRequest(h, "GET", "/api/v1/library/sync/schedule", f.token, "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &envelope) != nil || envelope.Data.WorkerRunning {
		t.Fatal(w.Code, w.Body.String())
	}
}
