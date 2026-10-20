package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
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
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

func TestIntakeScraperPartialGroupDoesNotEnqueueAndBadConfigPreservesIntake(t *testing.T) {
	for _, partial := range []bool{true, false} {
		x := newIntakeScrapeFixture(t, "copy", false)
		if partial {
			for _, item := range x.job.Items {
				if item.Kind == "subtitle" {
					path := filepath.Join(x.target, item.Target)
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("existing companion"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			status, job, body := organizationMoveRequest(t, x.f, x.job.ID, "execute")
			if status == 200 && job.State == "completed" {
				t.Fatal(body)
			}
			if rows := x.receipts(t); len(rows) != 0 {
				t.Fatal("partial group enqueued", rows)
			}
		} else {
			if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, "invalid-private-settings"); err != nil {
				t.Fatal(err)
			}
			x.execute(t)
			if rows := x.receipts(t); len(rows) != 1 || rows[0].State != "blocked" {
				t.Fatal(rows)
			}
		}
		if err := x.f.runner.scraper.tick(t.Context()); err != nil || x.lookups != 0 {
			t.Fatal("invalid intake contacted scraper", err, x.lookups)
		}
	}
}
func TestIntakeScraperWorkerStartsAndCancellationJoinsWithoutReplaying(t *testing.T) {
	x := newIntakeScrapeFixture(t, "copy", false)
	x.execute(t)
	seen := make(chan struct{})
	base := x.f.runner.scraper.transport
	var once sync.Once
	x.f.runner.scraper.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
			once.Do(func() { close(seen) })
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return base.RoundTrip(r)
	})
	ctx, cancel := context.WithCancel(t.Context())
	wait := startIntakeScraper(ctx, x.f.runner.scraper)
	defer func() { cancel(); wait() }()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("native worker never processed pending intake")
	}
	cancel()
	wait()
	if rows := x.receipts(t); len(rows) != 1 || rows[0].State != "blocked" {
		t.Fatal(rows)
	}
	x.f.runner.scraper.transport = base
	if err := x.f.runner.scraper.tick(t.Context()); err != nil || x.lookups != 0 {
		t.Fatal("cancelled metadata lookup replayed", err, x.lookups)
	}
}

type intakeScrapeFixture struct {
	f                          *subscriptionRunFixture
	source, target             string
	job                        organization.Job
	lookups, images, subtitles int
	badImage                   bool
}

func newIntakeScrapeFixture(t *testing.T, mode string, tv bool, prepare ...func(string)) *intakeScrapeFixture {
	t.Helper()
	f, source, target, input := organizationFixture(t)
	x := &intakeScrapeFixture{f: f, source: source, target: target}
	input.Mode = mode
	if err := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": true, "app.tmdb_image_url": "https://scrape-images.test"}); err != nil {
		t.Fatal(err)
	}
	files := []string{"Movie.2026.1080p.mkv", "Movie.2026.1080p.en.srt"}
	if tv {
		files = []string{"Show.S01E02.mkv", "Show.S01E02.en.srt"}
		input.TargetID = subscriptionResourceKey("target:" + target + ":TV")
	}
	for _, name := range files {
		organizationFile(t, source, name)
	}
	for _, hook := range prepare {
		hook(source)
	}
	for _, name := range files {
		old := time.Now().Add(-time.Hour)
		os.Chtimes(filepath.Join(source, name), old, old)
	}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "csf.test" {
			x.subtitles++
			if _, err := os.Stat(x.nfo(t)); err != nil {
				t.Fatal("subtitle dispatched before scraper", err)
			}
			return jsonResponse(r, `{"job_id":"csf-after-nfo"}`), nil
		}
		if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
			x.lookups++
			if r.Method != "GET" {
				t.Fatal("scraper performed remote mutation")
			}
			switch r.URL.Path {
			case "/3/movie/100":
				return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","overview":"Overview & safe XML","poster_path":"/poster.png"}`), nil
			case "/3/tv/200":
				return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","poster_path":"/poster.png","external_ids":{"tvdb_id":300}}`), nil
			}
			t.Fatal("wrong enrichment identity", r.URL)
		}
		if r.URL.Path == "/3/tv/200/season/1" {
			return jsonResponse(r, `{"id":201,"season_number":1,"name":"Season 1","episodes":[{"id":202,"show_id":200,"season_number":1,"episode_number":1,"name":"First"},{"id":203,"show_id":200,"season_number":1,"episode_number":2,"name":"Second","still_path":"/still.png"}]}`), nil
		}
		if r.URL.Host == "scrape-images.test" {
			x.images++
			if r.Method != "GET" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
				t.Fatal("image request leaked credentials")
			}
			if x.badImage {
				return jsonResponse(r, `{"error":"private-path-or-key"}`), nil
			}
			var b bytes.Buffer
			png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			v := jsonResponse(r, "")
			v.Body = io.NopCloser(strings.NewReader(b.String()))
			return v, nil
		}
		return base.RoundTrip(r)
	})
	monitorRebuild(t, f)
	raw := `{"scraper_nfo":{"movie":{"basic":true},"tv":{"basic":true,"season_basic":true,"episode_basic":true}},"scraper_pic":{"movie":{"poster":true},"tv":{"poster":true,"episode_thumb":true}}}`
	if err := f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, raw); err != nil {
		t.Fatal(err)
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 || !plan.ScraperEnabled {
		t.Fatal(status, body)
	}
	x.job = organizationCreateJob(t, f, input, plan)
	return x
}
func (x *intakeScrapeFixture) nfo(t *testing.T) string {
	t.Helper()
	for _, item := range x.job.Items {
		if item.Kind == "media" {
			return filepath.Join(x.target, strings.TrimSuffix(item.Target, filepath.Ext(item.Target))+".nfo")
		}
	}
	t.Fatal("no media")
	return ""
}
func (x *intakeScrapeFixture) execute(t *testing.T) {
	t.Helper()
	status, job, body := organizationMoveRequest(t, x.f, x.job.ID, "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
	x.job = job
}
func (x *intakeScrapeFixture) receipts(t *testing.T) []organization.ScrapeReceipt {
	t.Helper()
	rows, err := x.f.runner.organizer.jobs.Scrapes(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestIntakeScraperAllModesPublishesNFOAndImageOnceWithoutRepeatingIntake(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			x := newIntakeScrapeFixture(t, mode, false)
			if rows := x.receipts(t); len(rows) != 0 {
				t.Fatal("draft caused scraping")
			}
			x.execute(t)
			if rows := x.receipts(t); len(rows) != 1 || rows[0].State != "pending" || x.lookups != 0 {
				t.Fatal(rows, x.lookups)
			}
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows := x.receipts(t)
			if len(rows) != 1 || rows[0].State != "completed" || len(rows[0].Results) != 2 || x.lookups != 1 || x.images != 1 {
				t.Fatal(rows, x.lookups, x.images)
			}
			b, err := os.ReadFile(x.nfo(t))
			if err != nil || !strings.Contains(string(b), "<tmdbid>100</tmdbid>") {
				t.Fatal(string(b), err)
			}
			monitorRebuild(t, x.f)
			x.execute(t)
			if err := x.f.runner.scraper.tick(t.Context()); err != nil || x.lookups != 1 {
				t.Fatal("restart replay", err, x.lookups)
			}
			var count int
			x.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count)
			if count != 1 || x.f.adds != 0 {
				t.Fatal("intake repeated", count, x.f.adds)
			}
			w := nativeJSONRequest(x.f.handler, "GET", "/api/v1/organization/scraper", x.f.token, "")
			if w.Code != 200 || strings.Contains(w.Body.String(), x.target) || strings.Contains(w.Body.String(), "private-tmdb") || strings.Contains(w.Body.String(), ".nastool-sidecar-") {
				t.Fatal("unsafe receipt", w.Code, w.Body.String())
			}
		})
	}
}
func TestIntakeScraperTVProducesShowSeasonEpisodeAndThumbnails(t *testing.T) {
	x := newIntakeScrapeFixture(t, "copy", true)
	x.execute(t)
	if err := x.f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rows := x.receipts(t); len(rows) != 1 || rows[0].State != "completed" || len(rows[0].Results) != 5 {
		t.Fatal(rows)
	}
	episode := x.nfo(t)
	for _, file := range []string{episode, filepath.Join(filepath.Dir(episode), "season.nfo"), filepath.Join(filepath.Dir(filepath.Dir(episode)), "tvshow.nfo"), filepath.Join(filepath.Dir(filepath.Dir(episode)), "poster.png"), strings.TrimSuffix(episode, ".nfo") + "-thumb.png"} {
		if _, err := os.Stat(file); err != nil {
			t.Fatal(file, err)
		}
	}
	b, _ := os.ReadFile(episode)
	if !strings.Contains(string(b), "<episode>2</episode>") || !strings.Contains(string(b), "<tmdbid>203</tmdbid>") {
		t.Fatal(string(b))
	}
}
func TestIntakeScraperPreservesExistingSidecarsAndInvalidImagesDoNotUndoIntake(t *testing.T) {
	for _, bad := range []bool{false, true} {
		x := newIntakeScrapeFixture(t, "copy", false)
		x.badImage = bad
		x.execute(t)
		if !bad {
			os.WriteFile(x.nfo(t), []byte("existing user NFO"), 0600)
		}
		if err := x.f.runner.scraper.tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		rows := x.receipts(t)
		if bad {
			if rows[0].State != "partial" {
				t.Fatal(rows)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(x.nfo(t)), "poster.png")); !os.IsNotExist(err) {
				t.Fatal("invalid image written", err)
			}
		} else {
			b, _ := os.ReadFile(x.nfo(t))
			if string(b) != "existing user NFO" || rows[0].Results[0].State != "preserved" {
				t.Fatal(rows, string(b))
			}
		}
		job, err := x.f.runner.organizer.jobs.Get(t.Context(), x.job.ID)
		if err != nil || job.State != "completed" {
			t.Fatal("scraper rolled back intake", job, err)
		}
	}
}
func TestIntakeScraperChangedScopeInterruptedIntentAndFailedJournalNeverReplay(t *testing.T) {
	for _, mode := range []string{"options", "target", "running", "journal"} {
		t.Run(mode, func(t *testing.T) {
			x := newIntakeScrapeFixture(t, "copy", false)
			x.execute(t)
			rows := x.receipts(t)
			switch mode {
			case "options":
				x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"movie":{"basic":false}}}`)
			case "target":
				for _, item := range x.job.Items {
					if item.Kind == "media" {
						os.WriteFile(filepath.Join(x.target, item.Target), []byte("replacement"), 0600)
					}
				}
			case "running":
				if err := x.f.runner.organizer.jobs.ClaimScrape(t.Context(), rows[0]); err != nil {
					t.Fatal(err)
				}
			case "journal":
				if _, err := x.f.db.Exec(`CREATE TRIGGER fail_sidecar_intent BEFORE UPDATE OF OUTPUTS ON GO_SCRAPE_JOBS BEGIN SELECT RAISE(IGNORE); END`); err != nil {
					t.Fatal(err)
				}
			}
			monitorRebuild(t, x.f)
			if err := x.f.runner.scraper.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			calls := x.lookups
			if err := x.f.runner.scraper.tick(t.Context()); err != nil || x.lookups != calls {
				t.Fatal("uncertain write replayed", err)
			}
			if _, err := os.Stat(x.nfo(t)); !os.IsNotExist(err) {
				t.Fatal("invalid scope published NFO", err)
			}
			if mode == "running" && x.lookups != 0 {
				t.Fatal("interrupted worker replayed")
			}
		})
	}
}
func TestIntakeScraperRunsBeforeChineseSubFinderSubmission(t *testing.T) {
	x := newIntakeScrapeFixture(t, "copy", false)
	w := nativeJSONRequest(x.f.handler, "POST", "/api/v1/plugins/ChineseSubFinder/install", x.f.token, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = nativeJSONRequest(x.f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", x.f.token, `{"values":{"host":"https://csf.test","api_key":"private-csf-key"}}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	x.execute(t)
	if err := x.f.runner.subtitles.tick(t.Context()); err != nil || x.subtitles != 0 {
		t.Fatal("subtitle skipped scraper queue", err, x.subtitles)
	}
	if err := x.f.runner.scraper.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := x.f.runner.subtitles.tick(t.Context()); err != nil || x.subtitles != 1 {
		t.Fatal(err, x.subtitles)
	}
}
func TestIntakeScraperSettingsPermissionsUnknownPreservationAndNoImplicitEnable(t *testing.T) {
	x := newIntakeScrapeFixture(t, "copy", false)
	if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"private_unknown":"must-stay-private","scraper_nfo":{"movie":{"basic":true,"extra":false}}}`); err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(x.f.handler, "PUT", "/api/v1/organization/scraper/enabled", x.f.token, `{"enabled":false,"confirm":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var status struct{ Data struct{ Fingerprint string } }
	json.Unmarshal(w.Body.Bytes(), &status)
	body := `{"options":{"scraper_nfo":{"movie":{"credits":true}}},"fingerprint":"` + status.Data.Fingerprint + `","confirm":true}`
	w = nativeJSONRequest(x.f.handler, "PUT", "/api/v1/organization/scraper/options", x.f.token, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) || strings.Contains(w.Body.String(), "must-stay-private") {
		t.Fatal(w.Code, w.Body.String())
	}
	raw, _ := x.f.runner.scraper.system.Get(t.Context(), scraper.ConfigKey)
	if !strings.Contains(raw, "must-stay-private") || !strings.Contains(raw, `"basic":true`) {
		t.Fatal("options lost old data", raw)
	}
	w = nativeJSONRequest(x.f.handler, "PUT", "/api/v1/organization/scraper/options", x.f.token, body)
	if w.Code != 409 {
		t.Fatal("stale config accepted", w.Code, w.Body.String())
	}
	performFormRequest(x.f.handler, "/api/v1/user/manage", x.f.token, url.Values{"oper": {"add"}, "name": {"scrape-viewer"}, "password": {"strong-password"}, "pris": {""}})
	viewer := loginForTest(t, x.f.handler, "scrape-viewer", "strong-password")
	for _, spec := range []struct{ method, path, body string }{{"GET", "/api/v1/organization/scraper", ""}, {"PUT", "/api/v1/organization/scraper/options", body}, {"PUT", "/api/v1/organization/scraper/enabled", `{"enabled":true,"confirm":true}`}} {
		if w := nativeJSONRequest(x.f.handler, spec.method, spec.path, viewer, spec.body); w.Code != 403 {
			t.Fatal("viewer scraper access", w.Code, w.Body.String())
		}
	}
	h, err := newHandler(config.Config{ApplicationConfigPath: x.f.path, LegacyBackendURL: "http://legacy:3000"}, x.f.transport)
	if err != nil {
		t.Fatal(err)
	}
	if w := nativeJSONRequest(h, "PUT", "/api/v1/organization/scraper/enabled", x.f.token, `{"enabled":true,"confirm":true}`); w.Code != 501 {
		t.Fatal("mixed mode fallback", w.Code, w.Body.String())
	}
}
