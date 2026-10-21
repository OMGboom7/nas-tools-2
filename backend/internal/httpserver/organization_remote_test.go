package httpserver

import (
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
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

const remoteConfirmJSON = `{"confirm":true,"confirmSourceRemoval":true,"confirmRemoteExclusive":true,"confirmRemoteRecoveryCopy":true,"confirmOldExecutorsStopped":true}`

func remoteHTTPFixture(t *testing.T, mode string, disc bool) (*subscriptionRunFixture, string, string, organizationPlanInput, string) {
	t.Helper()
	binary := os.Getenv("NASTOOL_TEST_RCLONE_BINARY")
	if binary == "" {
		t.Skip("explicit temporary-alias Rclone binary required")
	}
	f, source, _, input := organizationFixture(t)
	root := t.TempDir()
	logical := "/rclone-test/movies"
	target := filepath.Join(root, "rclone-test/movies")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(file, []byte("[NASTOOL]\ntype = alias\nremote = "+root+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": logical}); err != nil {
		t.Fatal(err)
	}
	api := *f.runner.organizer
	api.rclone = remotestore.Factory{ConfigFile: file, Binary: binary, Environment: []string{}}
	// Use the production handlers with an explicitly empty child environment,
	// guaranteeing no workstation credentials or backend overrides are loaded.
	previous := f.handler
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/organization/roots", api.serveRoots)
	mux.HandleFunc("POST /api/v1/organization/plan", api.servePlan)
	mux.HandleFunc("POST /api/v1/organization/jobs", api.serveCreateJob)
	mux.HandleFunc("GET /api/v1/organization/jobs", api.serveJobs)
	mux.HandleFunc("GET /api/v1/organization/jobs/{id}", api.serveJob)
	mux.HandleFunc("GET /api/v1/organization/jobs/{id}/source-restore", api.serveSourceRestoreReview)
	mux.HandleFunc("POST /api/v1/organization/jobs/{id}/{action}", api.serveJobAction)
	mux.Handle("/", previous)
	f.handler = mux
	f.runner.organizer = &api
	f.runner.scraper.organizer = &api
	f.runner.subtitles.organizer = &api
	input.Mode = mode
	input.TargetID = subscriptionResourceKey("target:" + logical + ":MOV")
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
	return f, source, target, input, file
}
func remoteHTTPAction(t *testing.T, f *subscriptionRunFixture, id, action string) (int, organization.Job, string) {
	t.Helper()
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+id+"/"+action, f.token, remoteConfirmJSON)
	var payload struct{ Data organization.Job }
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return r.Code, payload.Data, r.Body.String()
}
func TestRealRcloneManualAPIRequiresDistinctConsentAndCompletesCopyMoveAndDisc(t *testing.T) {
	for _, mode := range []string{"rclonecopy", "rclone"} {
		for _, disc := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "-disc", false: "-file"}[disc], func(t *testing.T) {
				f, source, target, input, configFile := remoteHTTPFixture(t, mode, disc)
				status, plan, body := organizationPreview(t, f, input)
				if status != 200 || plan.Remote == nil || plan.Remote.Atomic || !plan.Remote.RecoveryCopy || len(plan.Fingerprint) != 64 {
					t.Fatal(status, body)
				}
				if strings.Contains(body, configFile) || strings.Contains(body, "ConfigDigest") {
					t.Fatal("private binding exposed", body)
				}
				j := organizationCreateJob(t, f, input, plan)
				for _, confirm := range []string{`{"confirm":true}`, `{"confirm":true,"confirmSourceRemoval":true,"confirmRemoteExclusive":true,"confirmOldExecutorsStopped":true}`, `{"confirm":true,"confirmSourceRemoval":true,"confirmRemoteRecoveryCopy":true,"confirmOldExecutorsStopped":true}`, `{"confirm":true,"confirmSourceRemoval":true,"confirmRemoteRecoveryCopy":true,"confirmRemoteExclusive":true}`} {
					r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/execute", f.token, confirm)
					if r.Code != 400 {
						t.Fatal(r.Code, r.Body.String())
					}
				}
				if mode == "rclone" {
					r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/execute", f.token, strings.Replace(remoteConfirmJSON, `"confirmSourceRemoval":true`, `"confirmSourceRemoval":false`, 1))
					if r.Code != 400 {
						t.Fatal(r.Code, r.Body.String())
					}
				}
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 {
					t.Fatal("consent check wrote data", entries, err)
				}
				status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
				if status != 200 || j.State != "completed" {
					t.Fatal(status, body)
				}
				for _, item := range j.Items {
					if _, err := os.Stat(filepath.Join(target, item.Target)); err != nil {
						t.Fatal(err)
					}
					_, err := os.Lstat(filepath.Join(source, item.Source))
					if mode == "rclone" && !errors.Is(err, os.ErrNotExist) || mode == "rclonecopy" && err != nil {
						t.Fatal("wrong source disposition", err)
					}
				}
				var n int
				var historyMode string
				if err := f.db.QueryRow(`SELECT COUNT(*),MIN(MODE) FROM TRANSFER_HISTORY`).Scan(&n, &historyMode); err != nil || n != 1 || historyMode != organization.ModeLabel(mode) {
					t.Fatal(n, historyMode, err)
				}
				status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
				if status != 200 || j.State != "completed" {
					t.Fatal(status, body)
				}
				f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&n)
				if n != 1 {
					t.Fatal("history replay", n)
				}
				if f.adds != 0 || f.queries != 0 {
					t.Fatal("touched downloader", f.adds, f.queries)
				}
			})
		}
	}
}
func remoteHTTPPrepared(t *testing.T, mode string, publicationIntent bool) (*subscriptionRunFixture, string, string, organization.Job, organization.Proof) {
	t.Helper()
	f, source, target, input, _ := remoteHTTPFixture(t, mode, false)
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	j := organizationCreateJob(t, f, input, plan)
	api := f.runner.organizer
	j, err := api.jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := api.rclone.Open(j.Definition.Remote)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if ok, err := api.jobs.Claim(t.Context(), j.ID, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	p, err := organization.BuildRemoteProof(t.Context(), j.Definition, j.Items[0].Entry, organization.TempName(j.ID, 0), organization.RemoteApproval{Actor: "admin", Created: time.Now().UTC().Format(time.RFC3339Nano), Exclusive: true, RecoveryCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.jobs.Stage(t.Context(), j.ID, 0, p); err != nil {
		t.Fatal(err)
	}
	if err := organization.UploadRemoteStage(t.Context(), c, j.Definition, j.Items[0].Entry, p, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	p.Incomplete = false
	p.Remote.StageReady = true
	if err := api.jobs.PrepareRemote(t.Context(), j.ID, 0, p); err != nil {
		t.Fatal(err)
	}
	if publicationIntent {
		p.Remote.PublicationIntent = true
		if err := api.jobs.BeginRemotePublish(t.Context(), j.ID, 0, p); err != nil {
			t.Fatal(err)
		}
	}
	j, err = api.jobs.Get(t.Context(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, source, target, j, p
}
func TestRealRcloneExplicitPublicationRecoveryAndReadOnlyReconciliationNeverUploadAgain(t *testing.T) {
	for _, action := range []string{"resume-publication", "reconcile"} {
		t.Run(action, func(t *testing.T) {
			f, source, target, j, p := remoteHTTPPrepared(t, "rclonecopy", true)
			if action == "reconcile" {
				c, err := f.runner.organizer.rclone.Open(j.Definition.Remote)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if err := organization.PublishRemote(t.Context(), c, j.Definition, j.Items[0].Entry, p, func() error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			// The original upload has a complete durable recovery copy. Recovery
			// reads only that copy and must not re-open a changed original as input.
			if err := os.WriteFile(filepath.Join(source, j.Items[0].Source), []byte("a new source object"), 0600); err != nil {
				t.Fatal(err)
			}
			status, updated, body := remoteHTTPAction(t, f, j.ID, action)
			if status != 200 || updated.Items[0].State != "completed" || updated.Items[1].State != "planned" {
				t.Fatal(status, body)
			}
			assertOrganizationMoveBytes(t, filepath.Join(target, j.Items[0].Target), "original-media")
			assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "a new source object")
			if _, err := os.Stat(filepath.Join(target, j.Items[1].Target)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("started unprepared companion", err)
			}
		})
	}
}
func TestRealRcloneConflictOrLostStagingNeverAdoptsExistingDataOrRepeatsUpload(t *testing.T) {
	for _, kind := range []string{"pre-intent-existing", "changed-final", "lost-staging", "changed-profile"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, j, p := remoteHTTPPrepared(t, "rclonecopy", kind != "pre-intent-existing")
			switch kind {
			case "pre-intent-existing", "changed-final":
				organizationFile(t, target, j.Items[0].Target)
				if kind == "changed-final" {
					if err := os.WriteFile(filepath.Join(target, j.Items[0].Target), []byte("different data"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "lost-staging":
				if err := os.Rename(filepath.Join(target, p.Remote.Stage), filepath.Join(target, p.Remote.Stage+".unmounted")); err != nil {
					t.Fatal(err)
				}
			case "changed-profile":
				if err := os.WriteFile(j.Definition.Remote.ConfigFile, []byte("[NASTOOL]\ntype = alias\nremote = "+t.TempDir()+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			status, _, body := remoteHTTPAction(t, f, j.ID, "resume-publication")
			if status == 200 {
				t.Fatal("unsafe recovery succeeded", body)
			}
			if j := organizationReadMoveJob(t, f, j.ID); j.Items[0].State == "completed" {
				t.Fatal("invented completion", j)
			}
			assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
			if kind == "changed-final" {
				assertOrganizationMoveBytes(t, filepath.Join(target, j.Items[0].Target), "different data")
			}
		})
	}
}
func TestRealRcloneSourceRestoreWorksWithRemoteOfflineAndCloseNeverDeletesRemoteEvidence(t *testing.T) {
	f, source, target, j, p := remoteHTTPPrepared(t, "rclone", true)
	api := f.runner.organizer
	c, err := api.rclone.Open(j.Definition.Remote)
	if err != nil {
		t.Fatal(err)
	}
	ctx := organization.RemoteContext(t.Context(), c, j.Definition)
	if err := organization.PublishRemote(ctx, c, j.Definition, j.Items[0].Entry, p, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	p.Remote.TargetVerified = true
	if err := api.jobs.FinishRemotePublish(ctx, j.ID, 0, p); err != nil {
		t.Fatal(err)
	}
	p, err = organization.PrepareMove(ctx, j.Definition, j.Items[0].Entry, p, organization.MoveHoldName(j.ID, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := api.jobs.BeginMove(ctx, j.ID, 0, p); err != nil {
		t.Fatal(err)
	}
	if err := organization.ContinueMove(ctx, j.Definition, j.Items[0].Entry, p, false, func() error { return api.jobs.MarkQuarantined(ctx, j.ID, 0, p) }); err != nil {
		t.Fatal(err)
	}
	c.Close()
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/close-remote", f.token, `{"confirm":true,"confirmOldExecutorsStopped":true,"reason":"stop"}`)
	if r.Code != 409 {
		t.Fatal("closed isolated source without restoring it", r.Code, r.Body.String())
	}
	if err := os.Rename(target, target+".offline"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(j.Definition.Remote.ConfigFile, j.Definition.Remote.ConfigFile+".offline"); err != nil {
		t.Fatal(err)
	}
	r = nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs/"+j.ID+"/source-restore", f.token, "")
	var review struct {
		Data organization.SourceRestoreReview
	}
	if json.Unmarshal(r.Body.Bytes(), &review) != nil || r.Code != 200 || !review.Data.CanRestore {
		t.Fatal(r.Code, r.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"confirm": true, "confirmRestoreSource": true, "confirmOldExecutorsStopped": true, "reviewFingerprint": review.Data.Fingerprint, "reason": "restore original"})
	r = nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+j.ID+"/restore-source", f.token, string(body))
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	assertOrganizationMoveBytes(t, filepath.Join(source, j.Items[0].Source), "original-media")
	if _, err := os.Stat(filepath.Join(target+".offline", p.Remote.Stage, "manifest.json")); err != nil {
		t.Fatal("deleted retained recovery copy", err)
	}
	if stored := organizationReadMoveJob(t, f, j.ID); stored.State != organization.SourceRestored {
		t.Fatal(stored)
	}
}

func TestRealRcloneIntakeNFOAndSubtitleWorkersUseRemoteEvidenceAndNeverReplay(t *testing.T) {
	f, _, target, input, _ := remoteHTTPFixture(t, "rclone", false)
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
			return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","overview":"Rclone NFO"}`), nil
		}
		if r.URL.Host == "csf.test" {
			submissions++
			matches, _ := filepath.Glob(filepath.Join(target, "Movie (2026)", "*.nfo"))
			if len(matches) != 1 {
				t.Fatal("subtitle dispatched without remote NFO", matches)
			}
			return jsonResponse(r, `{"job_id":"csf-remote"}`), nil
		}
		return base.RoundTrip(r)
	})
	f.runner.scraper.transport = transport
	f.runner.subtitles.transport = transport
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/ChineseSubFinder/install", f.token, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = nativeJSONRequest(f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", f.token, `{"values":{"host":"https://csf.test","api_key":"private-csf-key"}}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || !plan.ScraperEnabled {
		t.Fatal(status, body)
	}
	j := organizationCreateJob(t, f, input, plan)
	status, j, body = remoteHTTPAction(t, f, j.ID, "execute")
	if status != 200 || j.State != "completed" {
		t.Fatal(status, body)
	}
	if err := f.runner.subtitles.tick(t.Context()); err != nil || submissions != 0 {
		t.Fatal("outbox order", err, submissions)
	}
	if err := f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := f.runner.organizer.jobs.Scrapes(t.Context(), false)
	if err != nil || len(rows) != 1 || rows[0].State != "completed" || len(rows[0].Results) != 1 || rows[0].Results[0].State != "created" {
		t.Fatal(rows, err)
	}
	if err := f.runner.subtitles.tick(t.Context()); err != nil || submissions != 1 {
		t.Fatal("remote subtitle evidence", err, submissions)
	}
	if err := f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.runner.subtitles.tick(t.Context()); err != nil || submissions != 1 || lookups != 1 {
		t.Fatal("replayed enrichment", err, submissions, lookups)
	}
}
