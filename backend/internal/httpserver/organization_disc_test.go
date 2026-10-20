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
	"github.com/0xforee/nas-tools/backend/internal/scraper"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

func organizationDiscTree(t *testing.T, source, path string) []string {
	t.Helper()
	files := []string{}
	for _, name := range []string{"BDMV/index.bdmv", "BDMV/MovieObject.bdmv", "BDMV/STREAM/00000.m2ts", "BDMV/STREAM/00001.m2ts", "BDMV/PLAYLIST/00000.mpls", "CERTIFICATE/id.bdmv", "readme.txt"} {
		relative := filepath.Join(path, name)
		organizationFile(t, source, relative)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(filepath.Join(source, relative), old, old); err != nil {
			t.Fatal(err)
		}
		files = append(files, relative)
	}
	if err := os.MkdirAll(filepath.Join(source, path, "CERTIFICATE/empty"), 0700); err != nil {
		t.Fatal(err)
	}
	return files
}
func organizationDiscFixture(t *testing.T, mode string) (*subscriptionRunFixture, string, string, organizationPlanInput) {
	t.Helper()
	f, source, target, input := organizationFixture(t)
	input.Mode = mode
	input.Path = "Movie.2026.1080p"
	organizationDiscTree(t, source, input.Path)
	return f, source, target, input
}
func TestOrganizationBluRayAllLocalModesPublishOnceWithOneDirectoryHistory(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input := organizationDiscFixture(t, mode)
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 || len(plan.Items) != 1 || plan.Items[0].Disc == nil || plan.Items[0].Status != "available" || plan.Items[0].Target != "Movie (2026)" {
				t.Fatal(status, body)
			}
			item := plan.Items[0]
			if item.Disc.Files != 7 || item.Size != 7*14 || item.Disc.Expanded {
				t.Fatal(item)
			}
			job := organizationCreateJob(t, f, input, plan)
			if job.State != "ready" || len(job.Items) != 1 || job.Items[0].Disc == nil {
				t.Fatal(job)
			}
			if files, err := os.ReadDir(target); err != nil || len(files) != 0 {
				t.Fatal("draft wrote media", files, err)
			}
			if mode == "move" {
				w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs/"+job.ID+"/execute", f.token, `{"confirm":true}`)
				if w.Code != 400 {
					t.Fatal("move missing independent source consent", w.Code, w.Body.String())
				}
			}
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" || done.Items[0].Reason != "" {
				t.Fatal(status, body)
			}
			for _, name := range []string{"BDMV/index.bdmv", "BDMV/STREAM/00000.m2ts", "BDMV/STREAM/00001.m2ts", "BDMV/PLAYLIST/00000.mpls", "CERTIFICATE/id.bdmv", "readme.txt"} {
				assertOrganizationMoveBytes(t, filepath.Join(target, item.Target, name), "original-media")
			}
			if mode == "move" {
				if _, err := os.Lstat(filepath.Join(source, input.Path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("disc source retained", err)
				}
			} else {
				assertOrganizationMoveBytes(t, filepath.Join(source, input.Path, "BDMV/index.bdmv"), "original-media")
			}
			for _, action := range []string{"execute", "reconcile"} {
				status, _, body = organizationMoveRequest(t, f, job.ID, action)
				if status != 200 {
					t.Fatal(action, status, body)
				}
			}
			var count int
			var from, filename, destname string
			canonicalSource, err := filepath.EvalSymlinks(source)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.QueryRow(`SELECT COUNT(*),SOURCE_PATH,SOURCE_FILENAME,DEST_FILENAME FROM TRANSFER_HISTORY`).Scan(&count, &from, &filename, &destname); err != nil || count != 1 || filepath.Join(from, filename) != filepath.Join(canonicalSource, input.Path) || destname != "Movie (2026)" {
				t.Fatal("wrong whole-disc history", count, from, filename, destname, err)
			}
			w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/jobs/"+job.ID, f.token, "")
			if w.Code != 200 || strings.Contains(w.Body.String(), "BDMV/index.bdmv") || strings.Contains(w.Body.String(), "WitnessDirectories") {
				t.Fatal("private manifest exposed", w.Code, w.Body.String())
			}
		})
	}
}
func TestOrganizationBluRayInnerSelectionRequiresExplicitScopeAndFreshInventory(t *testing.T) {
	f, source, _, input := organizationDiscFixture(t, "copy")
	input.Path += "/BDMV/STREAM/00000.m2ts"
	status, _, body := organizationPreview(t, f, input)
	if status != 422 {
		t.Fatal("silent scope expansion", status, body)
	}
	input.AllowDiscExpansion = true
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || len(plan.Items) != 1 || !plan.Items[0].Disc.Expanded {
		t.Fatal(status, body)
	}
	organizationFile(t, source, "Movie.2026.1080p/new-unreviewed.txt")
	raw, _ := json.Marshal(struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}{input, plan.Fingerprint})
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
	if w.Code != 409 {
		t.Fatal("expanded job ignored new file", w.Code, w.Body.String())
	}
}
func TestOrganizationBluRayUnknownWholeScopeResolvesOnlyAfterNativeCompletion(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			f, source, target, input := organizationDiscFixture(t, mode)
			path := filepath.Join(source, input.Path)
			if _, err := f.db.Exec(`CREATE TABLE TRANSFER_UNKNOWN(ID INTEGER PRIMARY KEY,PATH TEXT,DEST TEXT,MODE TEXT,STATE TEXT);INSERT INTO TRANSFER_UNKNOWN VALUES(7,?,?,?,'N')`, path, target, organization.ModeLabel(mode)); err != nil {
				t.Fatal(err)
			}
			record := organization.UnknownRecord{ID: 7, Path: path, Dest: target, Mode: organization.ModeLabel(mode), State: "N"}
			input.UnknownRecord = &organizationUnknownSelector{ID: 7, Fingerprint: organization.Digest(record)}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			job := organizationCreateJob(t, f, input, plan)
			if unknownState(t, f, 7) != "N" {
				t.Fatal("draft changed unknown")
			}
			status, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 || done.State != "completed" || done.UnknownState != "resolved" || unknownState(t, f, 7) != "Y" {
				t.Fatal(status, body)
			}
		})
	}
}
func TestOrganizationBluRayNativeNFOAndSubtitlePayloadKeepDirectoryIdentity(t *testing.T) {
	for _, tv := range []bool{false, true} {
		for _, mode := range []string{"copy", "link", "softlink", "move"} {
			t.Run(mode+map[bool]string{true: "tv", false: "movie"}[tv], func(t *testing.T) {
				f, source, target, input := organizationDiscFixture(t, mode)
				if tv {
					input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
					input.Path = "Show.2026.S01"
					organizationDiscTree(t, source, input.Path)
				}
				if err := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": true}); err != nil {
					t.Fatal(err)
				}
				base := f.transport
				calls := 0
				f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Hostname() == "disc-csf.test" {
						calls++
						var payload subtitles.Payload
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || !payload.Bluray || !strings.HasPrefix(payload.Path, "/csf/") || filepath.Base(payload.Path) != filepath.Base(filepath.Dir(payload.Path))+".mp4" || tv != (payload.VideoType == 1) {
							t.Fatal(payload, err)
						}
						return jsonResponse(r, `{"job_id":"disc-subtitle-job"}`), nil
					}
					if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
						if tv {
							return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01"}`), nil
						}
						return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01"}`), nil
					}
					if r.URL.Path == "/3/tv/200/season/1" {
						return jsonResponse(r, `{"id":201,"season_number":1,"name":"Season 1","episodes":[{"id":202,"show_id":200,"season_number":1,"episode_number":1,"name":"First"}]}`), nil
					}
					return base.RoundTrip(r)
				})
				api := monitorRebuild(t, f)
				if err := f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"movie":{"basic":true},"tv":{"basic":true,"season_basic":true,"episode_basic":true}},"scraper_pic":{"tv":{"episode_thumb":true,"episode_thumb_ffmpeg":true}}}`); err != nil {
					t.Fatal(err)
				}
				w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/ChineseSubFinder/install", f.token, "")
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				canonical, _ := filepath.EvalSymlinks(target)
				raw, _ := json.Marshal(pluginConfigRequest{Values: map[string]any{"host": "http://disc-csf.test", "api_key": "disc-key", "local_path": canonical, "remote_path": "/csf"}})
				w = nativeJSONRequest(f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", f.token, string(raw))
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				status, plan, body := organizationPreview(t, f, input)
				if status != 200 || len(plan.Items) != 1 || plan.Items[0].Status != "available" {
					t.Fatal(status, body)
				}
				if tv {
					file := plan.Items[0]
					season := 1
					input.Assignments = []organizationMediaAssignment{{Source: file.Source, Identity: file.Identity, Modified: file.Modified, Size: file.Size, Kind: "tv", TMDBID: "200", Season: &season}}
					status, plan, body = organizationPreview(t, f, input)
					if status != 200 || plan.Items[0].Status != "available" {
						t.Fatal("season-only disc identity rejected", status, body)
					}
				}
				job := organizationCreateJob(t, f, input, plan)
				status, job, body = organizationMoveRequest(t, f, job.ID, "execute")
				if status != 200 || job.State != "completed" {
					t.Fatal(status, body)
				}
				if err := f.runner.subtitles.tick(t.Context()); err != nil || calls != 0 {
					t.Fatal("subtitle ran before scraper receipt", calls, err)
				}
				if err := f.runner.scraper.tick(t.Context()); err != nil {
					t.Fatal(err)
				}
				receipts, err := api.jobs.Scrapes(t.Context(), false)
				wanted := "completed"
				if tv {
					wanted = "partial"
				}
				if err != nil || len(receipts) != 1 || receipts[0].State != wanted {
					t.Fatal(receipts, err)
				}
				discDir := filepath.Join(target, job.Items[0].Target)
				nfo := filepath.Join(discDir, filepath.Base(discDir)+".nfo")
				if tv {
					nfo = filepath.Join(discDir, "season.nfo")
					if _, err := os.Stat(filepath.Join(filepath.Dir(discDir), "tvshow.nfo")); err != nil {
						t.Fatal(err)
					}
					for _, out := range receipts[0].Results {
						if strings.HasSuffix(out.Name, "-thumb.jpg") {
							t.Fatal("decoded arbitrary Blu-ray clip", out)
						}
					}
				}
				if _, err := os.Stat(nfo); err != nil {
					t.Fatal("metadata not written at disc level", nfo, err)
				}
				if err := f.runner.subtitles.tick(t.Context()); err != nil || calls != 1 {
					t.Fatal("native Blu-ray subtitle not dispatched", calls, err)
				}
				if err := f.runner.subtitles.tick(t.Context()); err != nil || calls != 1 {
					t.Fatal("subtitle request replayed", calls, err)
				}
				stored, err := api.jobs.Get(t.Context(), job.ID)
				if err != nil || organization.VerifyCompletedTarget(t.Context(), stored, stored.Items[0]) != nil {
					t.Fatal("sidecars broke original directory proof", err)
				}
			})
		}
	}
}
func TestOrganizationBluRaySyncMonitorAndDownloaderKeepWholeScope(t *testing.T) {
	t.Run("recognized-monitor", func(t *testing.T) {
		f, source, target, _ := organizationDiscFixture(t, "copy")
		if _, err := f.db.Exec(`CREATE TABLE CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER);INSERT INTO CONFIG_SYNC_PATHS VALUES(7,?,?,'','复制',1,1,1,NULL,1)`, source, target); err != nil {
			t.Fatal(err)
		}
		api := monitorRebuild(t, f)
		monitorEnable(t, f, true)
		now := time.Now()
		monitorTick(t, api, now.Add(time.Minute))
		monitorTick(t, api, now.Add(2*time.Minute))
		events := monitorEvents(t, api)
		if len(events) != 1 || len(events[0].Snapshot.Files) != 1 || events[0].Snapshot.Files[0].DiscInfo == nil || len(events[0].Jobs) != 1 {
			t.Fatal(events)
		}
		job, err := api.jobs.Get(t.Context(), events[0].Jobs[0])
		if err != nil || job.State != "completed" || len(job.Items) != 1 || job.Items[0].Disc == nil {
			t.Fatal(job, err)
		}
	})
	for _, kind := range []string{"qbittorrent", "transmission", "aria2"} {
		t.Run(kind, func(t *testing.T) {
			f, source, target, wire := downloadBatchFixture(t, kind, "copy", false)
			wire.files = organizationDiscTree(t, source, "Movie.2026.1080p")
			// Explicitly advertised hidden ancillary data is part of the disc,
			// unlike an arbitrary hidden folder elsewhere in a shared download.
			hidden := "Movie.2026.1080p/.release-info"
			organizationFile(t, source, hidden)
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(source, hidden), old, old); err != nil {
				t.Fatal(err)
			}
			wire.files = append(wire.files, hidden)
			status, review, body := downloadPlanHTTP(t, f, kind)
			if status != 200 || len(review.Plans) != 1 || len(review.Omitted) != 0 || len(review.Plans[0].Items) != 1 || review.Plans[0].Items[0].Disc == nil {
				t.Fatal(status, body)
			}
			status, batch, body := downloadBatchSaveHTTP(t, f, kind, review, false)
			if status != 201 || len(batch.Jobs) != 1 {
				t.Fatal(status, body)
			}
			status, batch, body = downloadBatchActionHTTP(t, f, batch, "execute", downloadBatchRequest{Confirm: true})
			if status != 200 || batch.State != "files_done" {
				t.Fatal(status, body)
			}
			assertOrganizationMoveBytes(t, filepath.Join(target, "Movie (2026)/BDMV/index.bdmv"), "original-media")
			assertOrganizationMoveBytes(t, filepath.Join(target, "Movie (2026)/.release-info"), "original-media")
			if wire.markCalls != 0 || wire.removeCalls != 0 {
				t.Fatal("intake silently changed remote task")
			}
		})
	}
}
