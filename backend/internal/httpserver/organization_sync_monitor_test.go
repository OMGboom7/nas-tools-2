package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func monitorRebuild(t *testing.T, f *subscriptionRunFixture) *organizationAPI {
	t.Helper()
	var err error
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport, &f.runner)
	if err != nil {
		t.Fatal(err)
	}
	return f.runner.organizer
}
func monitorFixture(t *testing.T, mode string) (*subscriptionRunFixture, *organizationAPI, string, string) {
	t.Helper()
	f, source, target, _ := directSyncFixture(t, mode)
	return f, monitorRebuild(t, f), source, target
}
func monitorEnable(t *testing.T, f *subscriptionRunFixture, existing bool) organization.MonitorControl {
	t.Helper()
	input := monitorInput{IDs: []string{"7"}, IncludeExisting: existing}
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/review", f.token, string(raw))
	var review struct{ Data monitorReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	input.Fingerprint = review.Data.Fingerprint
	input.Confirm = true
	input.ConfirmOldExecutorsStopped = true
	input.ConfirmSourceRemoval = review.Data.NeedsSourceRemoval
	raw, _ = json.Marshal(input)
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(raw))
	var result struct{ Data organization.MonitorControl }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return result.Data
}
func monitorEvents(t *testing.T, api *organizationAPI) []organization.MonitorEvent {
	t.Helper()
	c, err := api.jobs.MonitorControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items, err := api.jobs.MonitorEvents(context.Background(), c.Lease.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	return items
}
func monitorTick(t *testing.T, api *organizationAPI, now time.Time) {
	t.Helper()
	if err := api.syncMonitorTick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}

func TestSyncMonitorDisabledIsReadOnlyAndWorkerCancels(t *testing.T) {
	f, api, _, target := monitorFixture(t, "copy")
	monitorTick(t, api, time.Now().Add(time.Hour))
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/sync/monitor", f.token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"disabled"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_SYNC_MONITOR_%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := startSyncMonitorWorker(ctx, api)
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}
func TestSyncMonitorActivationRequiresFreshReviewAndIndependentConsents(t *testing.T) {
	f, api, source, target := monitorFixture(t, "move")
	input := monitorInput{IDs: []string{"7"}, IncludeExisting: true}
	review, failure := api.monitorReview(context.Background(), input, "admin")
	if failure != nil || !review.NeedsSourceRemoval {
		t.Fatal(review, failure)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_SYNC_MONITOR_CONTROL'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("review wrote control", count, err)
	}
	input.Fingerprint = review.Fingerprint
	input.Confirm = true
	for _, sourceConsent := range []bool{false, true} {
		input.ConfirmSourceRemoval = sourceConsent
		raw, _ := json.Marshal(input)
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(raw))
		if w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	input.ConfirmOldExecutorsStopped = true
	input.ConfirmSourceRemoval = false
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	organizationFile(t, source, "new.txt")
	input.ConfirmSourceRemoval = true
	raw, _ = json.Marshal(input)
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal("stale review", w.Code, w.Body.String())
	}
	c := monitorEnable(t, f, true)
	if c.State != "enabled" || !c.Lease.SourceRemoval || c.Lease.Generation == "" {
		t.Fatal(c)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("activation transferred", entries, err)
	}
}
func TestSyncMonitorAutomaticallyExecutesAllLocalModesWithoutDuplicateHistory(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, api, source, target := monitorFixture(t, mode)
			c := monitorEnable(t, f, true)
			start := time.Now()
			monitorTick(t, api, start)
			jobs, err := api.jobs.List(context.Background())
			if err != nil || len(jobs) != 0 {
				t.Fatal("not twice stable", jobs, err)
			}
			for i := 0; i < 6; i++ {
				monitorTick(t, api, start.Add(time.Duration(31+i)*time.Second))
			}
			jobs, err = api.jobs.List(context.Background())
			if err != nil || len(jobs) != 4 {
				t.Fatal(len(jobs), err, monitorEvents(t, api))
			}
			for _, job := range jobs {
				if job.State != "completed" || job.Mode != mode || job.Sync == nil || job.Sync.MonitorGeneration != c.Lease.Generation || job.Sync.MonitorEvent == "" {
					t.Fatal(job)
				}
				for _, item := range job.Items {
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
					if mode == "move" {
						if _, err := os.Lstat(filepath.Join(source, item.Source)); !os.IsNotExist(err) {
							t.Fatal(err)
						}
					} else {
						assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
					}
					if mode == "link" {
						a, _ := os.Stat(filepath.Join(source, item.Source))
						b, _ := os.Stat(filepath.Join(target, item.Target))
						if !os.SameFile(a, b) {
							t.Fatal("not hardlinked")
						}
					}
					if mode == "softlink" {
						if info, err := os.Lstat(filepath.Join(target, item.Target)); err != nil || info.Mode()&os.ModeSymlink == 0 {
							t.Fatal(info, err)
						}
					}
				}
			}
			var count, media int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM SYNC_HISTORY`).Scan(&count); err != nil || count != 4 {
				t.Fatal(count, err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&media); err != nil || media != 0 {
				t.Fatal(media, err)
			}
			monitorTick(t, api, start.Add(time.Minute))
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM SYNC_HISTORY`).Scan(&count); err != nil || count != 4 {
				t.Fatal(count, err)
			}
		})
	}
}
func TestSyncMonitorBaselineAndGrowingFileRequireNewStableSnapshot(t *testing.T) {
	f, api, source, target := monitorFixture(t, "copy")
	monitorEnable(t, f, false)
	start := time.Now()
	monitorTick(t, api, start.Add(time.Minute))
	jobs, err := api.jobs.List(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	organizationFile(t, source, "new.bin")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(source, "new.bin"), old, old); err != nil {
		t.Fatal(err)
	}
	monitorTick(t, api, start.Add(61*time.Second))
	if err := os.WriteFile(filepath.Join(source, "new.bin"), []byte("changed-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(source, "new.bin"), old, old); err != nil {
		t.Fatal(err)
	}
	monitorTick(t, api, start.Add(92*time.Second))
	if _, err := os.Stat(filepath.Join(target, "new.bin")); !os.IsNotExist(err) {
		t.Fatal("growing file executed", err)
	}
	monitorTick(t, api, start.Add(123*time.Second))
	assertOrganizationMoveBytes(t, filepath.Join(target, "new.bin"), "changed-data")
	superseded := false
	for _, e := range monitorEvents(t, api) {
		superseded = superseded || e.State == "superseded"
	}
	if !superseded {
		t.Fatal("old snapshot not superseded")
	}
}
func TestSyncMonitorInterruptedReservationNeverAutomaticallyRestartsAndFindsUnboundJobs(t *testing.T) {
	f, api, _, target := monitorFixture(t, "copy")
	c := monitorEnable(t, f, true)
	events, err := api.jobs.MonitorEvents(context.Background(), c.Lease.Generation, true)
	if err != nil || len(events) != 4 {
		t.Fatal(events, err)
	}
	event := events[0]
	plans, failure := api.monitorPlans(context.Background(), event, c.Lease)
	if failure != nil || len(plans) != 1 {
		t.Fatal(plans, failure)
	}
	if err := api.jobs.ReserveMonitorEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	d := plans[0].definition
	d.Sync.MonitorEvent = event.Key
	d.Sync.MonitorGeneration = c.Lease.Generation
	id, err := api.jobs.Create(context.Background(), organization.Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	monitorTick(t, api, time.Now())
	event, err = api.jobs.GetMonitorEvent(context.Background(), event.Key)
	if err != nil || event.State != "needs_review" || len(event.Jobs) != 1 || event.Jobs[0] != id {
		t.Fatal(event, err)
	}
	job, err := api.jobs.Get(context.Background(), id)
	if err != nil || job.State != "ready" {
		t.Fatal(job, err)
	}
	if _, err := os.Stat(filepath.Join(target, job.Items[0].Target)); !os.IsNotExist(err) {
		t.Fatal("interrupted reservation executed", err)
	}
	raw, _ := json.Marshal(map[string]any{"key": event.Key, "confirm": true, "reason": "reviewed"})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/dismiss", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	status, _, body := organizationJobRequest(t, f, id, "cancel")
	if status != 200 {
		t.Fatal(status, body)
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/dismiss", f.token, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestSyncMonitorCrossProcessGuardAndStopPreserveManualRecovery(t *testing.T) {
	f, api, _, _ := monitorFixture(t, "copy")
	c := monitorEnable(t, f, true)
	guard, err := api.jobs.AcquireJob(context.Background(), "native-sync-monitor")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.syncMonitorTick(context.Background(), time.Now().Add(time.Minute)); !errors.Is(err, organization.ErrBusy) {
		t.Fatal(err)
	}
	guard.Close()
	events, err := api.jobs.MonitorEvents(context.Background(), c.Lease.Generation, true)
	if err != nil {
		t.Fatal(err)
	}
	plans, failure := api.monitorPlans(context.Background(), events[0], c.Lease)
	if failure != nil {
		t.Fatal(failure)
	}
	d := plans[0].definition
	d.Sync.MonitorEvent = events[0].Key
	d.Sync.MonitorGeneration = c.Lease.Generation
	id, err := api.jobs.Create(context.Background(), organization.Digest(d), d)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/stop", f.token, `{"confirm":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	monitorTick(t, api, time.Now().Add(time.Minute))
	job, err := api.jobs.Get(context.Background(), id)
	if err != nil || job.State != "ready" {
		t.Fatal(job, err)
	}
	status, job, body := organizationMoveRequest(t, f, id, "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
}
func TestSyncMonitorConfigurationChangeAndUndoStayPaused(t *testing.T) {
	f, api, _, _ := monitorFixture(t, "copy")
	monitorEnable(t, f, false)
	input := syncConfigInput{Action: "update", ID: "7", Patch: syncConfigPatch{Note: strSync("changed")}}
	status, review, body := syncConfigPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, receipt, body := syncConfigSave(t, f, input, review, false)
	if status != 200 {
		t.Fatal(status, body)
	}
	c, err := api.jobs.MonitorControl(context.Background())
	if err != nil || c.State != "paused" {
		t.Fatal(c, err)
	}
	input = syncConfigInput{Action: "restore", RestoreID: receipt.ID}
	status, review, body = syncConfigPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	status, _, body = syncConfigSave(t, f, input, review, false)
	if status != 200 {
		t.Fatal(status, body)
	}
	c, err = api.jobs.MonitorControl(context.Background())
	if err != nil || c.State != "paused" {
		t.Fatal("undo resumed", c, err)
	}
}
func TestSyncMonitorStopsAtCheckpointWithoutSuccessHistory(t *testing.T) {
	f, api, _, target := monitorFixture(t, "copy")
	monitorEnable(t, f, true)
	if _, err := f.db.Exec(`CREATE TRIGGER stop_monitor AFTER UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='running' BEGIN UPDATE GO_SYNC_MONITOR_CONTROL SET STATE='paused',REASON='fixture checkpoint' WHERE ID=1; END`); err != nil {
		t.Fatal(err)
	}
	if err := api.syncMonitorTick(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("expected checkpoint rejection")
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='SYNC_HISTORY'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("false success history", count, err)
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	found := false
	for _, event := range monitorEvents(t, api) {
		found = found || event.State == "needs_review"
	}
	if !found {
		t.Fatal("missing interrupted event")
	}
}
func TestSyncMonitorRecognizedMovieTVAndSidecarsRouteToSeparateLibraries(t *testing.T) {
	f, source, movie, _, _ := organizationCopyFixture(t)
	tv := t.TempDir()
	if err := config.NewStore(f.path).Update(map[string]any{"media.tv_path": tv, "media.min_filesize": 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,'','','复制',1,1,1,NULL,1)`, source); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Show.S01E01.1080p.mkv", "Show.S01E01.1080p.en.srt"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	api := monitorRebuild(t, f)
	monitorEnable(t, f, true)
	monitorTick(t, api, time.Now().Add(time.Minute))
	jobs, err := api.jobs.List(context.Background())
	if err != nil || len(jobs) != 2 {
		t.Fatal(len(jobs), err, monitorEvents(t, api))
	}
	roots := map[string]bool{}
	for _, job := range jobs {
		roots[job.Definition.TargetRoot] = true
		if job.State != "completed" || job.Sync.Record.Rename != 1 || len(job.Items) != 2 {
			t.Fatal(job)
		}
		for _, item := range job.Items {
			assertOrganizationMoveBytes(t, filepath.Join(job.Definition.TargetRoot, item.Target), "original-media")
		}
	}
	movie, err = filepath.EvalSymlinks(movie)
	if err != nil {
		t.Fatal(err)
	}
	tv, err = filepath.EvalSymlinks(tv)
	if err != nil {
		t.Fatal(err)
	}
	if !roots[movie] || !roots[tv] {
		t.Fatal(roots)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE SOURCE='目录同步'`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	monitorTick(t, api, time.Now().Add(2*time.Minute))
	jobs, err = api.jobs.List(context.Background())
	if err != nil || len(jobs) != 2 {
		t.Fatal("duplicate recognized jobs", len(jobs), err)
	}
}
func TestSyncMonitorRecognizedDirectoryScopesDoNotExpandIntoChildren(t *testing.T) {
	f, source, target, _, _ := organizationCopyFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,?,'','复制',1,1,1,NULL,1)`, source, target); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, source, "child/Unknown.mkv")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(source, "child/Unknown.mkv"), old, old); err != nil {
		t.Fatal(err)
	}
	api := monitorRebuild(t, f)
	plans, failure := api.recognizedSyncPlans(context.Background(), mustMonitorSync(t, api), ".", "admin")
	if failure != nil || len(plans) != 1 || len(plans[0].definition.Entries) != 2 {
		t.Fatal(plans, failure)
	}
	for _, entry := range plans[0].definition.Entries {
		if strings.HasPrefix(entry.Source, "child/") {
			t.Fatal("scope expanded", entry)
		}
	}
}
func mustMonitorSync(t *testing.T, api *organizationAPI) organization.SyncRecord {
	t.Helper()
	r, err := api.jobs.GetSync(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestSyncMonitorRejectsRemoteAndPrivatePayloadWithoutLegacyFallback(t *testing.T) {
	f, api, _, _ := monitorFixture(t, "copy")
	if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET MODE='Rclone复制' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	_, failure := api.monitorReview(context.Background(), monitorInput{IDs: []string{"7"}}, "admin")
	if failure == nil || failure.status != 501 {
		t.Fatal(failure)
	}
	for _, route := range []string{"/api/v1/organization/plan", "/api/v1/organization/sync/monitor/review"} {
		w := nativeJSONRequest(f.handler, "POST", route, f.token, `{"sync":{"monitorGeneration":"injected"}}`)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/sync/monitor", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSyncMonitorAnimeRecognitionExecutesAllModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, _, anime, _ := animeOrganizationFixture(t, true, true)
			if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,'','',?,1,1,1,NULL,1)`, source, organization.ModeLabel(mode)); err != nil {
				t.Fatal(err)
			}
			api := monitorRebuild(t, f)
			monitorEnable(t, f, true)
			monitorTick(t, api, time.Now().Add(time.Minute))
			jobs, err := api.jobs.List(context.Background())
			if err != nil || len(jobs) != 1 {
				t.Fatal(jobs, err, monitorEvents(t, api))
			}
			job := jobs[0]
			physical, err := filepath.EvalSymlinks(anime)
			if err != nil {
				t.Fatal(err)
			}
			if job.State != "completed" || job.Mode != mode || job.Definition.TargetRoot != physical || len(job.Items) != 2 {
				t.Fatal(job)
			}
			for _, item := range job.Items {
				if item.MediaType != "动漫" || item.Category != "Animation" {
					t.Fatal(item)
				}
				assertOrganizationMoveBytes(t, filepath.Join(anime, item.Target), "original-media")
			}
		})
	}
}
func TestSyncMonitorRecognizedIgnoreRulesAlsoExcludeCompanions(t *testing.T) {
	f, source, target, _, _ := organizationCopyFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,?,'','复制',1,1,1,NULL,1)`, source, target); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.ignored_files": `\.srt$`}); err != nil {
		t.Fatal(err)
	}
	api := monitorRebuild(t, f)
	plans, failure := api.recognizedSyncPlans(context.Background(), mustMonitorSync(t, api), ".", "admin")
	if failure != nil || len(plans) != 1 || len(plans[0].definition.Entries) != 1 || plans[0].definition.Entries[0].Kind != "media" {
		t.Fatal(plans, failure)
	}
}
func TestSyncMonitorCompanionConflictRejectsEntireGroup(t *testing.T) {
	f, source, target, _, _ := organizationCopyFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,?,'','复制',1,1,1,NULL,1)`, source, target); err != nil {
		t.Fatal(err)
	}
	api := monitorRebuild(t, f)
	plans, failure := api.recognizedSyncPlans(context.Background(), mustMonitorSync(t, api), ".", "admin")
	if failure != nil || len(plans) != 1 {
		t.Fatal(plans, failure)
	}
	for _, entry := range plans[0].definition.Entries {
		if entry.Kind == "subtitle" {
			organizationFile(t, target, entry.Target)
		}
	}
	monitorEnable(t, f, true)
	monitorTick(t, api, time.Now().Add(time.Minute))
	events := monitorEvents(t, api)
	if len(events) != 1 || events[0].State != "needs_review" {
		t.Fatal(events)
	}
	jobs, err := api.jobs.List(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Fatal("partial group executed", jobs, err)
	}
}
func TestSyncMonitorReactivationCannotBypassOldUncertainScope(t *testing.T) {
	f, api, _, _ := monitorFixture(t, "copy")
	old := monitorEnable(t, f, true)
	events, err := api.jobs.MonitorEvents(context.Background(), old.Lease.Generation, true)
	if err != nil {
		t.Fatal(err)
	}
	event := events[0]
	if err := api.jobs.SetMonitorEvent(context.Background(), event.Key, "needs_review", nil, "fixture interrupted"); err != nil {
		t.Fatal(err)
	}
	next := monitorEnable(t, f, true)
	if next.Lease.Generation == old.Lease.Generation {
		t.Fatal("generation reused")
	}
	for i := 0; i < 5; i++ {
		monitorTick(t, api, time.Now().Add(time.Duration(60+i)*time.Second))
	}
	current, err := api.jobs.GetMonitorEvent(context.Background(), organization.MonitorEventKey(next.Lease.Generation, event.Snapshot))
	if err != nil || current.State != "needs_review" || len(current.Jobs) != 0 {
		t.Fatal("old uncertainty bypassed", current, err)
	}
}
func TestSyncMonitorYAMLChangePausesWithoutExecution(t *testing.T) {
	f, api, _, _ := monitorFixture(t, "copy")
	monitorEnable(t, f, true)
	if err := config.NewStore(f.path).Update(map[string]any{"media.ignored_files": "new-rule"}); err != nil {
		t.Fatal(err)
	}
	monitorTick(t, api, time.Now().Add(time.Minute))
	c, err := api.jobs.MonitorControl(context.Background())
	if err != nil || c.State != "paused" {
		t.Fatal(c, err)
	}
	jobs, err := api.jobs.List(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}

func TestSyncMonitorAllRoutesRequireAdminAndPureGo(t *testing.T) {
	f, _, _, _ := monitorFixture(t, "copy")
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "viewer", "strong-password")
	mixed, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/review", "/activate", "/stop", "/dismiss"} {
		method := "POST"
		if suffix == "" {
			method = "GET"
		}
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, method, "/api/v1/organization/sync/monitor"+suffix, token, `{}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(suffix, w.Code, w.Body.String())
			}
		}
		w := nativeJSONRequest(mixed, method, "/api/v1/organization/sync/monitor"+suffix, f.token, `{}`)
		if w.Code != 501 {
			t.Fatal(suffix, w.Code, w.Body.String())
		}
	}
}
func TestSyncMonitorSourceChangedDuringRecognitionCannotUseUnobservedEntries(t *testing.T) {
	f, source, target, _, _ := organizationCopyFixture(t)
	if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,?,'','复制',1,1,1,NULL,1)`, source, target); err != nil {
		t.Fatal(err)
	}
	base := f.transport
	changed := false
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !changed {
			changed = true
			name := filepath.Join(source, "Movie.2026.1080p.mkv")
			if err := os.WriteFile(name, []byte("unobserved-version"), 0600); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(name, old, old); err != nil {
				t.Fatal(err)
			}
		}
		return base.RoundTrip(r)
	})
	api := monitorRebuild(t, f)
	monitorEnable(t, f, true)
	monitorTick(t, api, time.Now().Add(time.Minute))
	events := monitorEvents(t, api)
	if len(events) != 1 || events[0].State != "needs_review" {
		t.Fatal(events)
	}
	jobs, err := api.jobs.List(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}
