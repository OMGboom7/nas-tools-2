package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

// All cloud operations are the real CLI against an explicitly isolated alias.
// The production routes are rebound to a factory with an empty environment;
// mock downloaders retain the existing exact-ID/data-preservation assertions.
func remoteAutomationHTTP(t *testing.T, f *subscriptionRunFixture) (*organizationAPI, string, string) {
	t.Helper()
	binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("explicit temporary-alias Rclone binary required")
	}
	root := t.TempDir()
	logical := "/rclone-auto/library"
	physical := filepath.Join(root, "rclone-auto/library")
	if err := os.MkdirAll(physical, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(file, []byte("[NASTOOL]\ntype = alias\nremote = "+root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": logical, "media.tv_path": logical, "media.anime_path": logical}); err != nil {
		t.Fatal(err)
	}
	api := *f.runner.organizer
	api.rclone = remotestore.Factory{ConfigFile: file, Binary: binary, Environment: []string{}}
	previous := f.handler
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/organization/roots", api.serveRoots)
	mux.HandleFunc("POST /api/v1/organization/plan", api.servePlan)
	mux.HandleFunc("POST /api/v1/organization/jobs", api.serveCreateJob)
	mux.HandleFunc("GET /api/v1/organization/jobs", api.serveJobs)
	mux.HandleFunc("GET /api/v1/organization/jobs/{id}", api.serveJob)
	mux.HandleFunc("GET /api/v1/organization/jobs/{id}/source-restore", api.serveSourceRestoreReview)
	mux.HandleFunc("POST /api/v1/organization/jobs/{id}/{action}", api.serveJobAction)
	mux.HandleFunc("POST /api/v1/organization/sync/plan", api.serveDirectSyncPlan)
	mux.HandleFunc("POST /api/v1/organization/sync/jobs", api.serveDirectSyncCreate)
	mux.HandleFunc("GET /api/v1/organization/sync/monitor", api.serveMonitorStatus)
	mux.HandleFunc("POST /api/v1/organization/sync/monitor/review", api.serveMonitorReview)
	mux.HandleFunc("POST /api/v1/organization/sync/monitor/activate", api.serveMonitorActivate)
	mux.HandleFunc("POST /api/v1/organization/sync/monitor/stop", api.serveMonitorStop)
	mux.HandleFunc("POST /api/v1/organization/sync/monitor/dismiss", api.serveMonitorDismiss)
	mux.HandleFunc("POST /api/v1/organization/sync/config/review", api.serveSyncConfigReview)
	mux.HandleFunc("POST /api/v1/organization/sync/config/apply", api.serveSyncConfigApply)
	mux.HandleFunc("POST /api/v1/organization/downloads/completion/review", api.serveDownloadCompletionReview)
	mux.HandleFunc("POST /api/v1/organization/downloads/plan", api.serveDownloadPlan)
	mux.HandleFunc("POST /api/v1/organization/downloads/batches", api.serveDownloadBatchSave)
	mux.HandleFunc("GET /api/v1/organization/downloads/batches", api.serveDownloadBatchFind)
	mux.HandleFunc("GET /api/v1/organization/downloads/batches/{id}", api.serveDownloadBatchGet)
	mux.HandleFunc("POST /api/v1/organization/downloads/batches/{id}/{action}", api.serveDownloadBatchAction)
	mux.HandleFunc("GET /api/v1/organization/downloads/monitor", api.serveDownloadMonitorStatus)
	mux.HandleFunc("POST /api/v1/organization/downloads/monitor/review", api.serveDownloadMonitorReview)
	mux.HandleFunc("POST /api/v1/organization/downloads/monitor/activate", api.serveDownloadMonitorActivate)
	mux.HandleFunc("POST /api/v1/organization/downloads/monitor/stop", api.serveDownloadMonitorStop)
	mux.HandleFunc("POST /api/v1/organization/downloads/monitor/dismiss", api.serveDownloadMonitorDismiss)
	mux.Handle("/", previous)
	f.handler = mux
	f.runner.organizer = &api
	f.runner.scraper.organizer = &api
	f.runner.subtitles.organizer = &api
	return &api, logical, physical
}
func remoteSyncFixture(t *testing.T, mode string, recognized bool) (*subscriptionRunFixture, *organizationAPI, string, string, directSyncInput) {
	t.Helper()
	f, source, _, input := directSyncFixture(t, mode)
	monitorRebuild(t, f)
	api, logical, target := remoteAutomationHTTP(t, f)
	if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET DEST=?,RENAME=? WHERE ID=7`, logical, map[bool]int{false: 0, true: 1}[recognized]); err != nil {
		t.Fatal(err)
	}
	if recognized {
		if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 0}); err != nil {
			t.Fatal(err)
		}
		// Restore only the fixture's metadata transport; raw sync keeps its
		// "no metadata requests" transport unchanged.
		base := newSubscriptionRunFixture(t)
		api.recognition.service.client.Transport = base.transport
		api.service.client.Transport = base.transport
	}
	return f, api, source, target, input
}
func remoteSyncEnable(t *testing.T, f *subscriptionRunFixture, existing bool) organization.MonitorControl {
	t.Helper()
	input := monitorInput{IDs: []string{"7"}, IncludeExisting: existing}
	raw, _ := json.Marshal(input)
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/review", f.token, string(raw))
	var review struct{ Data monitorReview }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &review) != nil || len(review.Data.RemoteTargets) == 0 || review.Data.Lease.Remote == nil {
		t.Fatal(r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), f.runner.organizer.rclone.ConfigFile) || strings.Contains(r.Body.String(), "ConfigDigest\"") {
		t.Fatal("private Rclone binding exposed", r.Body.String())
	}
	input.Fingerprint = review.Data.Fingerprint
	input.Confirm = true
	input.ConfirmOldExecutorsStopped = true
	input.ConfirmSourceRemoval = review.Data.NeedsSourceRemoval
	for _, missing := range []string{"exclusive", "recovery"} {
		bad := input
		bad.ConfirmRemoteExclusive = missing != "exclusive"
		bad.ConfirmRemoteRecoveryCopy = missing != "recovery"
		raw, _ = json.Marshal(bad)
		r = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(raw))
		if r.Code != 400 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	input.ConfirmRemoteExclusive = true
	input.ConfirmRemoteRecoveryCopy = true
	raw, _ = json.Marshal(input)
	r = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(raw))
	var control struct{ Data organization.MonitorControl }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &control) != nil || !control.Data.Lease.Remote.Valid() {
		t.Fatal(r.Code, r.Body.String())
	}
	return control.Data
}
func TestRealRcloneDirectSyncRetainsAllFilePathsAndWritesOnlySyncHistory(t *testing.T) {
	for _, mode := range []string{"rclonecopy", "rclone"} {
		t.Run(mode, func(t *testing.T) {
			f, _, source, target, input := remoteSyncFixture(t, mode, false)
			status, plan, body := directSyncPreview(t, f, input)
			if status != 200 || plan.Remote == nil || len(plan.Items) != 4 {
				t.Fatal(status, body)
			}
			status, j, body := directSyncSave(t, f, input, plan)
			if status != 201 || j.Remote == nil {
				t.Fatal(status, body)
			}
			status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
			if status != 200 || j.State != "completed" {
				t.Fatal(status, body)
			}
			for _, item := range j.Items {
				if item.Source != item.Target || item.Kind != "file" {
					t.Fatal(item)
				}
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
				_, err := os.Lstat(filepath.Join(source, item.Source))
				if organization.MoveMode(mode) && !errors.Is(err, os.ErrNotExist) || !organization.MoveMode(mode) && err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM SYNC_HISTORY`).Scan(&count); err != nil || count != 4 {
				t.Fatal(count, err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='TRANSFER_HISTORY'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("fake media history", count, err)
			}
		})
	}
}
func TestRealRcloneSyncMonitorRunsRawAndRecognizedWithIndependentPersistentGrant(t *testing.T) {
	for _, recognized := range []bool{false, true} {
		for _, mode := range []string{"rclonecopy", "rclone"} {
			t.Run(mode+map[bool]string{false: "-raw", true: "-recognized"}[recognized], func(t *testing.T) {
				f, api, _, target, _ := remoteSyncFixture(t, mode, recognized)
				control := remoteSyncEnable(t, f, true)
				now := time.Now().Add(time.Minute)
				monitorTick(t, api, now)
				monitorTick(t, api, now.Add(31*time.Second))
				events := monitorEvents(t, api)
				completed := 0
				// A tick deliberately processes one event; no re-execution of terminal
				// events is required to drain the original stable batch.
				for i := 0; i < 6; i++ {
					monitorTick(t, api, now.Add(time.Duration(32+i)*time.Second))
				}
				events = monitorEvents(t, api)
				for _, event := range events {
					if event.State == "completed" {
						completed++
						for _, id := range event.Jobs {
							j, err := api.jobs.Get(t.Context(), id)
							if err != nil || j.State != "completed" {
								t.Fatal(j, err)
							}
							for _, item := range j.Items {
								if item.Proof.Remote == nil || item.Proof.Remote.Approval.Generation != control.Lease.Generation || item.Proof.Remote.Approval.Origin != "sync-monitor" {
									t.Fatal("grant not journaled", item)
								}
								if _, err := os.Stat(filepath.Join(target, item.Target)); err != nil {
									t.Fatal(err)
								}
							}
						}
					}
				}
				if completed == 0 {
					t.Fatal("no remote monitor execution", events)
				}
				before, err := api.jobs.List(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				monitorTick(t, api, now.Add(2*time.Minute))
				after, err := api.jobs.List(t.Context())
				if err != nil || len(after) != len(before) {
					t.Fatal("automatic replay", len(before), len(after), err)
				}
			})
		}
	}
}
func TestRealRcloneSyncLeaseStopsOnProfileChangeAndNeverUpgradesAnOldGrant(t *testing.T) {
	for _, change := range []string{"profile", "old-grant"} {
		t.Run(change, func(t *testing.T) {
			f, api, source, target, _ := remoteSyncFixture(t, "rclone", false)
			control := remoteSyncEnable(t, f, true)
			if change == "profile" {
				if err := os.WriteFile(api.rclone.ConfigFile, []byte("[NASTOOL]\ntype=alias\nremote="+t.TempDir()+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				control.Lease.Remote = nil
				raw, _ := json.Marshal(control.Lease)
				if _, err := f.db.Exec(`UPDATE GO_SYNC_MONITOR_CONTROL SET DOCUMENT=?`, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			monitorTick(t, api, time.Now().Add(time.Minute))
			c, err := api.jobs.MonitorControl(t.Context())
			if err != nil || c.State != "paused" {
				t.Fatal(c, err)
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatal(entries, err)
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
		})
	}
}

func TestRealRcloneAutomaticIntakeKeepsWholeBluRayScope(t *testing.T) {
	t.Run("recognized-sync-move", func(t *testing.T) {
		f, api, source, target, _ := remoteSyncFixture(t, "rclone", true)
		for _, name := range []string{"Movie.2026.1080p.mkv", "Movie.2026.1080p.en.srt"} {
			if err := os.Remove(filepath.Join(source, name)); err != nil {
				t.Fatal(err)
			}
		}
		files := organizationDiscTree(t, source, "Movie.2026.1080p")
		remoteSyncEnable(t, f, true)
		now := time.Now()
		monitorTick(t, api, now.Add(time.Minute))
		monitorTick(t, api, now.Add(2*time.Minute))
		events := monitorEvents(t, api)
		if len(events) != 1 || events[0].State != "completed" || len(events[0].Jobs) != 1 {
			t.Fatal(events)
		}
		j, err := api.jobs.Get(t.Context(), events[0].Jobs[0])
		if err != nil || len(j.Items) != 1 || j.Items[0].Disc == nil || j.State != "completed" {
			t.Fatal(j, err)
		}
		for _, name := range files {
			relative, _ := filepath.Rel("Movie.2026.1080p", name)
			assertOrganizationMoveBytes(t, filepath.Join(target, j.Items[0].Target, relative), "original-media")
		}
		if _, err := os.Stat(filepath.Join(source, "Movie.2026.1080p")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	})
	t.Run("selected-download-copy", func(t *testing.T) {
		f, source, _, wire := downloadBatchFixture(t, "qbittorrent", "rclonecopy", false)
		_, _, target := remoteAutomationHTTP(t, f)
		wire.files = organizationDiscTree(t, source, "Movie.2026.1080p")
		b := saveDownloadFixtureBatch(t, f, "qbittorrent")
		status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmRemoteExclusive: true, ConfirmRemoteRecoveryCopy: true, ConfirmOldExecutorsStopped: true})
		if status != 200 || b.State != "files_done" {
			t.Fatal(status, body)
		}
		j, err := f.runner.organizer.jobs.Get(t.Context(), b.Jobs[0])
		if err != nil || len(j.Items) != 1 || j.Items[0].Disc == nil {
			t.Fatal(j, err)
		}
		assertOrganizationMoveBytes(t, filepath.Join(target, j.Items[0].Target, "BDMV/index.bdmv"), "original-media")
		if wire.markCalls+wire.removeCalls != 0 {
			t.Fatal("implicit downloader mutation")
		}
	})
}

func TestRealRcloneDownloadBatchesPreserveMovePolicyAndOneShotResultsAcrossProtocols(t *testing.T) {
	for _, spec := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}} {
		for _, mode := range []string{"rclonecopy", "rclone"} {
			t.Run(spec.kind+mode+map[bool]string{true: "-modern"}[spec.modern], func(t *testing.T) {
				f, source, _, wire := downloadBatchFixture(t, spec.kind, mode, spec.modern)
				_, _, target := remoteAutomationHTTP(t, f)
				if organization.MoveMode(mode) {
					if spec.kind == "qbittorrent" {
						wire.opts.state = "pausedUP"
					}
					if spec.kind == "transmission" {
						wire.opts.state = "stopped"
					}
				}
				organizationFile(t, source, "foreign-task.mkv")
				b := saveDownloadFixtureBatch(t, f, spec.kind)
				if b.Remote == nil || b.Remote.Atomic {
					t.Fatal(b)
				}
				status, _, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
				if status != 409 {
					t.Fatal("implicit remote authorization", status, body)
				}
				if wire.markCalls+wire.removeCalls != 0 {
					t.Fatal("result changed before file execution")
				}
				status, b, body = downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmOldExecutorsStopped: true, ConfirmRemoteExclusive: true, ConfirmRemoteRecoveryCopy: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
				if status != 200 || b.State != "files_done" {
					t.Fatal(status, body)
				}
				j, err := f.runner.organizer.jobs.Get(t.Context(), b.Jobs[0])
				if err != nil || j.State != "completed" {
					t.Fatal(j, err)
				}
				for _, item := range j.Items {
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
					_, err := os.Lstat(filepath.Join(source, item.Source))
					if organization.MoveMode(mode) && !errors.Is(err, os.ErrNotExist) || !organization.MoveMode(mode) && err != nil {
						t.Fatal(err)
					}
				}
				assertOrganizationMoveBytes(t, filepath.Join(source, "foreign-task.mkv"), "original-media")
				status, result, body := downloadResultHTTP(t, f, b)
				if status != 200 {
					t.Fatal(status, body)
				}
				if organization.MoveMode(mode) && result.Operation != "remove-task-keep-all-local-data" {
					t.Fatal("remote move mislabeled as copy", result)
				}
				status, b, body = downloadBatchActionHTTP(t, f, b, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskRemoval: true, ConfirmTaskStopped: true, ConfirmExclusiveLabels: true})
				if status != 200 || b.State != "completed" {
					t.Fatal(status, body)
				}
				if wire.markCalls+wire.removeCalls != 1 {
					t.Fatal("result not one-shot", wire)
				}
				status, _, body = downloadBatchActionHTTP(t, f, b, "reconcile-result", downloadBatchRequest{Confirm: true})
				if status != 200 || wire.markCalls+wire.removeCalls != 1 {
					t.Fatal("result replay", status, body)
				}
				assertOrganizationMoveBytes(t, filepath.Join(source, "foreign-task.mkv"), "original-media")
			})
		}
	}
}
func enableRemoteDownloadMonitor(t *testing.T, f *subscriptionRunFixture) organization.DownloadMonitorControl {
	t.Helper()
	input := downloadMonitorInput{IDs: []string{"1"}, IncludeExisting: true, AllowOmitted: false}
	raw, _ := json.Marshal(input)
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/review", f.token, string(raw))
	var review struct{ Data downloadMonitorReview }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &review) != nil || review.Data.Lease.Remote == nil || len(review.Data.RemoteTargets) == 0 {
		t.Fatal(r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), f.runner.organizer.rclone.ConfigFile) {
		t.Fatal("private profile exposed", r.Body.String())
	}
	input.Fingerprint = review.Data.Fingerprint
	input.Confirm = true
	input.ConfirmOldExecutorsStopped = true
	input.ConfirmResultWrites = true
	input.ConfirmSourceRemoval = review.Data.NeedsSourceRemoval
	input.ConfirmTaskRemoval = review.Data.NeedsTaskRemoval
	input.ConfirmPauseTasks = review.Data.NeedsPauseTasks
	input.ConfirmExclusiveLabels = review.Data.NeedsExclusiveLabels
	for _, missing := range []string{"exclusive", "recovery"} {
		bad := input
		bad.ConfirmRemoteExclusive = missing != "exclusive"
		bad.ConfirmRemoteRecoveryCopy = missing != "recovery"
		raw, _ = json.Marshal(bad)
		r = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/activate", f.token, string(raw))
		if r.Code != 400 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	input.ConfirmRemoteExclusive = true
	input.ConfirmRemoteRecoveryCopy = true
	raw, _ = json.Marshal(input)
	r = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/downloads/monitor/activate", f.token, string(raw))
	var control struct {
		Data organization.DownloadMonitorControl
	}
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &control) != nil || !control.Data.Lease.Remote.Valid() {
		t.Fatal(r.Code, r.Body.String())
	}
	return control.Data
}
func TestRealRcloneDownloadMonitorPauseTransferAndResultAreIndependentlyAuthorized(t *testing.T) {
	for _, spec := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}} {
		for _, mode := range []string{"rclonecopy", "rclone"} {
			t.Run(spec.kind+mode+map[bool]string{true: "-modern"}[spec.modern], func(t *testing.T) {
				f, source, _, wire := downloadMonitorFixture(t, spec.kind, mode, spec.modern)
				_, _, target := remoteAutomationHTTP(t, f)
				if organization.MoveMode(mode) && spec.kind != "aria2" {
					wire.batch.opts.state = "stalledUP"
					if spec.kind == "transmission" {
						wire.batch.opts.state = "seeding"
					}
				}
				control := enableRemoteDownloadMonitor(t, f)
				now := time.Now()
				downloadMonitorTick(t, f, now)
				if wire.pauseCalls+wire.batch.markCalls+wire.batch.removeCalls != 0 {
					t.Fatal("first snapshot performed mutations")
				}
				downloadMonitorTick(t, f, now.Add(31*time.Second))
				b, err := f.runner.organizer.jobs.FindDownloadBatch(t.Context(), "1", downloadTaskID(spec.kind))
				if err != nil || b.State != "completed" {
					t.Fatal(b, err, downloadMonitorEvents(t, f))
				}
				if wire.batch.markCalls+wire.batch.removeCalls != 1 {
					t.Fatal(wire.batch)
				}
				wantPause := 0
				if organization.MoveMode(mode) && spec.kind != "aria2" {
					wantPause = 1
				}
				if wire.pauseCalls != wantPause {
					t.Fatal("missing remote-move pause protection", wire.pauseCalls)
				}
				j, err := f.runner.organizer.jobs.Get(t.Context(), b.Jobs[0])
				if err != nil || j.State != "completed" {
					t.Fatal(j, err)
				}
				for _, item := range j.Items {
					if item.Proof.Remote == nil || item.Proof.Remote.Approval.Generation != control.Lease.Generation || item.Proof.Remote.Approval.Origin != "download-monitor" {
						t.Fatal("missing durable authorization origin", item)
					}
					assertOrganizationMoveBytes(t, filepath.Join(target, item.Target), "original-media")
					if !organization.MoveMode(mode) {
						assertOrganizationMoveBytes(t, filepath.Join(source, item.Source), "original-media")
					}
				}
				downloadMonitorTick(t, f, now.Add(time.Minute))
				if wire.batch.markCalls+wire.batch.removeCalls != 1 || wire.pauseCalls != wantPause {
					t.Fatal("replayed automatic operation")
				}
			})
		}
	}
}
func TestRealRcloneDownloadMonitorLostPauseAndProfileChangeDoNotStartFileWork(t *testing.T) {
	for _, change := range []string{"lost-pause", "profile", "old-grant"} {
		t.Run(change, func(t *testing.T) {
			f, source, _, wire := downloadMonitorFixture(t, "qbittorrent", "rclone", false)
			api, _, target := remoteAutomationHTTP(t, f)
			wire.batch.opts.state = "stalledUP"
			control := enableRemoteDownloadMonitor(t, f)
			now := time.Now()
			downloadMonitorTick(t, f, now)
			if change == "lost-pause" {
				wire.lostPause = true
			} else if change == "profile" {
				if err := os.WriteFile(api.rclone.ConfigFile, []byte("[NASTOOL]\ntype=alias\nremote="+t.TempDir()+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				control.Lease.Remote = nil
				raw, _ := json.Marshal(control.Lease)
				if _, err := f.db.Exec(`UPDATE GO_DOWNLOAD_MONITOR_CONTROL SET DOCUMENT=?`, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			err := api.downloadMonitorTick(context.Background(), now.Add(31*time.Second))
			if change == "lost-pause" && err == nil {
				t.Fatal("lost pause treated as success")
			}
			if change != "lost-pause" {
				c, err := api.jobs.DownloadMonitorControl(t.Context())
				if err != nil || c.State != "paused" {
					t.Fatal(c, err)
				}
			}
			if _, err := api.jobs.FindDownloadBatch(t.Context(), "1", completionHash); !errors.Is(err, organization.ErrNotFound) {
				t.Fatal("unexpected batch", err)
			}
			entries, err := os.ReadDir(target)
			if err != nil || len(entries) != 0 {
				t.Fatal("unexpected remote writes", entries, err)
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, "Movie.2026.1080p.mkv"), "original-media")
			if wire.batch.markCalls+wire.batch.removeCalls != 0 {
				t.Fatal("result changed without file proof")
			}
			_ = api.downloadMonitorTick(context.Background(), now.Add(time.Minute))
			if wire.pauseCalls > 1 {
				t.Fatal("lost pause replayed")
			}
		})
	}
}
