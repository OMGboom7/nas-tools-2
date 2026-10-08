package httpserver

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func organizationLinkFixture(t *testing.T, mode string) (*subscriptionRunFixture, string, string, organizationPlanInput, organizationPlan) {
	t.Helper()
	f, source, target, input, _ := organizationCopyFixture(t)
	input.Mode = mode
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	return f, source, target, input, plan
}

func TestOrganizationLinkModesExecuteExactlyAndRemainVisibleToLocalInventory(t *testing.T) {
	for _, mode := range []string{"link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, plan := organizationLinkFixture(t, mode)
			roots := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
			if roots.Code != 200 || !strings.Contains(roots.Body.String(), `"executionModes":["copy","link","softlink","move"]`) {
				t.Fatal(roots.Code, roots.Body.String())
			}
			job := organizationCreateJob(t, f, input, plan)
			if job.Mode != mode || job.State != "ready" {
				t.Fatal(job)
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatal("saving link job wrote files", entries, err)
			}
			status, job, body := organizationJobRequest(t, f, job.ID, "execute")
			if status != 200 || job.State != "completed" {
				t.Fatal(status, body)
			}
			for _, item := range job.Items {
				sourcePath, targetPath := filepath.Join(source, item.Source), filepath.Join(target, item.Target)
				sourceInfo, err := os.Stat(sourcePath)
				if err != nil {
					t.Fatal(err)
				}
				targetInfo, err := os.Lstat(targetPath)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "link" {
					if !targetInfo.Mode().IsRegular() || !os.SameFile(sourceInfo, targetInfo) {
						t.Fatal("hardlink became copy", item)
					}
				} else {
					canonical, _ := filepath.EvalSymlinks(sourcePath)
					link, err := os.Readlink(targetPath)
					if err != nil || link != canonical || targetInfo.Mode()&os.ModeSymlink == 0 {
						t.Fatal(link, err)
					}
				}
				bytes, err := os.ReadFile(targetPath)
				if err != nil || string(bytes) != "original-media" {
					t.Fatal(string(bytes), err)
				}
			}
			history := performFormRequest(f.handler, "/api/v1/organization/history/list", f.token, url.Values{"keyword": {"Movie"}})
			if history.Code != 200 || !strings.Contains(history.Body.String(), `"RMT_MODE":"`+mode+`"`) || !strings.Contains(history.Body.String(), `"MODE":"`+organization.ModeLabel(mode)+`"`) {
				t.Fatal(history.Code, history.Body.String())
			}
			snapshot, err := config.NewStore(f.path).Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			meta, _ := mediameta.Parse("Movie.2026.1080p", "")
			var detail tmdbMediaDetails
			json.Unmarshal([]byte(`{"id":100,"title":"Movie","release_date":"2026-01-01"}`), &detail)
			inventory, err := localMediaInventory(t.Context(), objectValue(snapshot["media"]), meta, detail, mediaserver.Identity{Title: "Movie", Year: "2026", TMDBID: "100"}, nil)
			if err != nil || len(inventory.ItemIDs) == 0 {
				t.Fatal("linked library entry disappeared from local inventory", inventory, err)
			}
			status, _, body = organizationJobRequest(t, f, job.ID, "execute")
			if status != 200 {
				t.Fatal(status, body)
			}
			var count int
			f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
			if count != 1 {
				t.Fatal("duplicate history", count)
			}
		})
	}
}

func TestOrganizationSoftlinkTVUsesVerifiedSeasonAndEpisodeInLibrary(t *testing.T) {
	f, source, target, input := organizationFixture(t)
	input.Mode = "softlink"
	input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	organizationFile(t, source, "Show.S01E02.mkv")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(source, "Show.S01E02.mkv"), old, old)
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	status, job, body = organizationJobRequest(t, f, job.ID, "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
	snapshot, err := config.NewStore(f.path).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := mediameta.Parse("Show.S01", "")
	var detail tmdbMediaDetails
	json.Unmarshal([]byte(`{"id":200,"name":"Show","first_air_date":"2026-01-01","seasons":[{"season_number":1,"episode_count":3}]}`), &detail)
	inventory, err := localMediaInventory(t.Context(), objectValue(snapshot["media"]), meta, detail, mediaserver.Identity{Title: "Show", Year: "2026", TMDBID: "200", TV: true}, nil)
	if err != nil || !inventory.Episodes[1][2] || inventory.Episodes[1][1] || inventory.Episodes[1][3] {
		t.Fatal(inventory, err)
	}
	if err = os.Remove(filepath.Join(source, "Show.S01E02.mkv")); err != nil {
		t.Fatal(err)
	}
	if _, err = localMediaInventory(t.Context(), objectValue(snapshot["media"]), meta, detail, mediaserver.Identity{Title: "Show", Year: "2026", TMDBID: "200", TV: true}, nil); err == nil {
		t.Fatal("dangling softlink still proved a valid TV inventory")
	}
}

func TestOrganizationLinkJobsCannotReuseCopyPreviewOrReserveSameTarget(t *testing.T) {
	f, _, _, input, copyPlan := organizationCopyFixture(t)
	copyJob := organizationCreateJob(t, f, input, copyPlan)
	input.Mode = "link"
	create := func(fingerprint string) int {
		raw, _ := json.Marshal(struct {
			organizationPlanInput
			Fingerprint string `json:"fingerprint"`
		}{input, fingerprint})
		return nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw)).Code
	}
	if status := create(copyPlan.Fingerprint); status != 409 {
		t.Fatal("copy approval authorized link mode", status)
	}
	status, linkPlan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	if status := create(linkPlan.Fingerprint); status != 409 {
		t.Fatal("different modes claimed same target", status)
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	status, _, body = organizationJobRequest(t, f, copyJob.ID, "cancel")
	if status != 200 {
		t.Fatal(status, body)
	}
	linkJob := organizationCreateJob(t, f, input, linkPlan)
	if linkJob.Mode != "link" {
		t.Fatal(linkJob)
	}
}

func TestOrganizationLinkJobsHistoryFailureReconcilesWithoutRelinking(t *testing.T) {
	for _, mode := range []string{"link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input, plan := organizationLinkFixture(t, mode)
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
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(path, path+".saved"); err != nil {
				t.Fatal(err)
			}
			if mode == "softlink" {
				canonical, _ := filepath.EvalSymlinks(filepath.Join(source, media.Source))
				if err = os.Symlink(canonical, path); err != nil {
					t.Fatal(err)
				}
			} else {
				organizationFile(t, target, media.Target)
			}
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
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("reconcile replaced link", err)
			}
			status, job, body = organizationJobRequest(t, f, job.ID, "execute")
			if status != 200 || job.State != "completed" {
				t.Fatal(status, body)
			}
			var count int
			f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE MODE=?`, organization.ModeLabel(mode)).Scan(&count)
			if count != 1 {
				t.Fatal(count)
			}
		})
	}
}

func TestOrganizationLinkJobsConcurrentExecutionHasOneHistoryAndNoSourceDeletion(t *testing.T) {
	for _, mode := range []string{"link", "softlink"} {
		t.Run(mode, func(t *testing.T) {
			f, source, _, input, plan := organizationLinkFixture(t, mode)
			job := organizationCreateJob(t, f, input, plan)
			var wg sync.WaitGroup
			for i := 0; i < 6; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/execute", f.token, `{"confirm":true}`)
					if r.Code != 200 && r.Code != 409 {
						t.Errorf("%d: %s", r.Code, r.Body.String())
					}
				}()
			}
			wg.Wait()
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY WHERE MODE=?`, organization.ModeLabel(mode)).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
			for _, item := range plan.Items {
				if _, err := os.Stat(filepath.Join(source, item.Source)); err != nil {
					t.Fatal("source removed", err)
				}
			}
		})
	}
}
