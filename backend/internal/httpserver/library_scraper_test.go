package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

const libraryEndpoint = "/api/v1/organization/library-scraper"

type libraryHTTPFixture struct {
	f                       *subscriptionRunFixture
	root, source            string
	input                   libraryPlanInput
	search, lookups, images atomic.Int32
	badImage                bool
}

func newLibraryHTTPFixture(t *testing.T, tv bool) *libraryHTTPFixture {
	t.Helper()
	f, source, root, _ := organizationFixture(t)
	x := &libraryHTTPFixture{f: f, root: root, source: source, input: libraryPlanInput{RootID: subscriptionResourceKey("target:" + root + ":MOV"), Path: ".", Mode: "no_force"}}
	if tv {
		x.input.RootID = subscriptionResourceKey("target:" + root + ":TV")
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": false, "app.tmdb_image_url": "https://library-images.test"}); err != nil {
		t.Fatal(err)
	}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Error("manual scraper performed remote mutation", r.Method, r.URL.Host)
			return nil, context.Canceled
		}
		if strings.HasPrefix(r.URL.Path, "/3/search/") {
			x.search.Add(1)
		}
		if strings.HasPrefix(r.URL.Query().Get("append_to_response"), "credits,external_ids") {
			x.lookups.Add(1)
			switch r.URL.Path {
			case "/3/movie/100":
				return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","poster_path":"/poster.png"}`), nil
			case "/3/movie/101":
				return jsonResponse(r, `{"id":101,"title":"Original NFO identity","release_date":"2026-01-01","poster_path":"/poster.png"}`), nil
			case "/3/tv/200":
				return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","poster_path":"/poster.png"}`), nil
			}
		}
		if r.URL.Path == "/3/movie/101" {
			return jsonResponse(r, `{"id":101,"title":"Original NFO identity","release_date":"2026-01-01"}`), nil
		}
		if r.URL.Path == "/3/tv/200/season/1" {
			return jsonResponse(r, `{"id":201,"season_number":1,"episodes":[{"id":202,"show_id":200,"season_number":1,"episode_number":1,"name":"First"},{"id":203,"show_id":200,"season_number":1,"episode_number":2,"name":"Second"}]}`), nil
		}
		if r.URL.Host == "library-images.test" {
			x.images.Add(1)
			if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
				t.Error("image read leaked credentials")
			}
			if x.badImage {
				return jsonResponse(r, `{"error":"private-image-token"}`), nil
			}
			var b bytes.Buffer
			png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			resp := jsonResponse(r, "")
			resp.Body = io.NopCloser(bytes.NewReader(b.Bytes()))
			return resp, nil
		}
		return base.RoundTrip(r)
	})
	monitorRebuild(t, f)
	if err := f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"movie":{"basic":true},"tv":{"basic":true,"season_basic":true,"episode_basic":true}},"scraper_pic":{"movie":{"poster":true},"tv":{"poster":true}}}`); err != nil {
		t.Fatal(err)
	}
	return x
}
func (x *libraryHTTPFixture) preview(t *testing.T) libraryPreview {
	t.Helper()
	body, _ := json.Marshal(x.input)
	r := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/preview", x.f.token, string(body))
	var result struct{ Data libraryPreview }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return result.Data
}
func (x *libraryHTTPFixture) create(t *testing.T, p libraryPreview) organization.LibraryJob {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"rootId": p.Input.RootID, "path": p.Input.Path, "mode": p.Input.Mode, "excludes": p.Input.Excludes, "requestId": p.Input.RequestID, "fingerprint": p.Fingerprint, "confirm": true, "confirmOverwrite": true, "confirmOldExecutorsStopped": true})
	r := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs", x.f.token, string(body))
	var result struct{ Data organization.LibraryJob }
	if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &result) != nil {
		t.Fatal(r.Code, r.Body.String())
	}
	return result.Data
}
func (x *libraryHTTPFixture) job(t *testing.T, id string) organization.LibraryJob {
	t.Helper()
	job, err := x.f.runner.organizer.jobs.LibraryJob(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func libraryOutputBody(r organization.LibraryOutput) string {
	b, _ := json.Marshal(map[string]any{"fingerprint": organization.LibraryOutputDigest(r), "confirm": true, "confirmOldExecutorsStopped": true})
	return string(b)
}

func TestLibraryScraperModesNFOIdentityAndNoInventedIntake(t *testing.T) {
	for _, mode := range []string{"no_force", "force_nfo", "force_all"} {
		t.Run(mode, func(t *testing.T) {
			x := newLibraryHTTPFixture(t, false)
			x.input.Mode = mode
			organizationFile(t, x.root, "Movie/Movie.2026.mkv")
			nfo := filepath.Join(x.root, "Movie/Movie.2026.nfo")
			original := `<movie><uniqueid type="tmdb">101</uniqueid></movie>`
			if err := os.WriteFile(nfo, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			poster := filepath.Join(x.root, "Movie/poster.png")
			if err := os.WriteFile(poster, []byte("old image"), 0600); err != nil {
				t.Fatal(err)
			}
			p := x.preview(t)
			if p.Total != 1 || p.Blocked != 0 || x.lookups.Load() != 0 {
				t.Fatal(p)
			}
			job := x.create(t, p)
			if job.State != "ready" {
				t.Fatal(job)
			}
			// Saved worklists do not adopt new media discovered after confirmation.
			organizationFile(t, x.root, "Later/Later.2026.mkv")
			if err := x.f.runner.library.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			job = x.job(t, job.ID)
			if job.State != "completed" || job.Total != 1 || job.Done != 1 {
				t.Fatal(job)
			}
			data, _ := os.ReadFile(nfo)
			if mode == "no_force" {
				if string(data) != original || x.search.Load() != 0 {
					t.Fatal(string(data), x.search.Load())
				}
			} else if !bytes.Contains(data, []byte(`<uniqueid type="tmdb" default="true">100</uniqueid>`)) {
				t.Fatal(string(data))
			}
			image, _ := os.ReadFile(poster)
			if mode == "force_all" {
				if scraper.ValidateImage(image, ".png") != nil || x.images.Load() != 1 {
					t.Fatal("image not replaced", x.images.Load())
				}
			} else if string(image) != "old image" || x.images.Load() != 0 {
				t.Fatal("image overwritten in preserve mode", string(image))
			}
			rows, err := x.f.runner.organizer.jobs.LibraryOutputs(t.Context(), job.ID, 1)
			if err != nil || len(rows) != 2 {
				t.Fatal(rows, err)
			}
			for _, out := range rows {
				if out.State == "replaced" {
					path := libraryEndpoint + "/outputs/" + out.ID
					review := nativeJSONRequest(x.f.handler, "GET", path, x.f.token, "")
					if review.Code != 200 || !strings.Contains(review.Body.String(), `"canRestore":true`) || strings.Contains(review.Body.String(), ".nastool-library-") {
						t.Fatal(review.Code, review.Body.String())
					}
					r := nativeJSONRequest(x.f.handler, "POST", path+"/restore", x.f.token, libraryOutputBody(out))
					if r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"restored"`) {
						t.Fatal(r.Code, r.Body.String())
					}
				}
			}
			data, _ = os.ReadFile(nfo)
			image, _ = os.ReadFile(poster)
			if string(data) != original || string(image) != "old image" {
				t.Fatal("restore lost originals")
			}
			if _, err := os.Stat(filepath.Join(x.root, "Later/Later.2026.nfo")); !os.IsNotExist(err) {
				t.Fatal("new media entered saved scope", err)
			}
			var count int
			for _, table := range []string{"GO_ORGANIZATION_JOBS", "TRANSFER_HISTORY", "GO_SCRAPE_JOBS"} {
				var exists bool
				if err := x.f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name=?)`, table).Scan(&exists); err != nil {
					t.Fatal(err)
				}
				if exists {
					if err := x.f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
						t.Fatal("manual library scrape invented intake/history/event", table, count, err)
					}
				}
			}
			lookups := x.lookups.Load()
			monitorRebuild(t, x.f)
			if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != lookups {
				t.Fatal("restart replayed scrape", err)
			}
			if again := x.create(t, p); again.ID != job.ID {
				t.Fatal("lost submit duplicated job", again)
			}
		})
	}
}

func TestLibraryScraperTVSharedReceiptsAndReadonlyThumbnail(t *testing.T) {
	x := newLibraryHTTPFixture(t, true)
	for _, file := range []string{"Show/Season 01/Show.S01E01.mkv", "Show/Season 01/Show.S01E02.mkv"} {
		organizationFile(t, x.root, file)
	}
	if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"tv":{"basic":true,"season_basic":true,"episode_basic":true}},"scraper_pic":{"tv":{"episode_thumb":true,"episode_thumb_ffmpeg":true}}}`); err != nil {
		t.Fatal(err)
	}
	renders := 0
	x.f.runner.scraper.thumbnail = func(_ context.Context, f *os.File) ([]byte, error) {
		renders++
		data, err := io.ReadAll(f)
		if err != nil || string(data) != "original-media" {
			t.Fatal(string(data), err)
		}
		if _, err := f.Write([]byte("bad")); err == nil {
			t.Fatal("writable media descriptor")
		}
		return thumbnailJPEG(t), nil
	}
	p := x.preview(t)
	job := x.create(t, p)
	if err := x.f.runner.library.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	job = x.job(t, job.ID)
	if job.State != "completed" || job.Done != 2 || renders != 2 {
		t.Fatal(job, renders)
	}
	outputs, err := x.f.runner.organizer.jobs.LibraryOutputs(t.Context(), job.ID, 1)
	if err != nil || len(outputs) != 6 {
		t.Fatal(outputs, err)
	}
	for _, name := range []string{"Show/tvshow.nfo", "Show/Season 01/season.nfo", "Show/Season 01/Show.S01E01.nfo", "Show/Season 01/Show.S01E02.nfo", "Show/Season 01/Show.S01E01-thumb.jpg", "Show/Season 01/Show.S01E02-thumb.jpg"} {
		if _, err := os.Stat(filepath.Join(x.root, name)); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestLibraryScraperStaleReviewConsentPermissionsAndRootBoundaries(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationFile(t, x.root, "Movie.2026.mkv")
	p := x.preview(t)
	for _, path := range []string{libraryEndpoint, libraryEndpoint + "/jobs/" + strings.Repeat("a", 64)} {
		r := nativeJSONRequest(x.f.handler, "GET", path, "", "")
		if r.Code != 401 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	viewer, err := x.f.runner.organizer.auth.service.IssueToken("viewer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r0 := nativeJSONRequest(x.f.handler, "GET", libraryEndpoint, viewer, "")
	if r0.Code != 403 {
		t.Fatal("non-admin accessed scraper", r0.Code, r0.Body.String())
	}
	body := map[string]any{"rootId": p.Input.RootID, "path": p.Input.Path, "mode": "force_all", "requestId": p.Input.RequestID, "fingerprint": p.Fingerprint, "confirm": true, "confirmOldExecutorsStopped": true}
	data, _ := json.Marshal(body)
	r := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs", x.f.token, string(data))
	if r.Code != 400 {
		t.Fatal("force lacked independent consent", r.Code, r.Body.String())
	}
	body["mode"] = "no_force"
	body["confirmOldExecutorsStopped"] = false
	data, _ = json.Marshal(body)
	r = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs", x.f.token, string(data))
	if r.Code != 400 {
		t.Fatal(r.Code, r.Body.String())
	}
	body["confirmOldExecutorsStopped"] = true
	organizationFile(t, x.root, "New.2026.mkv")
	data, _ = json.Marshal(body)
	r = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs", x.f.token, string(data))
	if r.Code != 409 {
		t.Fatal("stale review accepted", r.Code, r.Body.String())
	}
	x.input.Path = "../escape"
	request, _ := json.Marshal(x.input)
	r = nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/preview", x.f.token, string(request))
	if r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	jobs, err := x.f.runner.organizer.jobs.LibraryJobs(t.Context(), false)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	var runner *rssRunAPI
	handler, err := buildHandler(config.Config{ApplicationConfigPath: x.f.path, DisableLegacy: false}, x.f.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	r = nativeJSONRequest(handler, "GET", libraryEndpoint, x.f.token, "")
	if r.Code != 501 {
		t.Fatal("mixed scraper mutated or proxied", r.Code, r.Body.String())
	}
}

func TestLibraryScraperMetadataOrReceiptFailureAndNoReplay(t *testing.T) {
	for _, scenario := range []string{"invalid-image", "changed-media", "changed-settings", "source-revoked", "finish-failed", "intent-failed", "shared-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			x := newLibraryHTTPFixture(t, false)
			organizationFile(t, x.root, "Movie/Movie.2026.mkv")
			if scenario == "shared-conflict" {
				organizationFile(t, x.root, "Movie/Another.2026.mkv")
				if err := os.WriteFile(filepath.Join(x.root, "Movie/Another.2026.nfo"), []byte(`<movie><tmdbid>101</tmdbid></movie>`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			job := x.create(t, x.preview(t))
			switch scenario {
			case "invalid-image":
				x.badImage = true
			case "changed-media":
				if err := os.WriteFile(filepath.Join(x.root, "Movie/Movie.2026.mkv"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed-settings":
				if err := x.f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{}`); err != nil {
					t.Fatal(err)
				}
			case "source-revoked":
				if _, err := x.f.db.Exec(`UPDATE DOWNLOADER SET DOWNLOAD_DIR='[]'`); err != nil {
					t.Fatal(err)
				}
			case "finish-failed":
				if _, err := x.f.db.Exec(`CREATE TRIGGER fail_library_finish BEFORE UPDATE ON GO_LIBRARY_SCRAPE_OUTPUTS WHEN NEW.STATE='created' BEGIN SELECT RAISE(ABORT,'receipt unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			case "intent-failed":
				if _, err := x.f.db.Exec(`CREATE TRIGGER fail_library_intent BEFORE INSERT ON GO_LIBRARY_SCRAPE_OUTPUTS WHEN NEW.STATE='writing' BEGIN SELECT RAISE(ABORT,'intent unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if err := x.f.runner.library.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			job = x.job(t, job.ID)
			if job.State == "completed" || job.State == "ready" || job.State == "running" {
				t.Fatal("failure claimed success", job)
			}
			if scenario == "changed-media" || scenario == "changed-settings" || scenario == "source-revoked" || scenario == "intent-failed" {
				if _, err := os.Stat(filepath.Join(x.root, "Movie/Movie.2026.nfo")); !os.IsNotExist(err) {
					t.Fatal("blocked item published", err)
				}
			}
			if scenario == "finish-failed" {
				rows, err := x.f.runner.organizer.jobs.LibraryOutputs(t.Context(), job.ID, 1)
				if err != nil || len(rows) != 1 || rows[0].State != "writing" {
					t.Fatal(rows, err)
				}
				path := libraryEndpoint + "/outputs/" + rows[0].ID
				r := nativeJSONRequest(x.f.handler, "GET", path, x.f.token, "")
				if r.Code != 200 || !strings.Contains(r.Body.String(), `"canReconcile":true`) {
					t.Fatal(r.Code, r.Body.String())
				}
				x.f.db.Exec(`DROP TRIGGER fail_library_finish`)
				r = nativeJSONRequest(x.f.handler, "POST", path+"/reconcile", x.f.token, libraryOutputBody(rows[0]))
				if r.Code != 200 || !strings.Contains(r.Body.String(), `"state":"created"`) {
					t.Fatal(r.Code, r.Body.String())
				}
			}
			before := x.lookups.Load()
			monitorRebuild(t, x.f)
			if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != before {
				t.Fatal("failure replayed", err)
			}
		})
	}
}

func TestLibraryScraperCancelStopsInFlightPublicationAndWorkerJoins(t *testing.T) {
	x := newLibraryHTTPFixture(t, false)
	organizationFile(t, x.root, "Movie.2026.mkv")
	job := x.create(t, x.preview(t))
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
	wait := startLibraryScraper(ctx, x.f.runner.library)
	defer func() { cancel(); wait() }()
	select {
	case <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not begin")
	}
	r := nativeJSONRequest(x.f.handler, "POST", libraryEndpoint+"/jobs/"+job.ID+"/cancel", x.f.token, `{"confirm":true}`)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	cancel()
	wait()
	if job = x.job(t, job.ID); job.State != "cancelled" {
		t.Fatal(job)
	}
	if _, err := os.Stat(filepath.Join(x.root, "Movie.2026.nfo")); !os.IsNotExist(err) {
		t.Fatal("cancelled file published", err)
	}
	x.f.runner.scraper.transport = base
	before := x.lookups.Load()
	if err := x.f.runner.library.tick(t.Context()); err != nil || x.lookups.Load() != before {
		t.Fatal("cancelled job replayed", err)
	}
}
