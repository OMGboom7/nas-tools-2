package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func organizationCopyFixture(t *testing.T) (*subscriptionRunFixture, string, string, organizationPlanInput, organizationPlan) {
	t.Helper()
	f, source, target, input := organizationFixture(t)
	for _, name := range []string{"Movie.2026.1080p.mkv", "Movie.2026.1080p.en.srt"} {
		organizationFile(t, source, name)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || len(plan.Fingerprint) != 64 {
		t.Fatal(status, body)
	}
	return f, source, target, input, plan
}

func organizationCreateJob(t *testing.T, f *subscriptionRunFixture, input organizationPlanInput, plan organizationPlan) organization.Job {
	t.Helper()
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, plan.Fingerprint})
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	var payload struct{ Data organization.Job }
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil || r.Code != 201 || payload.Data.ID == "" {
		t.Fatal(r.Code, r.Body.String(), err)
	}
	return payload.Data
}

func organizationJobRequest(t *testing.T, f *subscriptionRunFixture, id, action string) (int, organization.Job, string) {
	t.Helper()
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+id+"/"+action, f.token, `{"confirm":true}`)
	var payload struct{ Data organization.Job }
	if err := json.Unmarshal(r.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return r.Code, payload.Data, r.Body.String()
}

func TestOrganizationCopyJobsPersistPreviewAndExecuteOnlyAfterConfirmation(t *testing.T) {
	f, source, target, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	if job.State != "ready" || len(job.Items) != 2 {
		t.Fatal(job)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal("job creation wrote media", entries, err)
	}
	if repeated := organizationCreateJob(t, f, input, plan); repeated.ID != job.ID {
		t.Fatal("duplicate draft", repeated)
	}
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/execute", f.token, `{"confirm":false}`)
	if r.Code != 400 {
		t.Fatal(r.Code, r.Body.String())
	}
	status, job, body := organizationJobRequest(t, f, job.ID, "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
	for _, item := range job.Items {
		if item.State != "completed" {
			t.Fatal(item)
		}
		for _, path := range []string{filepath.Join(source, item.Source), filepath.Join(target, item.Target)} {
			bytes, err := os.ReadFile(path)
			if err != nil || string(bytes) != "original-media" {
				t.Fatal(path, string(bytes), err)
			}
		}
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE MODE='复制' AND TYPE='电影' AND TITLE='Movie' AND SOURCE='手动整理'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	status, _, body = organizationJobRequest(t, f, job.ID, "execute")
	if status != 200 {
		t.Fatal(status, body)
	}
	f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
	if count != 1 {
		t.Fatal("duplicate history", count)
	}
	for _, methodPath := range []struct{ method, path string }{{"GET", "/api/v1/organization/jobs"}, {"GET", "/api/v1/organization/jobs/" + job.ID}} {
		r = nativeJSONRequest(f.handler, methodPath.method, methodPath.path, f.token, "")
		if r.Code != 200 || !strings.Contains(r.Body.String(), job.ID) || strings.Contains(r.Body.String(), "configDigest") || strings.Contains(r.Body.String(), "stagingIdentity") {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if f.adds != 0 || f.queries != 0 {
		t.Fatal("organizing accessed downloader or indexer")
	}
}

func TestOrganizationCopyJobCreationRejectsStalePreviewAndClientDestinations(t *testing.T) {
	f, source, _, input, plan := organizationCopyFixture(t)
	for _, body := range []string{
		`{"sourceId":"bad","targetId":"bad","mode":"move","fingerprint":"` + plan.Fingerprint + `"}`,
		`{"sourceId":"bad","targetId":"bad","mode":"copy","fingerprint":"` + plan.Fingerprint + `","targetRoot":"/outside"}`,
	} {
		r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, body)
		if r.Code != 400 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if err := os.WriteFile(filepath.Join(source, "Movie.2026.1080p.mkv"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, plan.Fingerprint})
	r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	if r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
}

func TestOrganizationCopyExecutionKeepsChangedSourceAndConflictingTargets(t *testing.T) {
	for _, kind := range []string{"source-changed", "target-conflict", "active-source"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, input, plan := organizationCopyFixture(t)
			job := organizationCreateJob(t, f, input, plan)
			media := plan.itemsMediaForTest(t)
			switch kind {
			case "source-changed":
				os.WriteFile(filepath.Join(source, media.Source), []byte("changed"), 0600)
			case "target-conflict":
				organizationFile(t, target, media.Target)
				os.WriteFile(filepath.Join(target, media.Target), []byte("existing"), 0600)
			case "active-source":
				now := time.Now()
				os.Chtimes(filepath.Join(source, media.Source), now, now)
			}
			status, _, body := organizationJobRequest(t, f, job.ID, "execute")
			if status != 409 {
				t.Fatal(status, body)
			}
			status, _, body = organizationJobRequest(t, f, job.ID, "execute")
			if status != 409 {
				t.Fatal("automatically retried", status, body)
			}
			status, _, body = organizationJobRequest(t, f, job.ID, "cancel")
			if status != 409 {
				t.Fatal("released uncertain claim", status, body)
			}
			if kind == "target-conflict" {
				data, _ := os.ReadFile(filepath.Join(target, media.Target))
				if string(data) != "existing" {
					t.Fatal(string(data))
				}
			}
			if _, err := os.Stat(filepath.Join(source, media.Source)); err != nil {
				t.Fatal("source removed", err)
			}
		})
	}
}
func (plan organizationPlan) itemsMediaForTest(t *testing.T) organizationPlanItem {
	t.Helper()
	for _, item := range plan.Items {
		if item.Kind == "media" {
			return item
		}
	}
	t.Fatal("missing media")
	return organizationPlanItem{}
}

func TestOrganizationCopyConfigurationChangeLeavesDraftCancellable(t *testing.T) {
	f, _, target, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_name_format": "{title}/different"}); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationJobRequest(t, f, job.ID, "execute")
	if status != 409 {
		t.Fatal(status, body)
	}
	status, job, body = organizationJobRequest(t, f, job.ID, "cancel")
	if status != 200 || job.State != "cancelled" {
		t.Fatal(status, body)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestOrganizationCopyJournalFailureAndPositiveReconciliation(t *testing.T) {
	f, _, target, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	if _, err := f.db.Exec(`CREATE TABLE TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT); CREATE TRIGGER deny_history BEFORE INSERT ON TRANSFER_HISTORY BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	status, _, body := organizationJobRequest(t, f, job.ID, "execute")
	if status != 503 {
		t.Fatal(status, body)
	}
	media := plan.itemsMediaForTest(t)
	path := filepath.Join(target, media.Target)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Equal bytes in a different inode are NOT proof of our publication.
	if err = os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, target, media.Target)
	status, job, body = organizationJobRequest(t, f, job.ID, "reconcile")
	if status != 200 || job.State != "needs_review" {
		t.Fatal(status, body)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.Exec(`DROP TRIGGER deny_history`); err != nil {
		t.Fatal(err)
	}
	status, job, body = organizationJobRequest(t, f, job.ID, "reconcile")
	if status != 200 || job.State != "ready" {
		t.Fatal(status, body)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("reconcile recopied file", err)
	}
	status, job, body = organizationJobRequest(t, f, job.ID, "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
}

func TestOrganizationCopyJobsRequireAdministratorAndExclusiveGoMode(t *testing.T) {
	f, _, _, _, plan := organizationCopyFixture(t)
	r := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs", "", "")
	if r.Code != 401 {
		t.Fatal(r.Code)
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"organizer"}, "password": {"strong-password"}, "pris": {"媒体整理"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, f.handler, "organizer", "strong-password")
	for _, endpoint := range []string{"/api/v1/organization/jobs", "/api/v1/organization/jobs/fake/execute"} {
		r = nativeJSONRequest(f.handler, "POST", endpoint, viewer, `{"confirm":true}`)
		if r.Code != 403 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	r = nativeJSONRequest(handler, "GET", "/api/v1/organization/roots", f.token, "")
	var roots struct{ Data organizationRoots }
	json.Unmarshal(r.Body.Bytes(), &roots)
	if r.Code != 200 || len(roots.Data.ExecutionModes) != 0 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = nativeJSONRequest(handler, "POST", "/api/v1/organization/jobs", f.token, `{"mode":"copy","fingerprint":"`+plan.Fingerprint+`"}`)
	if r.Code != 501 {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestOrganizationCopyConcurrentRequestsNeverDuplicatePublicationOrHistory(t *testing.T) {
	f, _, _, input, plan := organizationCopyFixture(t)
	job := organizationCreateJob(t, f, input, plan)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := nativeJSONRequest(f.handler, http.MethodPost, "/api/v1/organization/jobs/"+job.ID+"/execute", f.token, `{"confirm":true}`)
			if r.Code != 200 && r.Code != 409 {
				t.Errorf("status %d: %s", r.Code, r.Body.String())
			}
		}()
	}
	wg.Wait()
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestOrganizationPreviewHonorsMinimumMediaSizeForDirectories(t *testing.T) {
	f, source, _, input := organizationFixture(t)
	organizationFile(t, source, "Movie.2026.1080p.mkv")
	if err := config.NewStore(f.path).Update(map[string]any{"media.min_filesize": 1}); err != nil {
		t.Fatal(err)
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "blocked" || !strings.Contains(plan.Items[0].Reason, "minimum") {
		t.Fatal(status, body)
	}
	input.Path = "Movie.2026.1080p.mkv"
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 || plan.Items[0].Status != "available" {
		t.Fatal("explicit file differs from legacy selection semantics", status, body)
	}
}
