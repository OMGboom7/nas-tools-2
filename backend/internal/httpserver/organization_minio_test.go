package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
	"github.com/0xforee/nas-tools/backend/internal/testutil/s3test"
)

func minioHTTP(t *testing.T, f *subscriptionRunFixture) (*s3test.Server, config.Config) {
	t.Helper()
	// Production handler wiring with an explicitly isolated account and trust.
	// No test is allowed to inherit a workstation alias or credential provider.
	for _, name := range []string{"MC_HOST_NASTOOL", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		t.Setenv(name, "")
	}
	s := s3test.New(t)
	dir := t.TempDir()
	file, ca := filepath.Join(dir, "mc-config.json"), filepath.Join(dir, "ca.pem")
	data, _ := json.Marshal(map[string]any{"version": "10", "aliases": map[string]any{"NASTOOL": map[string]string{"url": s.HTTP.URL, "accessKey": s3test.AccessKey, "secretKey": s3test.SecretKey, "api": "S3v4", "path": "on"}}})
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ca, s.CA, 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": "/bucket/library", "media.tv_path": "/bucket/library", "media.anime_path": "/bucket/library"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ApplicationConfigPath: f.path, DisableLegacy: true, MinioConfigFile: file, MinioCAFile: ca}
	var err error
	f.handler, err = buildHandler(cfg, f.transport, &f.runner)
	if err != nil {
		t.Fatal(err)
	}
	return s, cfg
}
func minioManual(t *testing.T, mode string, disc bool) (*subscriptionRunFixture, *s3test.Server, string, organizationPlanInput, config.Config) {
	t.Helper()
	f, source, _, input := organizationFixture(t)
	input.Mode = mode
	input.TargetID = subscriptionResourceKey("target:/bucket/library:MOV")
	if disc {
		input.Path = "Movie.2026.1080p"
		organizationDiscTree(t, source, input.Path)
	} else {
		for _, name := range []string{"Movie.2026.1080p.mkv", "Movie.2026.1080p.en.srt"} {
			organizationFile(t, source, name)
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, cfg := minioHTTP(t, f)
	return f, s, source, input, cfg
}
func minioAssertJob(t *testing.T, f *subscriptionRunFixture, s *s3test.Server, id, source string) {
	t.Helper()
	j, err := f.runner.organizer.jobs.Get(t.Context(), id)
	if err != nil || j.State != "completed" || j.Remote == nil || j.Remote.Driver != "minio" {
		t.Fatal(j, err)
	}
	for _, item := range j.Items {
		if item.State != "completed" || item.Reason != "" || item.Proof.Remote == nil {
			t.Fatal(item)
		}
		for _, file := range item.Proof.Remote.Files {
			target := item.Target
			if item.Disc != nil {
				target = path.Join(target, file.Path)
			}
			data, ok := s.Get(strings.TrimPrefix(path.Join(j.TargetRoot, target), "/"))
			if !ok || string(data) != "original-media" {
				t.Fatal(target, ok, string(data))
			}
		}
		_, err := os.Lstat(filepath.Join(source, item.Source))
		if organization.MoveMode(j.Mode) && !os.IsNotExist(err) || !organization.MoveMode(j.Mode) && err != nil {
			t.Fatal(item.Source, err)
		}
	}
	for _, r := range s.Requests() {
		if r.Method == "DELETE" && !strings.Contains(r.Query, "uploadId=") {
			t.Fatal("unexpected object deletion", r)
		}
	}
}
func TestMinioNativeManualCopyMoveAndWholeDiscRequireIndependentConsent(t *testing.T) {
	for _, mode := range []string{"miniocopy", "minio"} {
		for _, disc := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-file", true: "-disc"}[disc], func(t *testing.T) {
				f, s, source, input, cfg := minioManual(t, mode, disc)
				status, p, body := organizationPreview(t, f, input)
				if status != 200 || p.Remote == nil || p.Remote.Driver != "minio" || !p.Remote.Directories {
					t.Fatal(status, body)
				}
				if len(s.Keys()) != 0 || strings.Contains(body, cfg.MinioConfigFile) || strings.Contains(body, s3test.SecretKey) {
					t.Fatal("preview wrote or disclosed account", body)
				}
				j := organizationCreateJob(t, f, input, p)
				for _, bad := range []string{`{"confirm":true}`, `{"confirm":true,"confirmSourceRemoval":true,"confirmOldExecutorsStopped":true,"confirmRemoteExclusive":true}`, `{"confirm":true,"confirmSourceRemoval":true,"confirmRemoteRecoveryCopy":true,"confirmRemoteExclusive":true}`} {
					r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/execute", f.token, bad)
					if r.Code != 400 {
						t.Fatal(r.Code, r.Body.String())
					}
				}
				status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
				if status != 200 || j.State != "completed" {
					t.Fatal(status, body)
				}
				minioAssertJob(t, f, s, j.ID, source)
				if disc {
					if data, ok := s.Get("bucket/library/" + j.Items[0].Target + "/CERTIFICATE/empty/"); !ok || len(data) != 0 {
						t.Fatal("lost original empty directory")
					}
				}
				before := len(s.Requests())
				status, _, body = remoteHTTPAction(t, f, j.ID, "reconcile")
				if status != 200 {
					t.Fatal(status, body)
				}
				for _, r := range s.Requests()[before:] {
					if r.Method == "PUT" || r.Method == "POST" || r.Method == "DELETE" {
						t.Fatal("read-only reconciliation replayed transfer", r)
					}
				}
			})
		}
	}
}
func TestMinioLostPublicationAndChangedAccountKeepOriginalReceipts(t *testing.T) {
	for _, change := range []string{"lost-publication", "account"} {
		t.Run(change, func(t *testing.T) {
			f, s, source, input, cfg := minioManual(t, "miniocopy", false)
			status, p, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			j := organizationCreateJob(t, f, input, p)
			if change == "account" {
				data, _ := os.ReadFile(cfg.MinioConfigFile)
				if err := os.WriteFile(cfg.MinioConfigFile, append(data, []byte("\n ")...), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				s.FailNextWrite("bucket/library/Movie (2026)", true, 500)
			}
			status, _, body = remoteHTTPAction(t, f, j.ID, "execute")
			var err error
			j, err = f.runner.organizer.jobs.Get(t.Context(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if status < 400 || j.State == "completed" || strings.Contains(body, s3test.SecretKey) || strings.Contains(body, cfg.MinioConfigFile) {
				t.Fatal(status, body)
			}
			if change == "account" {
				if len(s.Keys()) != 0 {
					t.Fatal("changed profile wrote remote")
				}
				return
			}
			copies := 0
			for _, r := range s.Requests() {
				if r.CopySource != "" {
					copies++
				}
			}
			if copies != 1 {
				t.Fatal("lost publication replay", copies)
			}
			status, j, body = remoteHTTPAction(t, f, j.ID, "resume-publication")
			if status != 200 {
				t.Fatal(status, body)
			}
			if j.State == "completed" || len(j.Items) != 2 || j.Items[0].State != "completed" || j.Items[1].State != "planned" {
				t.Fatal("unstarted companion was silently executed", j)
			}
			status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
			if status != 200 {
				t.Fatal(status, body)
			}
			minioAssertJob(t, f, s, j.ID, source)
			copies = 0
			for _, r := range s.Requests() {
				if r.CopySource != "" {
					copies++
				}
			}
			if copies != len(j.Items) {
				t.Fatal("already present publication copied twice", copies, j.Items)
			}
		})
	}
}
func TestMinioMoveSourceRecoveryWorksAfterTargetAndAccountGoOffline(t *testing.T) {
	f, s, source, input, cfg := minioManual(t, "minio", true)
	status, p, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	public := organizationCreateJob(t, f, input, p)
	api := f.runner.organizer
	j, err := api.jobs.Get(t.Context(), public.ID)
	if err != nil {
		t.Fatal(err)
	}
	api.remoteApproval = organization.RemoteApproval{Actor: "admin", Created: time.Now().UTC().Format(time.RFC3339Nano), Exclusive: true, RecoveryCopy: true}
	c, err := api.minio.Open(j.Definition.Remote)
	if err != nil {
		t.Fatal(err)
	}
	ctx := organization.RemoteContext(t.Context(), c, j.Definition)
	item := j.Items[0]
	proof, err := organization.BuildRemoteProof(ctx, j.Definition, item.Entry, organization.TempName(j.ID, 0), api.remoteApproval)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := api.jobs.Claim(ctx, j.ID, 0)
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := api.jobs.Stage(ctx, j.ID, 0, proof); err != nil {
		t.Fatal(err)
	}
	if err := organization.UploadRemoteStage(ctx, c, j.Definition, item.Entry, proof, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	proof.Incomplete = false
	proof.Remote.StageReady = true
	if err := api.jobs.PrepareRemote(ctx, j.ID, 0, proof); err != nil {
		t.Fatal(err)
	}
	proof.Remote.PublicationIntent = true
	if err := api.jobs.BeginRemotePublish(ctx, j.ID, 0, proof); err != nil {
		t.Fatal(err)
	}
	if err := organization.PublishRemote(ctx, c, j.Definition, item.Entry, proof, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	proof.Remote.TargetVerified = true
	if err := api.jobs.FinishRemotePublish(ctx, j.ID, 0, proof); err != nil {
		t.Fatal(err)
	}
	proof, err = organization.PrepareMove(ctx, j.Definition, item.Entry, proof, organization.MoveHoldName(j.ID, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := api.jobs.BeginMove(ctx, j.ID, 0, proof); err != nil {
		t.Fatal(err)
	}
	if err := organization.ContinueMove(ctx, j.Definition, item.Entry, proof, false, func() error { return api.jobs.MarkQuarantined(ctx, j.ID, 0, proof) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(source, input.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source was not isolated", err)
	}
	c.Close()
	s.Close()
	if err := os.Remove(cfg.MinioConfigFile); err != nil {
		t.Fatal(err)
	}
	j, err = api.jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs/"+j.ID+"/source-restore", f.token, "")
	var review struct {
		Data organization.SourceRestoreReview
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || !review.Data.CanRestore {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, _ := json.Marshal(map[string]any{"confirm": true, "confirmRestoreSource": true, "confirmOldExecutorsStopped": true, "reviewFingerprint": review.Data.Fingerprint, "reason": "restore isolated original"})
	w = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, string(raw))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(source, input.Path, "BDMV/index.bdmv")); err != nil {
		t.Fatal(err)
	}
	j, err = api.jobs.Get(t.Context(), j.ID)
	if err != nil || j.State != organization.SourceRestored {
		t.Fatal(j, err)
	}
}
func TestMinioDirectAndRecognizedSyncKeepOriginalHistoryAndIndependentGrants(t *testing.T) {
	for _, mode := range []string{"miniocopy", "minio"} {
		for _, recognized := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-raw", true: "-recognized"}[recognized], func(t *testing.T) {
				f, source, _, input := directSyncFixture(t, mode)
				if recognized {
					base := newSubscriptionRunFixture(t)
					f.transport = base.transport
					if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 0}); err != nil {
						t.Fatal(err)
					}
				}
				s, _ := minioHTTP(t, f)
				if _, err := f.db.Exec(`UPDATE CONFIG_SYNC_PATHS SET DEST='/bucket/library',RENAME=? WHERE ID=7`, map[bool]int{false: 0, true: 1}[recognized]); err != nil {
					t.Fatal(err)
				}
				if !recognized {
					status, p, body := directSyncPreview(t, f, input)
					if status != 200 || p.Remote == nil || p.Remote.Driver != "minio" {
						t.Fatal(status, body)
					}
					status, j, body := directSyncSave(t, f, input, p)
					if status != 201 {
						t.Fatal(status, body)
					}
					status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
					if status != 200 {
						t.Fatal(status, body)
					}
					minioAssertJob(t, f, s, j.ID, source)
					return
				}
				api := f.runner.organizer
				claims, _ := api.auth.service.VerifyToken(f.token)
				review, failure := api.monitorReview(t.Context(), monitorInput{IDs: []string{"7"}, IncludeExisting: true}, claims.Username)
				if failure != nil {
					t.Fatal(failure)
				}
				if len(review.RemoteTargets) == 0 || review.RemoteTargets[0].Remote.Driver != "minio" {
					t.Fatal(review)
				}
				inputJSON, _ := json.Marshal(monitorInput{IDs: []string{"7"}, IncludeExisting: true, Fingerprint: review.Fingerprint, Confirm: true, ConfirmOldExecutorsStopped: true, ConfirmSourceRemoval: organization.MoveMode(mode), ConfirmRemoteExclusive: true, ConfirmRemoteRecoveryCopy: true})
				w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/sync/monitor/activate", f.token, string(inputJSON))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				now := time.Now().Add(time.Minute)
				monitorTick(t, api, now)
				monitorTick(t, api, now.Add(31*time.Second))
				events := monitorEvents(t, api)
				completed := false
				for _, e := range events {
					for _, id := range e.Jobs {
						minioAssertJob(t, f, s, id, source)
						completed = true
					}
				}
				if !completed {
					t.Fatal(events)
				}
			})
		}
	}
}
func TestMinioDownloadCompletionRunsAllLocalProtocolsWithoutDeletingForeignData(t *testing.T) {
	for _, spec := range []struct {
		kind   string
		modern bool
	}{{"qbittorrent", false}, {"transmission", false}, {"transmission", true}, {"aria2", false}} {
		for _, mode := range []string{"miniocopy", "minio"} {
			t.Run(spec.kind+mode+map[bool]string{false: "", true: "-modern"}[spec.modern], func(t *testing.T) {
				f, source, _, wire := downloadBatchFixture(t, spec.kind, mode, spec.modern)
				s, _ := minioHTTP(t, f)
				if organization.MoveMode(mode) {
					wire.opts.state = "stopped"
					if spec.kind == "qbittorrent" {
						wire.opts.state = "pausedUP"
					}
				}
				organizationFile(t, source, "foreign-task.mkv")
				b := saveDownloadFixtureBatch(t, f, spec.kind)
				status, b, body := downloadBatchActionHTTP(t, f, b, "execute", downloadBatchRequest{Confirm: true, ConfirmOldExecutorsStopped: true, ConfirmRemoteExclusive: true, ConfirmRemoteRecoveryCopy: true, ConfirmSourceRemoval: true, ConfirmTaskStopped: true})
				if status != 200 || b.State != "files_done" {
					t.Fatal(status, body)
				}
				for _, id := range b.Jobs {
					minioAssertJob(t, f, s, id, source)
				}
				status, result, body := downloadResultHTTP(t, f, b)
				if status != 200 {
					t.Fatal(status, body)
				}
				status, b, body = downloadBatchActionHTTP(t, f, b, "finalize", downloadBatchRequest{Confirm: true, Fingerprint: result.Fingerprint, ConfirmTaskRemoval: true, ConfirmTaskStopped: true, ConfirmExclusiveLabels: true})
				if status != 200 || b.State != "completed" || wire.markCalls+wire.removeCalls != 1 {
					t.Fatal(status, body)
				}
				assertOrganizationMoveBytes(t, filepath.Join(source, "foreign-task.mkv"), "original-media")
			})
		}
	}
}

func TestMinioLegacyEntriesCoordinateAllLibrariesAndKeepUnrelatedOriginals(t *testing.T) {
	for _, mode := range []string{"miniocopy", "minio"} {
		for _, entry := range []string{"redo", "history", "rename", "renameudf"} {
			t.Run(mode+"-"+entry, func(t *testing.T) {
				f, source, _, _ := unknownFixture(t, "copy")
				for _, name := range []string{"Show.S01E01.1080p.mkv", "Show.S01E01.1080p.en.srt"} {
					organizationFile(t, source, name)
					old := time.Now().Add(-time.Hour)
					if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
						t.Fatal(err)
					}
				}
				s, _ := minioHTTP(t, f)
				if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": "/bucket/movies", "media.tv_path": "/bucket/tv", "media.anime_path": "/bucket/anime"}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE TRANSFER_UNKNOWN SET DEST='',MODE=? WHERE ID=7`, organization.ModeLabel(mode)); err != nil {
					t.Fatal(err)
				}
				api := f.runner.organizer
				if entry == "history" {
					if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY,MODE TEXT,TYPE TEXT,CATEGORY TEXT,TMDBID INTEGER,TITLE TEXT,YEAR TEXT,SEASON_EPISODE TEXT,SOURCE TEXT,SOURCE_PATH TEXT,SOURCE_FILENAME TEXT,DEST TEXT,DEST_PATH TEXT,DEST_FILENAME TEXT,DATE TEXT);INSERT INTO TRANSFER_HISTORY VALUES(17,?,'电影','',999,'Original','2020','','manual',?,?,'','/original','old.mkv','2020-01-01')`, organization.ModeLabel(mode), filepath.Dir(source), filepath.Base(source)); err != nil {
						t.Fatal(err)
					}
				}
				jobs := []string{}
				if entry == "redo" || entry == "history" {
					kind, id := "unidentification", "7"
					if entry == "history" {
						kind, id = "history", "17"
					}
					input := map[string]any{"flag": kind, "ids": []string{id}}
					w := legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo/plan", input)
					var review struct{ Data legacyRedoReview }
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || review.Data.Items[0].Plan == nil || len(review.Data.Items[0].Plan.Partitions) != 2 {
						t.Fatal(w.Code, w.Body.String())
					}
					input["reviewFingerprint"] = review.Data.Fingerprint
					input["confirmSourceRemoval"] = organization.MoveMode(mode)
					w = legacyCloudJSON(t, f, "/api/v1/organization/unknown/redo", legacyCloudConsent(input))
					var done struct {
						Success bool
						Data    struct{ Results []legacyRedoResult }
					}
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &done) != nil || !done.Success || done.Data.Results[0].BatchID == "" {
						t.Fatal(w.Code, w.Body.String())
					}
					jobs = done.Data.Results[0].Jobs
				} else {
					input := map[string]any{"syncmod": mode, "confirmSourceRemoval": organization.MoveMode(mode)}
					if entry == "rename" {
						input["unknown_id"] = "7"
					} else {
						input["inpath"], input["outpath"] = source, ""
					}
					route := "/api/v1/organization/unknown/" + entry
					w := legacyCloudJSON(t, f, route+"/plan", input)
					var review struct {
						Data struct{ Plan organizationPlan }
					}
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || len(review.Data.Plan.Partitions) != 2 {
						t.Fatal(w.Code, w.Body.String())
					}
					input["reviewFingerprint"] = review.Data.Plan.Fingerprint
					w = legacyCloudJSON(t, f, route, legacyCloudConsent(input))
					var done struct {
						Data struct {
							Jobs  []string
							State string
						}
					}
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &done) != nil || done.Data.State != "completed" {
						t.Fatal(w.Code, w.Body.String())
					}
					jobs = done.Data.Jobs
				}
				if len(jobs) != 2 {
					t.Fatal(jobs)
				}
				for _, id := range jobs {
					minioAssertJob(t, f, s, id, source)
				}
				want := "N"
				if entry == "redo" || entry == "rename" {
					want = "Y"
				}
				if state := unknownState(t, f, 7); state != want {
					t.Fatal("wrong whole-record receipt", state, want)
				}
				if entry == "history" {
					original, err := api.jobs.GetHistory(t.Context(), 17)
					if err != nil || original.Title != "Original" || original.Dest != "" || original.Mode != organization.ModeLabel(mode) {
						t.Fatal(original, err)
					}
				}
			})
		}
	}
}

func TestMinioDownloadMonitorPauseTransferAndWritebackUseFreshProviderGrant(t *testing.T) {
	for _, mode := range []string{"miniocopy", "minio"} {
		t.Run(mode, func(t *testing.T) {
			f, source, _, wire := downloadMonitorFixture(t, "qbittorrent", mode, false)
			s, _ := minioHTTP(t, f)
			if organization.MoveMode(mode) {
				wire.batch.opts.state = "stalledUP"
			}
			input := downloadMonitorInput{IDs: []string{"1"}, IncludeExisting: true}
			w := legacyCloudJSON(t, f, "/api/v1/organization/downloads/monitor/review", input)
			var review struct{ Data downloadMonitorReview }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &review) != nil || len(review.Data.RemoteTargets) == 0 || review.Data.RemoteTargets[0].Remote.Driver != "minio" {
				t.Fatal(w.Code, w.Body.String())
			}
			input.Fingerprint = review.Data.Fingerprint
			input.Confirm = true
			input.ConfirmOldExecutorsStopped = true
			input.ConfirmResultWrites = true
			input.ConfirmPauseTasks = true
			input.ConfirmSourceRemoval = true
			input.ConfirmTaskRemoval = true
			input.ConfirmExclusiveLabels = true
			w = legacyCloudJSON(t, f, "/api/v1/organization/downloads/monitor/activate", input)
			if w.Code != 400 {
				t.Fatal("implicit remote grant", w.Code, w.Body.String())
			}
			input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy = true, true
			w = legacyCloudJSON(t, f, "/api/v1/organization/downloads/monitor/activate", input)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			now := time.Now()
			downloadMonitorTick(t, f, now)
			if wire.pauseCalls+wire.batch.markCalls+wire.batch.removeCalls != 0 {
				t.Fatal("first snapshot wrote")
			}
			downloadMonitorTick(t, f, now.Add(31*time.Second))
			b, err := f.runner.organizer.jobs.FindDownloadBatch(t.Context(), "1", downloadTaskID("qbittorrent"))
			if err != nil || b.State != "completed" {
				t.Fatal(b, err, downloadMonitorEvents(t, f))
			}
			for _, id := range b.Jobs {
				minioAssertJob(t, f, s, id, source)
			}
			wantPause := 0
			if organization.MoveMode(mode) {
				wantPause = 1
			}
			if wire.pauseCalls != wantPause || wire.batch.markCalls+wire.batch.removeCalls != 1 {
				t.Fatal("missing pause/result guard", wire)
			}
			downloadMonitorTick(t, f, now.Add(time.Minute))
			if wire.pauseCalls != wantPause || wire.batch.markCalls+wire.batch.removeCalls != 1 {
				t.Fatal("automatic action replay", wire)
			}
		})
	}
}

func TestMinioIntakeScraperAndSubtitleQueueUseTheNativeRemoteEvidenceOnce(t *testing.T) {
	f, s, _, input, _ := minioManual(t, "minio", false)
	if err := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": true}); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"movie":{"basic":true}}}`); err != nil {
		t.Fatal(err)
	}
	lookups, submissions := 0, 0
	base := f.transport
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
			lookups++
			return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","overview":"Native MinIO NFO"}`), nil
		}
		if r.URL.Host == "csf.test" {
			submissions++
			nfo := false
			for _, key := range s.Keys() {
				nfo = nfo || strings.HasSuffix(key, ".nfo")
			}
			if !nfo {
				t.Fatal("subtitle before NFO")
			}
			return jsonResponse(r, `{"job_id":"csf-minio"}`), nil
		}
		return base.RoundTrip(r)
	})
	f.runner.scraper.transport, f.runner.subtitles.transport = transport, transport
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/ChineseSubFinder/install", f.token, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = nativeJSONRequest(f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", f.token, `{"values":{"host":"https://csf.test","api_key":"fixture-csf-key"}}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	status, p, body := organizationPreview(t, f, input)
	if status != 200 || !p.ScraperEnabled {
		t.Fatal(status, body)
	}
	j := organizationCreateJob(t, f, input, p)
	status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
	if status != 200 || j.State != "completed" {
		t.Fatal(status, body)
	}
	if err := f.runner.subtitles.tick(t.Context()); err != nil || submissions != 0 {
		t.Fatal(err, submissions)
	}
	if err := f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := f.runner.organizer.jobs.Scrapes(t.Context(), false)
	if err != nil || len(rows) != 1 || rows[0].State != "completed" {
		t.Fatal(rows, err)
	}
	if err := f.runner.subtitles.tick(t.Context()); err != nil || submissions != 1 {
		t.Fatal(err, submissions)
	}
	if err := f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.subtitles.tick(t.Context()); err != nil || submissions != 1 || lookups != 1 {
		t.Fatal("enrichment replay", err, submissions, lookups)
	}
}
