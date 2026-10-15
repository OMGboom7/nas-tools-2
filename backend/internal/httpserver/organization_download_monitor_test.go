package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type downloadMonitorWire struct {
	batch      *downloadBatchWire
	ids        []string
	version    string
	pauseCalls int
	lostPause  bool
	listError  bool
	onPause    func()
}

func downloadMonitorFixture(t *testing.T, kind, mode string, modern bool) (*subscriptionRunFixture, string, string, *downloadMonitorWire) {
	t.Helper()
	f, source, target, batch := downloadBatchFixture(t, kind, mode, modern)
	wire := &downloadMonitorWire{batch: batch, ids: []string{downloadTaskID(kind)}, version: "v4.6.7"}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "completion.test" {
			return base.RoundTrip(r)
		}
		if kind == "qbittorrent" {
			if strings.HasSuffix(r.URL.Path, "/app/version") {
				return jsonResponse(r, wire.version), nil
			}
			if strings.HasSuffix(r.URL.Path, "/torrents/info") && r.URL.Query().Get("filter") == "completed" {
				if r.URL.Query().Get("limit") != "1001" {
					t.Fatal("unbounded candidates", r.URL)
				}
				if wire.listError {
					return nil, errors.New("list unavailable")
				}
				rows := []any{}
				if !batch.removed {
					for _, id := range wire.ids {
						rows = append(rows, map[string]string{"hash": id})
					}
				}
				raw, _ := json.Marshal(rows)
				return jsonResponse(r, string(raw)), nil
			}
			if strings.HasSuffix(r.URL.Path, "/torrents/pause") || strings.HasSuffix(r.URL.Path, "/torrents/stop") {
				_ = r.ParseForm()
				if r.Method != "POST" || r.Form.Get("hashes") != completionHash || strings.HasPrefix(wire.version, "v5") != strings.HasSuffix(r.URL.Path, "/stop") {
					t.Fatal("pause scope/version", r.URL, r.Form)
				}
				var state string
				if err := f.db.QueryRow(`SELECT STATE FROM GO_DOWNLOAD_MONITOR_EVENTS WHERE TASK_ID=?`, completionHash).Scan(&state); err != nil || state != "pausing" {
					t.Fatal("missing durable pause intent", state, err)
				}
				wire.pauseCalls++
				batch.opts.state = "pausedUP"
				if strings.HasPrefix(wire.version, "v5") {
					batch.opts.state = "stoppedUP"
				}
				if wire.onPause != nil {
					wire.onPause()
				}
				if wire.lostPause {
					return nil, errors.New("lost pause response")
				}
				return jsonResponse(r, ""), nil
			}
			return base.RoundTrip(r)
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var payload struct {
			ID        json.RawMessage            `json:"id"`
			Method    string                     `json:"method"`
			Arguments map[string]json.RawMessage `json:"arguments"`
			Params    json.RawMessage            `json:"params"`
		}
		if json.Unmarshal(raw, &payload) != nil {
			t.Fatal("request")
		}
		if kind == "transmission" && r.Header.Get("X-Transmission-Session-Id") != "" {
			args := payload.Arguments
			if modern {
				_ = json.Unmarshal(payload.Params, &args)
			}
			reply := func(data any) *http.Response {
				env := map[string]any{"result": "success", "arguments": data}
				if modern {
					env = map[string]any{"jsonrpc": "2.0", "id": 1, "result": data}
				}
				body, _ := json.Marshal(env)
				return jsonResponse(r, string(body))
			}
			if (payload.Method == "torrent-get" || payload.Method == "torrent_get") && args["ids"] == nil {
				if wire.listError {
					return nil, errors.New("list unavailable")
				}
				rows := []any{}
				key := func(a, b string) string {
					if modern {
						return b
					}
					return a
				}
				if !batch.removed {
					for _, id := range wire.ids {
						rows = append(rows, map[string]any{key("hashString", "hash_string"): id, key("percentDone", "percent_done"): 1, key("metadataPercentComplete", "metadata_percent_complete"): 1, key("leftUntilDone", "left_until_done"): 0, "status": 6, "error": 0})
					}
				}
				return reply(map[string]any{"torrents": rows}), nil
			}
			if payload.Method == "torrent-stop" || payload.Method == "torrent_stop" {
				var ids []string
				_ = json.Unmarshal(args["ids"], &ids)
				if len(ids) != 1 || ids[0] != completionHash {
					t.Fatal("stop scope", ids)
				}
				var state string
				if err := f.db.QueryRow(`SELECT STATE FROM GO_DOWNLOAD_MONITOR_EVENTS WHERE TASK_ID=?`, completionHash).Scan(&state); err != nil || state != "pausing" {
					t.Fatal(state, err)
				}
				wire.pauseCalls++
				batch.opts.state = "stopped"
				if wire.onPause != nil {
					wire.onPause()
				}
				if wire.lostPause {
					return nil, errors.New("lost stop response")
				}
				return reply(map[string]any{}), nil
			}
		}
		if kind == "aria2" && payload.Method == "aria2.tellStopped" {
			var params []json.RawMessage
			_ = json.Unmarshal(payload.Params, &params)
			var count int
			if len(params) > 2 {
				_ = json.Unmarshal(params[2], &count)
			}
			if count == 1001 {
				if wire.listError {
					return nil, errors.New("list unavailable")
				}
				rows := []any{}
				if !batch.removed {
					for _, id := range wire.ids {
						rows = append(rows, map[string]string{"gid": id, "status": "complete"})
					}
				}
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": payload.ID, "result": rows})
				return jsonResponse(r, string(body)), nil
			}
		}
		return base.RoundTrip(r)
	})
	monitorRebuild(t, f)
	return f, source, target, wire
}
func downloadMonitorEnable(t *testing.T, f *subscriptionRunFixture, existing, omitted bool) organization.DownloadMonitorControl {
	t.Helper()
	input := downloadMonitorInput{IDs: []string{"1"}, IncludeExisting: existing, AllowOmitted: omitted}
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/review", f.token, string(raw))
	var review struct{ Data downloadMonitorReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	input.Fingerprint = review.Data.Fingerprint
	input.Confirm = true
	input.ConfirmOldExecutorsStopped = true
	input.ConfirmResultWrites = true
	input.ConfirmSourceRemoval = review.Data.NeedsSourceRemoval
	input.ConfirmPauseTasks = review.Data.NeedsPauseTasks
	input.ConfirmTaskRemoval = review.Data.NeedsTaskRemoval
	input.ConfirmExclusiveLabels = review.Data.NeedsExclusiveLabels
	raw, _ = json.Marshal(input)
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/activate", f.token, string(raw))
	var result struct {
		Data organization.DownloadMonitorControl
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return result.Data
}
func downloadMonitorTick(t *testing.T, f *subscriptionRunFixture, now time.Time) {
	t.Helper()
	if err := f.runner.organizer.downloadMonitorTick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
}
func downloadMonitorEvents(t *testing.T, f *subscriptionRunFixture) []organization.DownloadMonitorEvent {
	t.Helper()
	c, err := f.runner.organizer.jobs.DownloadMonitorControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.runner.organizer.jobs.DownloadMonitorEvents(context.Background(), c.Lease.Generation, false)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestDownloadMonitorDisabledDoesNotReadSourceOrStartWorkAndJoinsCancellation(t *testing.T) {
	f, _, _, _ := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	api := f.runner.organizer
	api.service.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("disabled worker made request", r.URL)
		return nil, nil
	})
	downloadMonitorTick(t, f, time.Now())
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_DOWNLOAD_MONITOR_%'`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := startDownloadMonitorWorker(ctx, api)
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
}
func TestDownloadMonitorAutomaticallyRunsEveryModeAndProtocolExactlyOnce(t *testing.T) {
	for _, spec := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}} {
		for _, mode := range []string{"copy", "link", "softlink", "move"} {
			t.Run(spec.kind+mode+map[bool]string{true: "modern"}[spec.modern], func(t *testing.T) {
				f, source, target, wire := downloadMonitorFixture(t, spec.kind, mode, spec.modern)
				if mode == "move" && spec.kind != "aria2" {
					wire.batch.opts.state = "stalledUP"
					if spec.kind == "transmission" {
						wire.batch.opts.state = "seeding"
					}
				}
				lease := downloadMonitorEnable(t, f, true, false)
				now := time.Now()
				downloadMonitorTick(t, f, now)
				jobs, err := f.runner.organizer.jobs.List(context.Background())
				if err != nil || len(jobs) != 0 || wire.pauseCalls != 0 {
					t.Fatal("first observation executed", jobs, err)
				}
				downloadMonitorTick(t, f, now.Add(31*time.Second))
				b, err := f.runner.organizer.jobs.FindDownloadBatch(context.Background(), "1", downloadTaskID(spec.kind))
				if err != nil || b.State != "completed" || b.Origin.MonitorGeneration != lease.Lease.Generation || b.Origin.MonitorEvent == "" {
					t.Fatal(b, err, downloadMonitorEvents(t, f))
				}
				if wire.batch.markCalls+wire.batch.removeCalls != 1 {
					t.Fatal("missing/duplicate result", wire.batch)
				}
				wantPause := 0
				if mode == "move" && spec.kind != "aria2" {
					wantPause = 1
				}
				if wire.pauseCalls != wantPause {
					t.Fatal("pause behavior", wire.pauseCalls)
				}
				events := downloadMonitorEvents(t, f)
				if len(events) != 1 || events[0].State != "completed" || events[0].BatchID != b.ID {
					t.Fatal(events)
				}
				job, err := f.runner.organizer.jobs.Get(context.Background(), b.Jobs[0])
				if err != nil || job.State != "completed" || len(job.Items) != 2 {
					t.Fatal(job, err)
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
				}
				downloadMonitorTick(t, f, now.Add(time.Minute))
				if wire.batch.markCalls+wire.batch.removeCalls != 1 || wire.pauseCalls != wantPause {
					t.Fatal("automatic replay")
				}
			})
		}
	}
}
func TestDownloadMonitorBaselineOnlyAndGrowingSourceNeedFreshStableObservation(t *testing.T) {
	f, source, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	wire.ids = nil
	downloadMonitorEnable(t, f, false, false)
	now := time.Now()
	wire.ids = []string{completionHash}
	downloadMonitorTick(t, f, now)
	name := filepath.Join(source, wire.batch.files[0])
	if err := os.WriteFile(name, []byte("changed-media!"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(name, old, old)
	downloadMonitorTick(t, f, now.Add(31*time.Second))
	if _, err := f.runner.organizer.jobs.FindDownloadBatch(context.Background(), "1", completionHash); !errors.Is(err, organization.ErrNotFound) {
		t.Fatal("growing snapshot ran", err)
	}
	if err := os.WriteFile(name, []byte("original-media"), 0600); err != nil {
		t.Fatal(err)
	}
	old = old.Add(time.Minute)
	_ = os.Chtimes(name, old, old)
	downloadMonitorTick(t, f, now.Add(62*time.Second))
	downloadMonitorTick(t, f, now.Add(93*time.Second))
	b, err := f.runner.organizer.jobs.FindDownloadBatch(context.Background(), "1", completionHash)
	if err != nil || b.State != "completed" {
		t.Fatal(b, err, downloadMonitorEvents(t, f))
	}
}
func TestDownloadMonitorExistingBaselineNeverBecomesExecutionGrant(t *testing.T) {
	f, _, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	downloadMonitorEnable(t, f, false, false)
	downloadMonitorTick(t, f, time.Now().Add(time.Hour))
	jobs, err := f.runner.organizer.jobs.List(context.Background())
	if err != nil || len(jobs) != 0 || wire.batch.markCalls != 0 || downloadMonitorEvents(t, f)[0].State != "baseline" {
		t.Fatal(jobs, err)
	}
}
func TestDownloadMonitorActivationRequiresEveryIndependentAuthorityAndFreshScope(t *testing.T) {
	f, _, _, wire := downloadMonitorFixture(t, "transmission", "move", false)
	input := downloadMonitorInput{IDs: []string{"1"}, IncludeExisting: true}
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/review", f.token, string(raw))
	var review struct{ Data downloadMonitorReview }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	input.Fingerprint = review.Data.Fingerprint
	input.Confirm = true
	input.ConfirmOldExecutorsStopped = true
	input.ConfirmResultWrites = true
	for _, kind := range []string{"move", "pause", "remove"} {
		bad := input
		bad.ConfirmSourceRemoval = kind != "move"
		bad.ConfirmPauseTasks = kind != "pause"
		bad.ConfirmTaskRemoval = kind != "remove"
		raw, _ := json.Marshal(bad)
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/activate", f.token, string(raw))
		if w.Code != 409 {
			t.Fatal(kind, w.Code, w.Body.String())
		}
	}
	wire.ids = nil
	input.ConfirmSourceRemoval = true
	input.ConfirmPauseTasks = true
	input.ConfirmTaskRemoval = true
	raw, _ = json.Marshal(input)
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/activate", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal("stale candidate baseline accepted", w.Code, w.Body.String())
	}
}
func TestDownloadMonitorConfigurationChangePausesAndRestoringItDoesNotReactivate(t *testing.T) {
	f, _, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	downloadMonitorEnable(t, f, true, false)
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET MATCH_PATH=0 WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	downloadMonitorTick(t, f, time.Now())
	c, err := f.runner.organizer.jobs.DownloadMonitorControl(context.Background())
	if err != nil || c.State != "paused" {
		t.Fatal(c, err)
	}
	if _, err := f.db.Exec(`UPDATE DOWNLOADER SET MATCH_PATH=1 WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	downloadMonitorTick(t, f, time.Now().Add(time.Hour))
	if wire.batch.markCalls != 0 {
		t.Fatal("config undo reactivated")
	}
}
func TestDownloadMonitorLostPauseNeverRepeatsPauseOrStartsFiles(t *testing.T) {
	f, source, target, wire := downloadMonitorFixture(t, "qbittorrent", "move", false)
	wire.version = "v5.0.0"
	wire.batch.opts.state = "stalledUP"
	wire.lostPause = true
	downloadMonitorEnable(t, f, true, false)
	now := time.Now()
	downloadMonitorTick(t, f, now)
	if err := f.runner.organizer.downloadMonitorTick(context.Background(), now.Add(31*time.Second)); err == nil {
		t.Fatal("lost pause treated as completion")
	}
	downloadMonitorTick(t, f, now.Add(time.Minute))
	events := downloadMonitorEvents(t, f)
	if wire.pauseCalls != 1 || events[0].State != "needs_review" || wire.batch.removeCalls != 0 {
		t.Fatal(events, wire.pauseCalls)
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatal(entries)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, wire.batch.files[0]), "original-media")
}
func TestDownloadMonitorRevocationAtPreparedCheckpointPreservesExplicitRecovery(t *testing.T) {
	f, _, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	downloadMonitorEnable(t, f, true, false)
	now := time.Now()
	downloadMonitorTick(t, f, now)
	if _, err := f.db.Exec(`CREATE TRIGGER revoke_download_lease AFTER UPDATE OF STATE ON GO_ORGANIZATION_ITEMS WHEN NEW.STATE='prepared' BEGIN UPDATE GO_DOWNLOAD_MONITOR_CONTROL SET STATE='paused',REASON='fixture revoke' WHERE ID=1; END`); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.organizer.downloadMonitorTick(context.Background(), now.Add(31*time.Second)); err == nil {
		t.Fatal("revoked automatic history commit")
	}
	events := downloadMonitorEvents(t, f)
	if len(events) != 1 || events[0].State != "needs_review" || events[0].BatchID == "" || wire.batch.markCalls != 0 {
		t.Fatal(events)
	}
	b, err := f.runner.organizer.jobs.GetDownloadBatch(context.Background(), events[0].BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER revoke_download_lease`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationJobRequest(t, f, b.Jobs[0], "resume-publication")
	if status != 200 {
		t.Fatal(status, body)
	}
	status, job, body := organizationJobRequest(t, f, b.Jobs[0], "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
}
func TestDownloadMonitorLostResultIsOnlyManuallyReconciled(t *testing.T) {
	f, _, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	wire.batch.lostAck = true
	downloadMonitorEnable(t, f, true, false)
	now := time.Now()
	downloadMonitorTick(t, f, now)
	if err := f.runner.organizer.downloadMonitorTick(context.Background(), now.Add(31*time.Second)); err == nil {
		t.Fatal("ambiguous result reported success")
	}
	downloadMonitorTick(t, f, now.Add(time.Minute))
	events := downloadMonitorEvents(t, f)
	if events[0].State != "needs_review" || wire.batch.markCalls != 1 {
		t.Fatal(events, wire.batch.markCalls)
	}
	b, err := f.runner.organizer.jobs.GetDownloadBatch(context.Background(), events[0].BatchID)
	if err != nil || b.State != "result_pending" {
		t.Fatal(b, err)
	}
	status, b, body := downloadBatchActionHTTP(t, f, b, "reconcile-result", downloadBatchRequest{Confirm: true})
	if status != 200 || b.State != "completed" || wire.batch.markCalls != 1 {
		t.Fatal(status, body)
	}
}
func TestDownloadMonitorNoAutomaticAdoptionOfSavedBatchOrInterruptedEvent(t *testing.T) {
	f, _, _, _ := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	lease := downloadMonitorEnable(t, f, true, false)
	downloadMonitorTick(t, f, time.Now().Add(time.Hour))
	events := downloadMonitorEvents(t, f)
	if len(events) != 1 || events[0].State != "linked" || events[0].BatchID != b.ID {
		t.Fatal(events)
	}
	fresh, err := f.runner.organizer.jobs.GetDownloadBatch(context.Background(), b.ID)
	if err != nil || fresh.State != "ready" {
		t.Fatal(fresh, err)
	}
	if err := f.runner.organizer.jobs.PauseDownloadMonitorGeneration(context.Background(), "older-generation", "must not pause the current grant"); err != nil {
		t.Fatal(err)
	}
	control, err := f.runner.organizer.jobs.DownloadMonitorControl(context.Background())
	if err != nil || control.State != "enabled" || control.Lease.Generation != lease.Lease.Generation {
		t.Fatal(control, err)
	}
}
func TestDownloadMonitorGlobalGuardAndDismissRequireTerminalLinkedJobs(t *testing.T) {
	f, _, _, _ := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	b := saveDownloadFixtureBatch(t, f, "qbittorrent")
	downloadMonitorEnable(t, f, true, false)
	guard, err := f.runner.organizer.jobs.AcquireJob(context.Background(), "native-download-monitor")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.organizer.downloadMonitorTick(context.Background(), time.Now()); !errors.Is(err, organization.ErrBusy) {
		t.Fatal(err)
	}
	guard.Close()
	downloadMonitorTick(t, f, time.Now())
	event := downloadMonitorEvents(t, f)[0]
	raw, _ := json.Marshal(map[string]any{"key": event.Key, "reason": "audited", "confirm": true})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/dismiss", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, id := range b.Jobs {
		status, _, body := organizationJobRequest(t, f, id, "cancel")
		if status != 200 {
			t.Fatal(status, body)
		}
	}
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/dismiss", f.token, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestDownloadMonitorAllRoutesRequireAdminAndPureGo(t *testing.T) {
	f, _, _, _ := downloadMonitorFixture(t, "qbittorrent", "copy", false)
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
		path := "/api/v1/organization/downloads/monitor" + suffix
		for _, token := range []string{"", viewer} {
			w := nativeJSONRequest(f.handler, method, path, token, `{}`)
			want := 403
			if token == "" {
				want = 401
			}
			if w.Code != want {
				t.Fatal(path, w.Code, w.Body.String())
			}
		}
		w := nativeJSONRequest(mixed, method, path, f.token, `{}`)
		if w.Code != 501 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
}

func TestDownloadMonitorInterruptedGenerationRequiresExplicitDismissalNotAutomaticReplay(t *testing.T) {
	f, _, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	downloadMonitorEnable(t, f, true, false)
	now := time.Now()
	downloadMonitorTick(t, f, now)
	event := downloadMonitorEvents(t, f)[0]
	if err := f.runner.organizer.jobs.SetDownloadMonitorEvent(context.Background(), event, "reserving", event.Snapshot, event.FirstSeen, "", "fixture interrupted before batch response", now); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f)
	downloadMonitorTick(t, f, now.Add(time.Minute))
	if downloadMonitorEvents(t, f)[0].State != "needs_review" {
		t.Fatal(downloadMonitorEvents(t, f))
	}
	downloadMonitorEnable(t, f, true, false)
	downloadMonitorTick(t, f, now.Add(2*time.Minute))
	for _, e := range downloadMonitorEvents(t, f) {
		if e.State != "needs_review" {
			t.Fatal("old uncertainty bypassed", e)
		}
		raw, _ := json.Marshal(map[string]any{"key": e.Key, "reason": "audited no saved work", "confirm": true})
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/dismiss", f.token, string(raw))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if wire.batch.markCalls != 0 {
		t.Fatal("unexpected execution")
	}
	downloadMonitorEnable(t, f, true, false)
	downloadMonitorTick(t, f, now.Add(3*time.Minute))
	downloadMonitorTick(t, f, now.Add(4*time.Minute))
	b, err := f.runner.organizer.jobs.FindDownloadBatch(context.Background(), "1", completionHash)
	if err != nil || b.State != "completed" || wire.batch.markCalls != 1 {
		t.Fatal(b, err, downloadMonitorEvents(t, f))
	}
}
func TestDownloadMonitorDynamicOwnershipAndOmittedFileAuthority(t *testing.T) {
	f, source, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	wire.batch.opts.labels = []string{"NASTOOL-other"}
	downloadMonitorEnable(t, f, true, false)
	now := time.Now()
	downloadMonitorTick(t, f, now)
	if e := downloadMonitorEvents(t, f)[0]; e.State != "filtered" {
		t.Fatal(e)
	}
	wire.batch.opts.labels = append(wire.batch.opts.labels, "NASTOOL")
	downloadMonitorTick(t, f, now.Add(time.Minute))
	downloadMonitorTick(t, f, now.Add(2*time.Minute))
	b, err := f.runner.organizer.jobs.FindDownloadBatch(context.Background(), "1", completionHash)
	if err != nil || b.State != "completed" {
		t.Fatal(b, err)
	}
	f, source, _, wire = downloadMonitorFixture(t, "qbittorrent", "copy", false)
	organizationFile(t, source, "metadata.nfo")
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(source, "metadata.nfo"), old, old)
	wire.batch.files = append(wire.batch.files, "metadata.nfo")
	downloadMonitorEnable(t, f, true, false)
	downloadMonitorTick(t, f, now)
	if err := f.runner.organizer.downloadMonitorTick(context.Background(), now.Add(time.Minute)); err == nil {
		t.Fatal("omitted files silently authorized")
	}
	if wire.batch.markCalls != 0 || downloadMonitorEvents(t, f)[0].State != "needs_review" {
		t.Fatal(downloadMonitorEvents(t, f))
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, "metadata.nfo"), "original-media")
}
func TestDownloadMonitorCandidateEnumerationFailurePausesRatherThanPretendsEmpty(t *testing.T) {
	f, _, _, wire := downloadMonitorFixture(t, "qbittorrent", "copy", false)
	downloadMonitorEnable(t, f, true, false)
	wire.listError = true
	downloadMonitorTick(t, f, time.Now())
	c, err := f.runner.organizer.jobs.DownloadMonitorControl(context.Background())
	if err != nil || c.State != "paused" {
		t.Fatal(c, err)
	}
	wire.listError = false
	downloadMonitorTick(t, f, time.Now().Add(time.Hour))
	if wire.batch.markCalls != 0 {
		t.Fatal("transient list failure silently reactivated")
	}
}
func TestDownloadMonitorAutomaticPauseRevocationDoesNotStartFiles(t *testing.T) {
	f, source, target, wire := downloadMonitorFixture(t, "transmission", "move", true)
	wire.batch.opts.state = "seeding"
	downloadMonitorEnable(t, f, true, false)
	wire.onPause = func() {
		if err := f.runner.organizer.jobs.PauseDownloadMonitor(context.Background(), "fixture stop during pause acknowledgement"); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	downloadMonitorTick(t, f, now)
	if err := f.runner.organizer.downloadMonitorTick(context.Background(), now.Add(time.Minute)); err == nil {
		t.Fatal("revoked pause continued to files")
	}
	if wire.pauseCalls != 1 || wire.batch.removeCalls != 0 {
		t.Fatal(wire.pauseCalls)
	}
	entries, _ := os.ReadDir(target)
	if len(entries) != 0 {
		t.Fatal(entries)
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, wire.batch.files[0]), "original-media")
}
