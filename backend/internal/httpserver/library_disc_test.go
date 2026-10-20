package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

func libraryDiscCreate(t *testing.T, x *libraryHTTPFixture, p libraryPreview) organization.LibraryJob {
	t.Helper()
	raw, _ := json.Marshal(struct {
		libraryPlanInput
		Fingerprint string `json:"fingerprint"`
		Confirm     bool   `json:"confirm"`
		Stopped     bool   `json:"confirmOldExecutorsStopped"`
		Overwrite   bool   `json:"confirmOverwrite"`
	}{p.Input, p.Fingerprint, true, true, true})
	w := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs", x.f.token, string(raw))
	var body struct{ Data organization.LibraryJob }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return body.Data
}
func TestLibraryBluRayAllModesKeepWholeIntakeAndRestoreMetadataIndependently(t *testing.T) {
	for _, transferMode := range []string{"copy", "link", "softlink", "move"} {
		for _, scrapeMode := range []string{"no_force", "force_nfo", "force_all"} {
			t.Run(transferMode+scrapeMode, func(t *testing.T) {
				x := newLibraryHTTPFixture(t, false)
				organizationDiscTree(t, x.source, "Movie.2026.1080p")
				input := organizationPlanInput{SourceID: subscriptionResourceKey("source:" + x.source + ":"), TargetID: x.input.RootID, Path: "Movie.2026.1080p", Mode: transferMode}
				status, plan, body := organizationPreview(t, x.f, input)
				if status != 200 {
					t.Fatal(status, body)
				}
				intake := organizationCreateJob(t, x.f, input, plan)
				status, intake, body = organizationMoveRequest(t, x.f, intake.ID, "execute")
				if status != 200 || intake.State != "completed" {
					t.Fatal(status, body)
				}
				disc := intake.Items[0].Target
				nfo := filepath.Join(disc, filepath.Base(disc)+".nfo")
				poster := filepath.Join(disc, "poster.png")
				old := `<movie><uniqueid type="tmdb">101</uniqueid></movie>`
				if transferMode == "softlink" {
					sourceNFO := filepath.Join(x.source, "old.nfo")
					if err := os.WriteFile(sourceNFO, []byte(old), 0600); err != nil {
						t.Fatal(err)
					}
					// Native intake links use canonical approved roots. Do not
					// introduce the macOS /var directory alias into this fixture.
					sourceNFO, err := filepath.EvalSymlinks(sourceNFO)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(sourceNFO, filepath.Join(x.root, nfo)); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(filepath.Join(x.root, nfo), []byte(old), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(x.root, poster), []byte("old art"), 0600); err != nil {
					t.Fatal(err)
				}
				x.input.Mode = scrapeMode
				x.input.Path = disc
				p := x.preview(t)
				if p.Total != 1 || p.Blocked != 0 || p.Sample[0].Disc == nil || p.Sample[0].Disc.Files != 7 {
					t.Fatal(p)
				}
				job := libraryDiscCreate(t, x, p)
				if err := x.f.runner.library.tick(t.Context()); err != nil {
					t.Fatal(err)
				}
				job = x.job(t, job.ID)
				if job.State != "completed" || job.Done != 1 {
					items, _ := x.f.runner.organizer.jobs.LibraryItems(t.Context(), job.ID, 1)
					t.Fatal(job.State, job.Done, items)
				}
				rows, err := x.f.runner.organizer.jobs.LibraryOutputs(t.Context(), job.ID, 1)
				if err != nil || len(rows) != 2 {
					t.Fatal(rows, err)
				}
				for _, out := range rows {
					if strings.Contains(out.Target, "BDMV") || strings.Contains(out.Target, "STREAM") {
						t.Fatal("metadata written among clips", out)
					}
				}
				data, err := os.ReadFile(filepath.Join(x.root, nfo))
				if err != nil {
					t.Fatal(err)
				}
				if scrapeMode == "no_force" {
					if string(data) != old {
						t.Fatal("old NFO overwritten", string(data))
					}
				} else if !strings.Contains(string(data), "<tmdbid>100</tmdbid>") {
					t.Fatal("forced NFO reused old identity", string(data))
				}
				for _, file := range []string{"BDMV/index.bdmv", "BDMV/STREAM/00000.m2ts", "BDMV/STREAM/00001.m2ts", "CERTIFICATE/id.bdmv"} {
					assertOrganizationMoveBytes(t, filepath.Join(x.root, disc, file), "original-media")
				}
				var count int
				if err := x.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); err != nil || count != 1 {
					t.Fatal("library scrape invented intake history", count, err)
				}
				for _, out := range rows {
					if out.Target == nfo && out.State == "replaced" {
						w := nativeJSONRequest(x.f.handler, "GET", libraryEndpoint+"/outputs/"+out.ID, x.f.token, "")
						if w.Code != 200 || !strings.Contains(w.Body.String(), `"canRestore":true`) {
							t.Fatal(w.Code, w.Body.String())
						}
						w = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/outputs/"+out.ID+"/restore", x.f.token, libraryOutputBody(out))
						if w.Code != 200 {
							t.Fatal(w.Code, w.Body.String())
						}
						b, err := os.ReadFile(filepath.Join(x.root, nfo))
						if err != nil || string(b) != old {
							t.Fatal("restored wrong metadata", string(b), err)
						}
					}
				}
				before := x.lookups.Load()
				monitorRebuild(t, x.f)
				if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != before {
					t.Fatal("library job replayed after restart", err)
				}
			})
		}
	}
}
func TestLibraryBluRayTVSeasonHasNoInventedEpisodeMapping(t *testing.T) {
	for _, mode := range []string{"no_force", "force_nfo", "force_all"} {
		t.Run(mode, func(t *testing.T) {
			x := newLibraryHTTPFixture(t, true)
			organizationDiscTree(t, x.root, "Show (2026)/Season 01")
			x.input.Mode = mode
			if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"tv":{"basic":true,"season_basic":true,"episode_basic":true}},"scraper_pic":{"tv":{"episode_thumb":true,"episode_thumb_ffmpeg":true}}}`); err != nil {
				t.Fatal(err)
			}
			decoded := false
			x.f.runner.scraper.thumbnail = func(context.Context, *os.File) ([]byte, error) { decoded = true; return nil, context.Canceled }
			p := x.preview(t)
			if p.Total != 1 || p.Blocked != 0 || p.Sample[0].Disc == nil {
				t.Fatal(p)
			}
			job := libraryDiscCreate(t, x, p)
			if err := x.f.runner.library.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			job = x.job(t, job.ID)
			if job.State != "partial" || job.Blocked != 1 || decoded {
				t.Fatal(job, decoded)
			}
			for _, name := range []string{"Show (2026)/tvshow.nfo", "Show (2026)/Season 01/season.nfo"} {
				b, err := os.ReadFile(filepath.Join(x.root, name))
				if err != nil || len(b) == 0 {
					t.Fatal(name, string(b), err)
				}
			}
			rows, err := x.f.runner.organizer.jobs.LibraryOutputs(t.Context(), job.ID, 1)
			if err != nil || len(rows) != 2 {
				t.Fatal(rows, err)
			}
			for _, out := range rows {
				if strings.Contains(out.Target, "-thumb") || strings.Contains(out.Target, "BDMV") {
					t.Fatal(out)
				}
			}
		})
	}
}
func TestLibraryBluRayExpansionAndNewDataRequireFreshReview(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	disc := "Movie.2026.1080p"
	organizationDiscTree(t, x.root, disc)
	x.input.Path = disc + "/BDMV/STREAM/00000.m2ts"
	raw, _ := json.Marshal(x.input)
	w := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/preview", x.f.token, string(raw))
	if w.Code != 409 {
		t.Fatal("inner selection expanded silently", w.Code, w.Body.String())
	}
	x.input.AllowDiscExpansion = true
	p := x.preview(t)
	if p.Total != 1 || p.Sample[0].Path != disc || !p.Sample[0].Disc.Expanded {
		t.Fatal(p)
	}
	organizationFile(t, x.root, disc+"/new.dat")
	body := map[string]any{"rootId": p.Input.RootID, "path": p.Input.Path, "mode": p.Input.Mode, "allowDiscExpansion": true, "excludes": p.Input.Excludes, "requestId": p.Input.RequestID, "fingerprint": p.Fingerprint, "confirm": true, "confirmOldExecutorsStopped": true}
	raw, _ = json.Marshal(body)
	w = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs", x.f.token, string(raw))
	if w.Code != 409 {
		t.Fatal("new disc payload ignored", w.Code, w.Body.String())
	}
	p = x.preview(t)
	job := libraryDiscCreate(t, x, p)
	detail := nativeJSONRequest(x.f.handler, "GET", libraryEndpoint+"/jobs/"+job.ID, x.f.token, "")
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `"expanded":true`) || strings.Contains(detail.Body.String(), "BDMV/index.bdmv") {
		t.Fatal("private inventory exposed or expanded scope lost", detail.Code, detail.Body.String())
	}
}

func TestLibraryBluRayTVWithoutVerifiedSeasonDoesNotWriteEpisodeMetadata(t *testing.T) {
	for _, path := range []string{"Show (2026)", "Show (2026)/Season 02"} {
		t.Run(path, func(t *testing.T) {
			x := newLibraryHTTPFixture(t, true)
			organizationDiscTree(t, x.root, path)
			job := libraryDiscCreate(t, x, x.preview(t))
			if err := x.f.runner.library.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			job = x.job(t, job.ID)
			if job.State != "partial" || job.Done != 0 || job.Blocked != 1 {
				t.Fatal("unverified season invented", job)
			}
			rows, err := x.f.runner.organizer.jobs.LibraryOutputs(t.Context(), job.ID, 1)
			if err != nil || len(rows) != 0 {
				t.Fatal("wrote metadata without a verified season", rows, err)
			}
		})
	}
}

func TestLibraryBluRayAutomaticPluginDoesNotBorrowOrdinaryEpisodeDefaultSeason(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationDiscTree(t, x.root, "Show.E01")
	libraryPluginInstall(t, x)
	c := libraryPluginSave(t, x, libraryPluginValues(x, true, "", "force_nfo"))
	libraryPluginTick(t, x, c.OnceAt)
	c = libraryPluginControlRead(t, x)
	if c.State != "queued" || len(c.Jobs) != 1 {
		t.Fatal(c)
	}
	if err := x.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	job := x.job(t, c.Jobs[0])
	if job.State != "partial" || job.Blocked != 1 {
		t.Fatal("disc adopted the ordinary file default season", job)
	}
	outputs, err := x.f.runner.organizer.jobs.LibraryOutputs(t.Context(), job.ID, 1)
	if err != nil || len(outputs) != 0 {
		t.Fatal(outputs, err)
	}
}
func TestLibraryBluRayScheduledOnceAndCronFreezeOneDiscPerCycle(t *testing.T) {
	for _, once := range []bool{true, false} {
		t.Run(map[bool]string{true: "once", false: "cron"}[once], func(t *testing.T) {
			x := newLibraryHTTPFixture(t, false)
			organizationDiscTree(t, x.root, "Movie.2026.1080p")
			libraryPluginInstall(t, x)
			cron := ""
			if !once {
				cron = "* * * * *"
			}
			c := libraryPluginSave(t, x, libraryPluginValues(x, once, cron, "force_nfo"))
			due := c.OnceAt
			if !once {
				due = c.Next
			}
			libraryPluginTick(t, x, due)
			c = libraryPluginControlRead(t, x)
			if c.State != "queued" || len(c.Jobs) != 1 {
				t.Fatal(c)
			}
			job := x.job(t, c.Jobs[0])
			if job.Total != 1 || job.Definition.Schedule == nil {
				t.Fatal(job)
			}
			if err := x.f.runner.library.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if job = x.job(t, job.ID); job.State != "completed" {
				t.Fatal(job)
			}
			libraryPluginTick(t, x, due.Add(time.Second))
			c = libraryPluginControlRead(t, x)
			if once && c.State != "idle" || !once && (c.State != "scheduled" || c.Next.IsZero()) {
				t.Fatal(c)
			}
			if !once {
				libraryPluginTick(t, x, c.Next)
				c = libraryPluginControlRead(t, x)
				if c.State != "queued" || len(c.Jobs) != 1 {
					t.Fatal(c)
				}
				if err := x.f.runner.library.tick(t.Context()); err != nil {
					t.Fatal(err)
				}
				if job = x.job(t, c.Jobs[0]); job.State != "completed" {
					t.Fatal("retained metadata backup broke next disc cycle", job)
				}
			}
		})
	}
}
func TestLibraryBluRayLegacyEventAndAjaxUseWholeDiscSnapshot(t *testing.T) {
	for _, event := range []bool{false, true} {
		t.Run(map[bool]string{true: "event", false: "ajax"}[event], func(t *testing.T) {
			x := newLibraryHTTPFixture(t, false)
			organizationDiscTree(t, x.root, "Movie.2026.1080p")
			endpoint := "/do"
			if event {
				libraryPluginInstall(t, x)
				endpoint = libraryEndpoint + "/event"
			}
			in := libraryTriggerInput{Path: filepath.Join(x.root, "Movie.2026.1080p"), Review: true}
			if event {
				in.Path = filepath.Join(in.Path, "BDMV/STREAM/00000.m2ts")
				w := nativeJSONRequest(x.f.handler, "POST", endpoint, x.f.token, libraryTriggerBody(t, event, in))
				if w.Code != 409 {
					t.Fatal("event expanded its inner selection without approval", w.Code, w.Body.String())
				}
				in.AllowDiscExpansion = true
			}
			w := nativeJSONRequest(x.f.handler, "POST", endpoint, x.f.token, libraryTriggerBody(t, event, in))
			var p struct{ Data libraryPreview }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Data.Total != 1 || p.Data.Sample[0].Disc == nil {
				t.Fatal(w.Code, w.Body.String())
			}
			in.RequestID, in.Fingerprint = p.Data.Input.RequestID, p.Data.Fingerprint
			in.Review = false
			in.Confirm, in.ConfirmOldExecutorsStopped, in.ConfirmOverwrite = true, true, true
			w = nativeJSONRequest(x.f.handler, "POST", endpoint, x.f.token, libraryTriggerBody(t, event, in))
			var job struct{ Data organization.LibraryJob }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil {
				t.Fatal(w.Code, w.Body.String())
			}
			if err := x.f.runner.library.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if j := x.job(t, job.Data.ID); j.State != "completed" || j.Done != 1 {
				t.Fatal(j)
			}
		})
	}
}
func TestLibraryBluRayChangedOrInterruptedSourcesDoNotReplayWrites(t *testing.T) {
	for _, scenario := range []string{"changed-payload", "revoked-source", "lost-result", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			x := newLibraryHTTPFixture(t, false)
			organizationDiscTree(t, x.root, "Movie.2026.1080p")
			job := libraryDiscCreate(t, x, x.preview(t))
			switch scenario {
			case "changed-payload":
				organizationFile(t, x.root, "Movie.2026.1080p/new.m2ts")
			case "revoked-source":
				if _, err := x.f.db.Exec(`UPDATE DOWNLOADER SET DOWNLOAD_DIR='[]'`); err != nil {
					t.Fatal(err)
				}
			case "lost-result":
				if _, err := x.f.db.Exec(`CREATE TRIGGER lose_disc_result BEFORE UPDATE ON GO_LIBRARY_SCRAPE_OUTPUTS WHEN NEW.STATE='created' BEGIN SELECT RAISE(ABORT,'lost result'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cancelled" {
				entered := make(chan struct{})
				base := x.f.runner.scraper.transport
				var once sync.Once
				x.f.runner.scraper.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
						once.Do(func() { close(entered) })
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return base.RoundTrip(r)
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- x.f.runner.library.tick(ctx) }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					cancel()
					t.Fatal("disc worker did not start")
				}
				w := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs/"+job.ID+"/cancel", x.f.token, `{"confirm":true}`)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				x.f.runner.scraper.transport = base
			} else if err := x.f.runner.library.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			job = x.job(t, job.ID)
			if job.State == "completed" || job.State == "ready" || job.State == "running" {
				t.Fatal("failure claimed success", job)
			}
			before := x.lookups.Load()
			monitorRebuild(t, x.f)
			if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != before {
				t.Fatal("interrupted disc write replayed", err)
			}
		})
	}
}
