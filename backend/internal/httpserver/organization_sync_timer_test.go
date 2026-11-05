package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func installSyncTimerFixture(t *testing.T, f *subscriptionRunFixture) {
	t.Helper()
	res := nativeJSONRequest(f.handler, http.MethodPost, "/api/v1/plugins/SyncTimer/install", f.token, "")
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	if _, err := f.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','plugin.SyncTimer','{"cron":"* * * * *","future":9007199254740993}')`); err != nil {
		t.Fatal(err)
	}
}

func activateSyncTimerFixture(t *testing.T, f *subscriptionRunFixture) organization.MonitorControl {
	t.Helper()
	input := monitorInput{IDs: []string{"7"}, Timer: true}
	b, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/review", f.token, string(b))
	var review struct{ Data monitorReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || review.Data.Lease.Timer == nil || !review.Data.Lease.IncludeExisting {
		t.Fatal(w.Code, w.Body.String())
	}
	input.Fingerprint, input.Confirm, input.ConfirmOldExecutorsStopped = review.Data.Fingerprint, true, true
	input.ConfirmSourceRemoval = review.Data.NeedsSourceRemoval
	b, _ = json.Marshal(input)
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(b))
	var result struct{ Data organization.MonitorControl }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.Lease.Timer == nil || result.Data.State != "scheduled" {
		t.Fatal(w.Code, w.Body.String())
	}
	return result.Data
}

func syncTimerDue(t *testing.T, api *organizationAPI, generation string) time.Time {
	t.Helper()
	clock, err := api.jobs.SyncTimerClock(t.Context(), generation)
	if err != nil {
		t.Fatal(err)
	}
	due, err := time.Parse(time.RFC3339Nano, clock.Next)
	if err != nil {
		t.Fatal(err)
	}
	return due
}

func TestSyncTimerFreezesPeriodAndPersistsExecutionAcrossRebuild(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	f, api, source, target := monitorFixture(t, "copy")
	installSyncTimerFixture(t, f)
	monitorTick(t, api, time.Now().Add(time.Hour))
	if rows, err := os.ReadDir(target); err != nil || len(rows) != 0 {
		t.Fatal("old plugin settings started automatic work", err)
	}
	control := activateSyncTimerFixture(t, f)
	gen := control.Lease.Generation
	due := syncTimerDue(t, api, gen)
	monitorTick(t, api, due.Add(-time.Second))
	if rows, err := api.jobs.SyncTimerRuns(t.Context(), gen); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	monitorTick(t, api, due)
	rows, err := api.jobs.SyncTimerRuns(t.Context(), gen)
	if err != nil || len(rows) != 1 || rows[0].State != "observing" || rows[0].Candidates != 4 {
		t.Fatal(rows, err)
	}
	id := rows[0].ID
	late := "misc/after-slot.txt"
	organizationFile(t, source, late)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(source, late), old, old); err != nil {
		t.Fatal(err)
	}
	api = monitorRebuild(t, f)
	monitorTick(t, api, due.Add(29*time.Second))
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("published before stability window", err)
	}
	monitorTick(t, api, due.Add(31*time.Second))
	rows, err = api.jobs.SyncTimerRuns(t.Context(), gen)
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].State != "scanned" {
		t.Fatal(rows, err)
	}
	if _, err := os.Stat(filepath.Join(target, late)); !os.IsNotExist(err) {
		t.Fatal("late arrival borrowed original slot", err)
	}
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM SYNC_HISTORY").Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	second := syncTimerDue(t, api, gen)
	monitorTick(t, api, second)
	monitorTick(t, api, second.Add(31*time.Second))
	assertOrganizationMoveBytes(t, filepath.Join(target, late), "original-media")
	if err := f.db.QueryRow("SELECT COUNT(*) FROM SYNC_HISTORY").Scan(&count); err != nil || count != 5 {
		t.Fatal(count, err)
	}
	page := nativeJSONRequest(f.handler, "GET", "/api/v1/plugins/SyncTimer/page", f.token, "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), id) {
		t.Fatal(page.Code, page.Body.String())
	}
}

func TestSyncTimerChangingSettingsRevokesOnlyTimerAndPreservesLegacyData(t *testing.T) {
	f, api, _, target := monitorFixture(t, "copy")
	installSyncTimerFixture(t, f)
	control := activateSyncTimerFixture(t, f)
	due := syncTimerDue(t, api, control.Lease.Generation)
	monitorTick(t, api, due)
	res := performFormRequest(f.handler, "/api/v1/plugin/config", f.token, url.Values{"id": {"SyncTimer"}, "config": {`{"cron":"0 8 * * 0"}`}})
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	st, err := api.jobs.SyncTimerSettings(t.Context())
	if err != nil || !strings.Contains(st.Raw, `"future":9007199254740993`) {
		t.Fatal(st, err)
	}
	after, err := api.jobs.MonitorControl(t.Context())
	if err != nil || after.State != "paused" {
		t.Fatal(after, err)
	}
	if rows, err := api.jobs.SyncTimerRuns(t.Context(), control.Lease.Generation); err != nil || len(rows) != 1 || rows[0].State != "needs_review" {
		t.Fatal(rows, err)
	}
	monitorTick(t, api, due.Add(time.Minute))
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("revoked slot wrote files", err)
	}
	activateSyncTimerFixture(t, f)
	res = performFormRequest(f.handler, "/api/v1/plugin/config", f.token, url.Values{"id": {"SyncTimer"}, "config": {`{"cron":"0 8 * * 0"}`}})
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	after, err = api.jobs.MonitorControl(t.Context())
	if err != nil || after.State != "paused" {
		t.Fatal("saving an unchanged cycle left previous authorization active", after, err)
	}
	plain := monitorEnable(t, f, true)
	res = nativeJSONRequest(f.handler, "DELETE", "/api/v1/plugins/SyncTimer", f.token, "")
	if res.Code != 200 {
		t.Fatal(res.Code, res.Body.String())
	}
	after, err = api.jobs.MonitorControl(t.Context())
	if err != nil || after.State != "enabled" || after.Lease.Generation != plain.Lease.Generation {
		t.Fatal(after, err)
	}
}

func TestSyncTimerHybridRoutesNeverFallBack(t *testing.T) {
	h, token, dbPath := nativeServicesFixture(t, "", nil, nil)
	settings, err := systemconfig.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer settings.Close()
	if err := settings.Set(t.Context(), "UserInstalledPlugins", `["SyncTimer"]`); err != nil {
		t.Fatal(err)
	}
	if err := settings.Set(t.Context(), organization.SyncTimerConfig, `{"cron":"0 8 * * 0"}`); err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(h, "GET", "/api/v1/plugins/SyncTimer", token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "0 8 * * 0") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = nativeJSONRequest(h, "PUT", "/api/v1/plugins/%20SyncTimer", token, `{"values":{"cron":""}}`)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSyncTimerReobservesSupersededSnapshotButDoesNotReplayCompletedJobs(t *testing.T) {
	f, api, source, target := monitorFixture(t, "copy")
	installSyncTimerFixture(t, f)
	control := activateSyncTimerFixture(t, f)
	gen := control.Lease.Generation
	due := syncTimerDue(t, api, gen)
	original := filepath.Join(source, "misc/archive.bin")
	info, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	monitorTick(t, api, due)
	if err := os.WriteFile(original, []byte("fixture changed after slot opened"), 0600); err != nil {
		t.Fatal(err)
	}
	monitorTick(t, api, due.Add(31*time.Second))
	if _, err := os.Stat(filepath.Join(target, "misc/archive.bin")); !os.IsNotExist(err) {
		t.Fatal("changed candidate was published", err)
	}
	if err := os.WriteFile(original, []byte("original-media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(original, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	next := syncTimerDue(t, api, gen)
	monitorTick(t, api, next)
	monitorTick(t, api, next.Add(31*time.Second))
	assertOrganizationMoveBytes(t, filepath.Join(target, "misc/archive.bin"), "original-media")
	var count int
	if err := f.db.QueryRow("SELECT COUNT(*) FROM SYNC_HISTORY").Scan(&count); err != nil || count != 4 {
		t.Fatal(count, err)
	}
	runs, err := api.jobs.SyncTimerRuns(t.Context(), gen)
	if err != nil || len(runs) != 2 {
		t.Fatal(runs, err)
	}
	for _, run := range runs {
		if run.Candidates != 4 || len(run.Keys) != 0 || run.State != "scanned" {
			t.Fatal("terminal slot retained its working snapshot", run)
		}
	}
}

func TestSyncTimerKeepsLegacyNamedRangeStepSemantics(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	loc, err := rssWorkerLocation()
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2026, 11, 2, 8, 0, 0, 0, loc)
	january := time.Date(2026, 1, 1, 8, 0, 0, 0, loc)
	for _, tc := range []struct {
		cron       string
		from, want time.Time
	}{
		{"0 8 * * mon-fri/2", monday, monday.AddDate(0, 0, 1)},
		{"0 8 * * 0-4/2", monday, monday.AddDate(0, 0, 2)},
		{"0 8 1 jan-mar/2 *", january, january.AddDate(0, 1, 0)},
	} {
		got, err := nextSyncTimer(organization.MonitorTimer{Cron: tc.cron, Timezone: loc.String()}, tc.from)
		if err != nil || !got.Equal(tc.want) {
			t.Fatal(tc.cron, got, tc.want, err)
		}
	}
}
