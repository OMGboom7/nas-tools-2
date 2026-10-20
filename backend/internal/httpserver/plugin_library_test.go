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
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

const libraryPluginEndpoint = "/api/v1/plugins/LibraryScraper"

func libraryPluginInstall(t *testing.T, x *libraryHTTPFixture) {
	t.Helper()
	r := nativeJSONRequest(x.f.handler, "POST", libraryPluginEndpoint+"/install", x.f.token, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
}
func libraryPluginDetail(t *testing.T, x *libraryHTTPFixture) pluginConfigDetail {
	t.Helper()
	r := nativeJSONRequest(x.f.handler, "GET", libraryPluginEndpoint, x.f.token, "")
	var p struct{ Data pluginConfigDetail }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &p) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return p.Data
}
func libraryPluginSave(t *testing.T, x *libraryHTTPFixture, values map[string]any, clear ...string) libraryPluginControl {
	t.Helper()
	detail := libraryPluginDetail(t, x)
	body, _ := json.Marshal(pluginConfigRequest{Fingerprint: detail.Meta.Fingerprint, Values: values, ClearConfig: clear})
	r := nativeJSONRequest(x.f.handler, "PUT", libraryPluginEndpoint, x.f.token, string(body))
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	return libraryPluginControlRead(t, x)
}
func libraryPluginControlRead(t *testing.T, x *libraryHTTPFixture) libraryPluginControl {
	t.Helper()
	raw, err := x.f.runner.scraper.system.Get(t.Context(), libraryControlKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := decodeLibraryControl(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func libraryPluginValues(x *libraryHTTPFixture, once bool, cron, mode string) map[string]any {
	return map[string]any{"scraper_path": []string{x.input.RootID}, "mode": mode, "onlyonce": once, "cron": cron, "go_confirm": true, "go_old_stopped": true, "go_overwrite": true}
}
func libraryPluginTick(t *testing.T, x *libraryHTTPFixture, now time.Time) {
	t.Helper()
	if err := x.f.runner.library.pluginTick(t.Context(), now); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryPluginOnlyOnceConsumptionAutoKindsAndRestartNoReplay(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationFile(t, x.root, "Movie/Movie.2026.mkv")
	organizationFile(t, x.root, "Show/Season 01/Show.S01E02.mkv")
	libraryPluginInstall(t, x)
	c := libraryPluginSave(t, x, libraryPluginValues(x, true, "", "no_force"))
	if !c.Authorized || c.OnceAt.IsZero() || !c.Next.IsZero() {
		t.Fatal(c)
	}
	raw, err := x.f.runner.scraper.system.Get(t.Context(), libraryPluginKey)
	if err != nil || !strings.Contains(raw, `"onlyonce":false`) || strings.Contains(raw, "go_confirm") {
		t.Fatal(raw, err)
	}
	libraryPluginTick(t, x, c.OnceAt.Add(-time.Nanosecond))
	jobs, _ := x.f.runner.organizer.jobs.LibraryJobs(t.Context(), false)
	if len(jobs) != 0 || x.lookups.Load() != 0 {
		t.Fatal("once fired early", jobs)
	}
	libraryPluginTick(t, x, c.OnceAt)
	c = libraryPluginControlRead(t, x)
	if c.State != "queued" || len(c.Jobs) != 1 || !c.OnceAt.IsZero() {
		t.Fatal(c)
	}
	job := x.job(t, c.Jobs[0])
	if job.Total != 2 || job.Definition.Root.Kind != "auto" || job.Definition.Schedule == nil {
		t.Fatal(job)
	}
	if err := x.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if job = x.job(t, job.ID); job.State != "completed" || job.Done != 2 {
		t.Fatal(job)
	}
	for _, name := range []string{"Movie/Movie.2026.nfo", "Movie/poster.png", "Show/tvshow.nfo", "Show/Season 01/season.nfo", "Show/Season 01/Show.S01E02.nfo"} {
		if _, err := os.Stat(filepath.Join(x.root, name)); err != nil {
			t.Fatal(name, err)
		}
	}
	libraryPluginTick(t, x, time.Now().Add(time.Hour))
	c = libraryPluginControlRead(t, x)
	if c.State != "idle" {
		t.Fatal(c)
	}
	before := x.lookups.Load()
	monitorRebuild(t, x.f)
	libraryPluginTick(t, x, time.Now().Add(24*time.Hour))
	if err := x.f.runner.library.tick(t.Context()); err != nil || before != x.lookups.Load() {
		t.Fatal("once replayed after restart", err)
	}
	runs, err := x.f.runner.organizer.jobs.LibraryPluginRuns(t.Context())
	if err != nil || len(runs) != 1 || runs[0].State != "completed" {
		t.Fatal(runs, err)
	}
}
func TestLibraryPluginCronFutureMediaExclusionsAndCoalescedCycles(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	libraryPluginInstall(t, x)
	values := libraryPluginValues(x, false, "* * * * *", "force_nfo")
	values["exclude_path"] = filepath.Join(x.root, "skip")
	c := libraryPluginSave(t, x, values)
	if c.Next.IsZero() || !c.OnceAt.IsZero() {
		t.Fatal(c)
	}
	for _, name := range []string{"skip/Movie.2026.mkv", "skip-too/Movie.2026.mkv"} {
		organizationFile(t, x.root, name)
	}
	libraryPluginTick(t, x, c.Next)
	c = libraryPluginControlRead(t, x)
	if len(c.Jobs) != 1 {
		t.Fatal(c)
	}
	first := x.job(t, c.Jobs[0])
	if first.Total != 1 {
		t.Fatal(first)
	}
	if err := x.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(x.root, "skip/Movie.2026.nfo")); !os.IsNotExist(err) {
		t.Fatal("excluded directory scraped", err)
	}
	organizationFile(t, x.root, "Later/Movie.2026.mkv")
	late := c.Next.Add(24 * time.Hour)
	libraryPluginTick(t, x, late)
	c = libraryPluginControlRead(t, x)
	if !c.Next.After(late) || len(c.Jobs) != 1 || c.Jobs[0] == first.ID {
		t.Fatal("cron backlog or stale worklist reused", c)
	}
	second := x.job(t, c.Jobs[0])
	if second.Total != 2 {
		t.Fatal(second)
	}
	if err := x.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	libraryPluginTick(t, x, late)
	runs, err := x.f.runner.organizer.jobs.LibraryPluginRuns(t.Context())
	if err != nil || len(runs) != 2 {
		t.Fatal("missed cycles replayed", runs, err)
	}
	// APScheduler Monday=0 and day/weekday AND, not Unix cron OR.
	zone, _ := rssWorkerLocation()
	s, err := parseRSSSchedule("0 8 1 * mon", zone)
	if err != nil {
		t.Fatal(err)
	}
	next := s.next(time.Date(2026, 10, 2, 0, 0, 0, 0, zone))
	if next.Day() != 1 || next.Weekday() != time.Monday {
		t.Fatal(next)
	}
}
func TestLibraryPluginLegacyConfigNotAutoAdoptedAndSafeLifecycleReads(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	libraryPluginInstall(t, x)
	legacy := map[string]any{"cron": "* * * * *", "mode": "force_all", "onlyonce": true, "scraper_path": []string{x.root}, "unknown_cache": "private-legacy-cache"}
	b, _ := json.Marshal(legacy)
	if err := x.f.runner.scraper.system.Set(t.Context(), libraryPluginKey, string(b)); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, x.root, "Movie.2026.mkv")
	libraryPluginTick(t, x, time.Now().Add(24*time.Hour))
	jobs, _ := x.f.runner.organizer.jobs.LibraryJobs(t.Context(), false)
	if len(jobs) != 0 {
		t.Fatal("legacy settings silently adopted", jobs)
	}
	for _, path := range []string{"/api/v1/plugins", libraryPluginEndpoint, libraryPluginEndpoint + "/page"} {
		r := nativeJSONRequest(x.f.handler, "GET", path, x.f.token, "")
		if r.Code != 200 || strings.Contains(r.Body.String(), "private-legacy-cache") || strings.Contains(r.Body.String(), "private-server-key") {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/plugin/list", "/api/v1/plugin/status", "/api/v1/plugin/page"} {
		r := performFormRequest(x.f.handler, path, x.f.token, url.Values{"id": {libraryPluginID}})
		if r.Code != 200 || strings.Contains(r.Body.String(), "private-legacy-cache") {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	r := performFormRequest(x.f.handler, "/api/v1/plugin/config", x.f.token, url.Values{"id": {libraryPluginID}, "config": {string(b)}})
	if r.Code != 400 {
		t.Fatal("force configuration accepted without consent", r.Code, r.Body.String())
	}
	form := url.Values{"id": {libraryPluginID}, "config": {string(b)}, "confirm": {"true"}, "confirmOldExecutorsStopped": {"true"}, "confirmOverwrite": {"true"}, "fingerprint": {libraryPluginDetail(t, x).Meta.Fingerprint}}
	r = performFormRequest(x.f.handler, "/api/v1/plugin/config", x.f.token, form)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	c := libraryPluginControlRead(t, x)
	if c.OnceAt.IsZero() || c.Next.IsZero() {
		t.Fatal(c)
	}
	stored, _ := x.f.runner.scraper.system.Get(t.Context(), libraryPluginKey)
	if !strings.Contains(stored, "private-legacy-cache") || !strings.Contains(stored, `"onlyonce":false`) {
		t.Fatal(stored)
	}
	r = performFormRequest(x.f.handler, "/api/v1/plugin/config", x.f.token, form)
	if r.Code != 409 || !libraryPluginControlRead(t, x).OnceAt.Equal(c.OnceAt) {
		t.Fatal("legacy lost-response retry rearmed once", r.Code, r.Body.String())
	}
	legacy["scraper_path"] = []string{x.input.RootID}
	b, _ = json.Marshal(legacy)
	form.Set("config", string(b))
	form.Set("fingerprint", libraryPluginDetail(t, x).Meta.Fingerprint)
	r = performFormRequest(x.f.handler, "/api/v1/plugin/config", x.f.token, form)
	if r.Code != 200 {
		t.Fatal("legacy root IDs not accepted", r.Code, r.Body.String())
	}
}
func TestLibraryPluginChangesStopJobsAndReinstallDoesNotRestoreAuthorization(t *testing.T) {
	for _, action := range []string{"stop", "uninstall", "options", "source", "application"} {
		t.Run(action, func(t *testing.T) {
			x := newLibraryHTTPFixture(t, false)
			organizationFile(t, x.root, "Movie.2026.mkv")
			libraryPluginInstall(t, x)
			c := libraryPluginSave(t, x, libraryPluginValues(x, true, "* * * * *", "no_force"))
			libraryPluginTick(t, x, c.OnceAt)
			c = libraryPluginControlRead(t, x)
			id := c.Jobs[0]
			switch action {
			case "stop":
				libraryPluginSave(t, x, map[string]any{"scraper_path": []string{x.input.RootID}, "go_stop": true})
			case "uninstall":
				r := nativeJSONRequest(x.f.handler, "DELETE", libraryPluginEndpoint, x.f.token, "")
				if r.Code != 200 {
					t.Fatal(r.Code, r.Body.String())
				}
				libraryPluginInstall(t, x)
			case "options":
				if err := x.f.runner.scraper.system.Set(t.Context(), "UserScraperConf", `{}`); err != nil {
					t.Fatal(err)
				}
				libraryPluginTick(t, x, c.Next)
			case "source":
				if _, err := x.f.db.Exec(`UPDATE DOWNLOADER SET DOWNLOAD_DIR='[]'`); err != nil {
					t.Fatal(err)
				}
				libraryPluginTick(t, x, c.Next)
			case "application":
				if err := config.NewStore(x.f.path).Update(map[string]any{"media.tmdb_language": "en"}); err != nil {
					t.Fatal(err)
				}
				libraryPluginTick(t, x, c.Next)
			}
			job := x.job(t, id)
			if job.State != "cancelled" {
				t.Fatal("authorization change left job live", job)
			}
			if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != 0 {
				t.Fatal("revoked job contacted metadata", err, x.lookups.Load())
			}
			if _, err := os.Stat(filepath.Join(x.root, "Movie.2026.nfo")); !os.IsNotExist(err) {
				t.Fatal("revoked job wrote metadata", err)
			}
			if action == "uninstall" {
				c = libraryPluginControlRead(t, x)
				if c.Authorized || c.State != "stopped" || !c.Next.IsZero() {
					t.Fatal("reinstall revived old cron", c)
				}
			}
		})
	}
}
func TestLibraryPluginInterruptedScanOrExecutionAndOrdinaryPartialNextCycle(t *testing.T) {
	for _, scenario := range []string{"scan", "running", "uncertain", "partial"} {
		t.Run(scenario, func(t *testing.T) {
			x := newLibraryHTTPFixture(t, false)
			organizationFile(t, x.root, "Movie.2026.mkv")
			libraryPluginInstall(t, x)
			c := libraryPluginSave(t, x, libraryPluginValues(x, true, "* * * * *", "no_force"))
			if scenario == "scan" {
				c.State = "scanning"
				c.Batch = strings.Repeat("a", 32)
				c.Created = time.Now().UTC().Format(time.RFC3339Nano)
				raw, _ := libraryControlJSON(c)
				if err := x.f.runner.scraper.system.Set(t.Context(), libraryControlKey, raw); err != nil {
					t.Fatal(err)
				}
			} else {
				libraryPluginTick(t, x, c.OnceAt)
				c = libraryPluginControlRead(t, x)
				switch scenario {
				case "running":
					if err := x.f.runner.organizer.jobs.ClaimLibraryJob(t.Context(), c.Jobs[0]); err != nil {
						t.Fatal(err)
					}
				case "partial":
					x.badImage = true
					if err := x.f.runner.library.tick(t.Context()); err != nil {
						t.Fatal(err)
					}
				case "uncertain":
					if _, err := x.f.db.Exec(`CREATE TRIGGER fail_scheduled_receipt BEFORE UPDATE ON GO_LIBRARY_SCRAPE_OUTPUTS WHEN NEW.STATE='created' BEGIN SELECT RAISE(ABORT,'receipt failed'); END`); err != nil {
						t.Fatal(err)
					}
					if err := x.f.runner.library.tick(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			monitorRebuild(t, x.f)
			libraryPluginTick(t, x, c.Next)
			next := libraryPluginControlRead(t, x)
			if scenario == "partial" {
				if !next.Authorized || next.State != "queued" || next.Batch == c.Batch {
					t.Fatal("ordinary incomplete metadata blocked all future cycles", next)
				}
			} else if next.Authorized || next.State != "paused" {
				t.Fatal("interrupted side effects replayable", next)
			}
		})
	}
}
func TestLibraryPluginAtomicBatchFailureHasNoReadyJobsAndNoAutomaticRescan(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationFile(t, x.root, "Movie.2026.mkv")
	libraryPluginInstall(t, x)
	c := libraryPluginSave(t, x, libraryPluginValues(x, true, "", "no_force"))
	if _, err := x.f.db.Exec(`CREATE TRIGGER fail_scheduled_jobs BEFORE INSERT ON GO_LIBRARY_SCRAPE_ITEMS BEGIN SELECT RAISE(ABORT,'cannot persist file worklist'); END`); err != nil {
		t.Fatal(err)
	}
	if err := x.f.runner.library.pluginTick(t.Context(), c.OnceAt); err == nil {
		t.Fatal("batch transaction succeeded")
	}
	jobs, err := x.f.runner.organizer.jobs.LibraryJobs(t.Context(), false)
	if err != nil || len(jobs) != 0 {
		t.Fatal("partial ready job leaked", jobs, err)
	}
	c = libraryPluginControlRead(t, x)
	if c.State != "scanning" || !c.OnceAt.IsZero() {
		t.Fatal(c)
	}
	if _, err := x.f.db.Exec(`DROP TRIGGER fail_scheduled_jobs`); err != nil {
		t.Fatal(err)
	}
	libraryPluginTick(t, x, time.Now().Add(time.Hour))
	if c = libraryPluginControlRead(t, x); c.State != "paused" {
		t.Fatal(c)
	}
	if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != 0 {
		t.Fatal("failed queue replayed", err)
	}
}
func TestLibraryPluginFreshFingerprintPermissionsAndConcurrentWorkerLock(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	libraryPluginInstall(t, x)
	detail := libraryPluginDetail(t, x)
	c := libraryPluginSave(t, x, libraryPluginValues(x, true, "", "no_force"))
	body, _ := json.Marshal(pluginConfigRequest{Fingerprint: detail.Meta.Fingerprint, Values: libraryPluginValues(x, true, "", "no_force")})
	r := nativeJSONRequest(x.f.handler, "PUT", libraryPluginEndpoint, x.f.token, string(body))
	if r.Code != 409 {
		t.Fatal("stale write repeated once", r.Code, r.Body.String())
	}
	guard, err := x.f.runner.organizer.jobs.AcquireJob(t.Context(), "library-scraper-scheduler")
	if err != nil {
		t.Fatal(err)
	}
	if err := x.f.runner.library.pluginTick(t.Context(), c.OnceAt); !errors.Is(err, organization.ErrBusy) {
		t.Fatal("scheduler ran without guard", err)
	}
	guard.Close()
	viewer, err := x.f.runner.organizer.auth.service.IssueToken("viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		r := nativeJSONRequest(x.f.handler, method, libraryPluginEndpoint, viewer, `{}`)
		if r.Code != 403 {
			t.Fatal(method, r.Code, r.Body.String())
		}
	}
	var runner *rssRunAPI
	handler, err := buildHandler(config.Config{ApplicationConfigPath: x.f.path, DisableLegacy: false}, x.f.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	r = nativeJSONRequest(handler, "PUT", libraryPluginEndpoint, x.f.token, string(body))
	if r.Code != 501 {
		t.Fatal("mixed write proxied or accepted", r.Code, r.Body.String())
	}
}
func TestLibraryPluginWorkerProductionDispatchRevocationAndShutdown(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationFile(t, x.root, "Movie.2026.mkv")
	libraryPluginInstall(t, x)
	c := libraryPluginSave(t, x, libraryPluginValues(x, true, "", "no_force"))
	seen := make(chan struct{})
	var once sync.Once
	base := x.f.runner.scraper.transport
	x.f.runner.scraper.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
			once.Do(func() { close(seen) })
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return base.RoundTrip(r)
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	ticks := make(chan time.Time)
	go func() { defer close(done); runLibraryScraper(ctx, x.f.runner.library, c.OnceAt, ticks) }()
	defer func() { cancel(); <-done }()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("production loop did not dispatch scheduled file")
	}
	r := nativeJSONRequest(x.f.handler, "DELETE", libraryPluginEndpoint, x.f.token, "")
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not join after cancellation")
	}
	if _, err := os.Stat(filepath.Join(x.root, "Movie.2026.nfo")); !os.IsNotExist(err) {
		t.Fatal("revoked in-flight metadata published", err)
	}
}

func TestLibraryPluginEmptyCountsSurviveRollupAndAutomaticSeasonOneIsVerified(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	empty := t.TempDir()
	if err := config.NewStore(x.f.path).Update(map[string]any{"media.anime_path": empty}); err != nil {
		t.Fatal(err)
	}
	organizationFile(t, x.root, "Show/Episodes/Show.E02.mkv")
	libraryPluginInstall(t, x)
	values := libraryPluginValues(x, true, "", "no_force")
	values["scraper_path"] = []string{x.input.RootID, subscriptionResourceKey("target:" + empty + ":ANIME")}
	c := libraryPluginSave(t, x, values)
	libraryPluginTick(t, x, c.OnceAt)
	c = libraryPluginControlRead(t, x)
	if c.Empty != 1 || len(c.Jobs) != 1 {
		t.Fatal(c)
	}
	if err := x.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if job := x.job(t, c.Jobs[0]); job.State != "completed" {
		t.Fatal(job)
	}
	libraryPluginTick(t, x, time.Now().Add(time.Minute))
	runs, err := x.f.runner.organizer.jobs.LibraryPluginRuns(t.Context())
	if err != nil || len(runs) != 1 || runs[0].Empty != 1 || runs[0].State != "completed" {
		t.Fatal(runs, err)
	}
	if _, err := os.Stat(filepath.Join(x.root, "Show/Episodes/Show.E02.nfo")); err != nil {
		t.Fatal(err)
	}
}
func TestLibraryPluginCanonicalAliasAndAncestorExclusions(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	alias := filepath.Join(t.TempDir(), "media-alias")
	if err := os.Symlink(x.root, alias); err != nil {
		t.Fatal(err)
	}
	if err := config.NewStore(x.f.path).Update(map[string]any{"media.movie_path": alias, "media.tv_path": ""}); err != nil {
		t.Fatal(err)
	}
	x.input.RootID = subscriptionResourceKey("target:" + alias + ":MOV")
	for _, name := range []string{"skip/Movie.2026.mkv", "keep/Movie.2026.mkv"} {
		organizationFile(t, x.root, name)
	}
	real, err := filepath.EvalSymlinks(x.root)
	if err != nil {
		t.Fatal(err)
	}
	libraryPluginInstall(t, x)
	values := libraryPluginValues(x, true, "", "no_force")
	values["exclude_path"] = filepath.Join(real, "skip")
	c := libraryPluginSave(t, x, values)
	libraryPluginTick(t, x, c.OnceAt)
	c = libraryPluginControlRead(t, x)
	if len(c.Jobs) != 1 || x.job(t, c.Jobs[0]).Total != 1 {
		t.Fatal("canonical exclusion bypassed through alias", c)
	}
	libraryPluginSave(t, x, map[string]any{"scraper_path": []string{x.input.RootID}, "go_stop": true})
	values["exclude_path"] = filepath.Dir(real)
	c = libraryPluginSave(t, x, values)
	libraryPluginTick(t, x, c.OnceAt)
	c = libraryPluginControlRead(t, x)
	if len(c.Jobs) != 0 || c.Empty != 1 || c.State != "idle" {
		t.Fatal("ancestor exclusion ignored", c)
	}
}
func TestLibraryPluginDirectAdministratorChangeAndCorruptStateUninstall(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	libraryPluginInstall(t, x)
	if err := config.NewStore(x.f.path).Update(map[string]any{"app.login_user": "replacement-admin"}); err != nil {
		t.Fatal(err)
	}
	r := nativeJSONRequest(x.f.handler, "GET", libraryPluginEndpoint, x.f.token, "")
	if r.Code != 403 {
		t.Fatal("stale administrator snapshot retained authority", r.Code, r.Body.String())
	}
	if err := config.NewStore(x.f.path).Update(map[string]any{"app.login_user": "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.runner.scraper.system.Set(t.Context(), libraryControlKey, `{"Epoch":"broken","State":"unknown"}`); err != nil {
		t.Fatal(err)
	}
	r = nativeJSONRequest(x.f.handler, "DELETE", libraryPluginEndpoint, x.f.token, "")
	if r.Code != 200 {
		t.Fatal("corrupt schedule prevented explicit revocation", r.Code, r.Body.String())
	}
	libraryPluginInstall(t, x)
	c := libraryPluginControlRead(t, x)
	if c.Authorized || c.State != "stopped" {
		t.Fatal(c)
	}
}

func TestLibraryPluginPersistedUnknownKeysCannotAuthorizeRecurringWrites(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	libraryPluginInstall(t, x)
	values := map[string]any{"cron": "* * * * *", "mode": "force_all", "scraper_path": []string{x.root}, "go_confirm": true, "go_old_stopped": true, "go_overwrite": true}
	b, _ := json.Marshal(values)
	if err := x.f.runner.scraper.system.Set(t.Context(), libraryPluginKey, string(b)); err != nil {
		t.Fatal(err)
	}
	detail := libraryPluginDetail(t, x)
	body, _ := json.Marshal(pluginConfigRequest{Fingerprint: detail.Meta.Fingerprint, Values: map[string]any{"scraper_path": []string{x.input.RootID}, "mode": "force_all"}})
	r := nativeJSONRequest(x.f.handler, "PUT", libraryPluginEndpoint, x.f.token, string(body))
	if r.Code != 400 {
		t.Fatal("stored values became request consent", r.Code, r.Body.String())
	}
	if c := libraryPluginControlRead(t, x); c.Authorized {
		t.Fatal(c)
	}
}
