package httpserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func animeOrganizationFixture(t *testing.T, configured, animated bool) (*subscriptionRunFixture, string, string, string, organizationPlanInput) {
	t.Helper()
	f, source, tv, input, _ := organizationLinkFixture(t, "copy")
	anime := t.TempDir()
	for _, pair := range [][2]string{{"Movie.2026.1080p.mkv", "Show.S01E01.1080p.mkv"}, {"Movie.2026.1080p.en.srt", "Show.S01E01.1080p.en.srt"}} {
		if err := os.Rename(filepath.Join(source, pair[0]), filepath.Join(source, pair[1])); err != nil {
			t.Fatal(err)
		}
	}
	category := "movie:\n  Movies:\ntv:\n  Drama:\nanime:\n  Animation:\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.path), "anime-routing.yaml"), []byte(category), 0600); err != nil {
		t.Fatal(err)
	}
	var animePath any = []any{}
	if configured {
		animePath = []any{anime}
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.movie_path": t.TempDir(), "media.tv_path": tv, "media.anime_path": animePath, "media.category": "anime-routing"}); err != nil {
		t.Fatal(err)
	}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "tmdb.test" {
			t.Fatalf("unexpected external/legacy request: %s", r.URL)
		}
		switch r.URL.Path {
		case "/3/search/tv":
			return jsonResponse(r, `{"results":[{"id":200,"name":"Show","first_air_date":"2026-01-01"}]}`), nil
		case "/3/tv/200":
			genre := 18
			if animated {
				genre = 16
			}
			raw, _ := json.Marshal(map[string]any{"id": 200, "name": "Show", "original_name": "Show", "first_air_date": "2026-01-01", "genres": []map[string]int{{"id": genre}}, "origin_country": []string{"JP"}, "seasons": []map[string]int{{"season_number": 1, "episode_count": 3}}})
			return jsonResponse(r, string(raw)), nil
		}
		return base.RoundTrip(r)
	})
	var err error
	f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	input.TargetID = subscriptionResourceKey("target:" + tv + ":TV")
	if configured && animated {
		input.TargetID = subscriptionResourceKey("target:" + anime + ":ANIME")
	}
	return f, source, tv, anime, input
}

func TestAnimeNativeAssignmentAndAutomaticRecognitionExecuteAllModes(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		for _, manual := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "automatic", true: "manual"}[manual], func(t *testing.T) {
				f, source, tv, anime, input := animeOrganizationFixture(t, true, true)
				input.Mode = mode
				status, plan, body := organizationPreview(t, f, input)
				if status != 200 || len(plan.Items) != 2 {
					t.Fatal(status, body)
				}
				if manual {
					for _, item := range plan.Items {
						if item.Kind == "media" {
							a := assignmentFor(item, "anime", "200")
							season, episode := 1, 1
							a.Season, a.Episode = &season, &episode
							input.Assignments = []organizationMediaAssignment{a}
						}
					}
					status, plan, body = organizationPreview(t, f, input)
					if status != 200 {
						t.Fatal(status, body)
					}
				}
				for _, item := range plan.Items {
					if item.Status != "available" || item.MediaType != "动漫" || item.Category != "Animation" || !strings.HasPrefix(item.Target, "Animation/Show (2026)/Season 01/Show.S01E01") {
						t.Fatal(item)
					}
				}
				job := organizationCreateJob(t, f, input, plan)
				status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
				if status != 200 {
					t.Fatal(status, body)
				}
				job = organizationReadMoveJob(t, f, job.ID)
				if job.State != "completed" || job.TargetRoot == tv {
					t.Fatal(job)
				}
				for _, item := range job.Items {
					assertOrganizationMoveBytes(t, filepath.Join(anime, item.Target), "original-media")
					if item.Assigned != manual {
						t.Fatal(item)
					}
				}
				var mediaType, category string
				if err := f.db.QueryRow(`SELECT TYPE,CATEGORY FROM TRANSFER_HISTORY`).Scan(&mediaType, &category); err != nil || mediaType != "动漫" || category != "Animation" {
					t.Fatal(mediaType, category, err)
				}
				if mode == "move" {
					if _, err := os.Lstat(filepath.Join(source, "Show.S01E01.1080p.mkv")); !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestAnimeHintDoesNotOverrideVerifiedNonAnimationTV(t *testing.T) {
	f, _, tv, anime, input := animeOrganizationFixture(t, true, false)
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Kind == "media" {
			a := assignmentFor(item, "anime", "200")
			season, episode := 1, 1
			a.Season, a.Episode = &season, &episode
			input.Assignments = []organizationMediaAssignment{a}
		}
	}
	status, plan, body = organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Status != "available" || item.MediaType != "电视剧" || !strings.HasPrefix(item.Target, "Drama/") {
			t.Fatal(item)
		}
	}
	job := organizationCreateJob(t, f, input, plan)
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		assertOrganizationMoveBytes(t, filepath.Join(tv, item.Target), "original-media")
	}
	if entries, err := os.ReadDir(anime); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestAnimeMissingDirectoryFallsBackToTVWithoutChangingIdentity(t *testing.T) {
	for _, value := range []any{nil, "", []any{}, []any{""}} {
		t.Run(organization.Digest(value)[:8], func(t *testing.T) {
			f, _, tv, _, input := animeOrganizationFixture(t, false, true)
			if err := config.NewStore(f.path).Update(map[string]any{"media.anime_path": value}); err != nil {
				t.Fatal(err)
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			for _, item := range plan.Items {
				if item.Status != "available" || item.MediaType != "动漫" || !strings.HasPrefix(item.Target, "Animation/") {
					t.Fatal(item)
				}
			}
			job := organizationCreateJob(t, f, input, plan)
			status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 {
				t.Fatal(status, body)
			}
			for _, item := range plan.Items {
				assertOrganizationMoveBytes(t, filepath.Join(tv, item.Target), "original-media")
			}
		})
	}
}

func TestAnimeFallbackRespectsTVCategoryFlagIncludingAliases(t *testing.T) {
	for _, category := range []string{"anime:\n  Animation:\n", "base: &categories\n  Drama:\ntv: *categories\nanime:\n  Animation:\n"} {
		t.Run(organization.Digest(category)[:8], func(t *testing.T) {
			f, _, _, _, input := animeOrganizationFixture(t, false, true)
			if err := os.WriteFile(filepath.Join(filepath.Dir(f.path), "anime-routing.yaml"), []byte(category), 0600); err != nil {
				t.Fatal(err)
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			for _, item := range plan.Items {
				if item.Status != "available" || item.MediaType != "动漫" || item.Category != "Animation" {
					t.Fatal(item)
				}
				prefix := "Show (2026)/"
				if strings.Contains(category, "tv:") {
					prefix = "Animation/"
				}
				if !strings.HasPrefix(item.Target, prefix) {
					t.Fatal(item)
				}
			}
		})
	}
}

func TestLegacyANIMEUsesDefaultLibraryAndExplicitConfiguredDestination(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, mode := range []string{"copy", "link", "softlink", "move"} {
			t.Run(mode+map[bool]string{true: "explicit", false: "default"}[explicit], func(t *testing.T) {
				f, source, tv, anime, _ := animeOrganizationFixture(t, true, true)
				dest := ""
				if explicit {
					dest = tv
				}
				if _, err := f.db.Exec(`CREATE TABLE TRANSFER_UNKNOWN(ID INTEGER PRIMARY KEY,PATH TEXT,DEST TEXT,MODE TEXT,STATE TEXT);INSERT INTO TRANSFER_UNKNOWN VALUES(7,?,?,'复制','N')`, source, dest); err != nil {
					t.Fatal(err)
				}
				input := map[string]any{"unknown_id": "7", "syncmod": mode, "type": "ANIME", "tmdb": "200", "confirmSourceRemoval": mode == "move"}
				input["reviewFingerprint"] = renameReviewFingerprint(t, f, input)
				w, id := legacyRenameRequest(t, f, input, false)
				if w.Code != 200 || id == "" {
					t.Fatal(w.Code, w.Body.String())
				}
				job := organizationReadMoveJob(t, f, id)
				if job.State != "completed" || job.LegacySelection.Kind != "anime" || job.LegacyTargetOverride != explicit || unknownState(t, f, 7) != "Y" {
					t.Fatal(job)
				}
				root := anime
				if explicit {
					root = tv
				}
				for _, item := range job.Items {
					if item.MediaType != "动漫" {
						t.Fatal(item)
					}
					assertOrganizationMoveBytes(t, filepath.Join(root, item.Target), "original-media")
				}
			})
		}
	}
}

func TestLegacyANIMECustomAndRedoUseNativeMetadataNotPython(t *testing.T) {
	for _, action := range []string{"custom", "redo"} {
		t.Run(action, func(t *testing.T) {
			f, source, _, anime, _ := animeOrganizationFixture(t, true, true)
			var id string
			if action == "custom" {
				w, jobID := legacyCustomRequest(t, f, map[string]any{"inpath": source, "outpath": anime, "syncmod": "copy", "type": "ANIME", "tmdb": 200})
				if w.Code != 200 || jobID == "" {
					t.Fatal(w.Code, w.Body.String())
				}
				id = jobID
			} else {
				if _, err := f.db.Exec(`CREATE TABLE TRANSFER_UNKNOWN(ID INTEGER PRIMARY KEY,PATH TEXT,DEST TEXT,MODE TEXT,STATE TEXT);INSERT INTO TRANSFER_UNKNOWN VALUES(7,?,'','复制','N')`, source); err != nil {
					t.Fatal(err)
				}
				status, success, results, body := legacyRedoRequest(t, f, "unidentification", []int64{7}, false)
				if status != 200 || !success || len(results) != 1 {
					t.Fatal(status, body)
				}
				id = results[0].JobID
			}
			job := organizationReadMoveJob(t, f, id)
			if job.State != "completed" {
				t.Fatal(job)
			}
			for _, item := range job.Items {
				assertOrganizationMoveBytes(t, filepath.Join(anime, item.Target), "original-media")
			}
		})
	}
}

func TestAnimeRootsAreDistinctAndOldReviewedRootIDStillExecutes(t *testing.T) {
	f, _, _, anime, input := animeOrganizationFixture(t, true, true)
	w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"ANIME"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	job := organizationCreateJob(t, f, input, plan)
	var raw string
	if err := f.db.QueryRow(`SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var definition organization.Definition
	if err := json.Unmarshal([]byte(raw), &definition); err != nil {
		t.Fatal(err)
	}
	definition.TargetID = subscriptionResourceKey("target:" + anime + ":TV") // Old binaries used TV IDs for anime roots.
	encoded, _ := json.Marshal(definition)
	if _, err := f.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET DEFINITION=? WHERE ID=?`, string(encoded), job.ID); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range job.Items {
		assertOrganizationMoveBytes(t, filepath.Join(anime, item.Target), "original-media")
	}
}

func TestAnimeNativeTargetMismatchAndInvalidRulesCannotBeBypassed(t *testing.T) {
	f, _, tv, _, input := animeOrganizationFixture(t, true, true)
	input.TargetID = subscriptionResourceKey("target:" + tv + ":TV")
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Status == "available" || item.Kind == "media" && item.Status != "blocked" {
			t.Fatal(item)
		}
	}
	raw, _ := json.Marshal(input)
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/plan", f.token, strings.TrimSuffix(string(raw), "}")+`,"LegacyTargetOverride":true}`)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, item := range plan.Items {
		if item.Kind == "media" {
			input.Assignments = []organizationMediaAssignment{assignmentFor(item, "anime", "200")}
		}
	}
	status, _, body = organizationPreview(t, f, input)
	if status != 422 {
		t.Fatal(status, body)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.anime_path": []any{7}}); err != nil {
		t.Fatal(err)
	}
	w = nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
	if w.Code == 200 {
		t.Fatal(w.Body.String())
	}
}

func TestAnimeInventoryDirectoryRoutingAndEmptyArrayFallback(t *testing.T) {
	var detail tmdbMediaDetails
	if err := json.Unmarshal([]byte(`{"id":200,"name":"Show","first_air_date":"2026-01-01","genres":[{"id":16}],"seasons":[{"season_number":1,"episode_count":3}]}`), &detail); err != nil {
		t.Fatal(err)
	}
	// fetchNativeTMDBDetails preserves the raw attributes separately from its
	// typed fields; plain json.Unmarshal intentionally does not populate them.
	detail.Attributes = map[string]any{"genres": []any{map[string]any{"id": 16}}}
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "fallback", true: "anime"}[configured], func(t *testing.T) {
			f, _, tv, anime, _ := animeOrganizationFixture(t, configured, true)
			if err := config.NewStore(f.path).Update(map[string]any{"media.media_server": ""}); err != nil {
				t.Fatal(err)
			}
			root := tv
			if configured {
				root = anime
			}
			organizationFile(t, root, "Animation/Show (2026)/Season 01/Show.S01E01.mkv")
			meta, err := mediameta.Parse("Show.S01E01", "")
			if err != nil {
				t.Fatal(err)
			}
			service := subscriptionService{configStore: config.NewStore(f.path), client: &http.Client{Transport: f.transport}}
			coverage, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv", "Animation")
			if err != nil || !coverage.Complete {
				t.Fatal(coverage, err)
			}
		})
	}
}

func TestAnimationMoviesRemainMoviesAndAnimeMetadataChangesInvalidatePreview(t *testing.T) {
	t.Run("movie", func(t *testing.T) {
		f, _, _, _, input := animeOrganizationFixture(t, true, true)
		status, plan, body := organizationPreview(t, f, input)
		if status != 200 {
			t.Fatal(status, body)
		}
		base := f.transport
		f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/3/movie/100" {
				return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","genres":[{"id":16}]}`), nil
			}
			return base.RoundTrip(r)
		})
		var err error
		f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
		if err != nil {
			t.Fatal(err)
		}
		w := nativeJSONRequest(f.handler, "GET", "/api/v1/organization/roots", f.token, "")
		var roots struct{ Data organizationRoots }
		if json.Unmarshal(w.Body.Bytes(), &roots) != nil {
			t.Fatal(w.Body.String())
		}
		for _, root := range roots.Data.Targets {
			if root.Type == "MOV" {
				input.TargetID = root.ID
			}
		}
		for _, item := range plan.Items {
			if item.Kind == "media" {
				input.Assignments = []organizationMediaAssignment{assignmentFor(item, "movie", "100")}
			}
		}
		status, plan, body = organizationPreview(t, f, input)
		if status != 200 {
			t.Fatal(status, body)
		}
		for _, item := range plan.Items {
			if item.Status != "available" || item.MediaType != "电影" || !strings.HasPrefix(item.Target, "Movies/") {
				t.Fatal(item)
			}
		}
	})
	t.Run("changed genre", func(t *testing.T) {
		f, _, _, _, input := animeOrganizationFixture(t, true, true)
		status, plan, body := organizationPreview(t, f, input)
		if status != 200 {
			t.Fatal(status, body)
		}
		base := f.transport
		f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/3/tv/200" {
				return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","genres":[{"id":18}],"seasons":[{"season_number":1,"episode_count":3}]}`), nil
			}
			return base.RoundTrip(r)
		})
		var err error
		f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(struct {
			organizationPlanInput
			Fingerprint string `json:"fingerprint"`
		}{input, plan.Fingerprint})
		w := nativeJSONRequest(f.handler, "POST", "/api/v1/organization/jobs", f.token, string(raw))
		if w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
		var count int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); err != nil || count != 0 {
			t.Fatal(count, err)
		}
	})
}

func TestAnimeBatchSelectionAndDisabledLibrarySafety(t *testing.T) {
	f, _, tv, anime, input := animeOrganizationFixture(t, true, true)
	season := 1
	input.EpisodeSelection = &organization.EpisodeSelection{TMDBID: "200", Season: &season, EpisodeFormat: "Show.S01E{ep}.1080p.mkv"}
	status, plan, body := organizationPreview(t, f, input)
	if status != 200 {
		t.Fatal(status, body)
	}
	for _, item := range plan.Items {
		if item.Status != "available" || item.MediaType != "动漫" {
			t.Fatal(item)
		}
	}
	job := organizationCreateJob(t, f, input, plan)
	if err := config.NewStore(f.path).Update(map[string]any{"media.anime_path": []any{}}); err != nil {
		t.Fatal(err)
	}
	status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
	if status != 409 {
		t.Fatal(status, body)
	}
	for _, root := range []string{tv, anime} {
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			t.Fatal(entries, err)
		}
	}
}

func TestAnimeFallbackWrittenMediaAndInventoryUseSameEffectiveCategory(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "bare TV fallback", true: "categorized TV fallback"}[enabled], func(t *testing.T) {
			f, _, tv, _, input := animeOrganizationFixture(t, false, true)
			category := "anime:\n  Animation:\n"
			if enabled {
				category = "tv:\n  Drama:\n" + category
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(f.path), "anime-routing.yaml"), []byte(category), 0600); err != nil {
				t.Fatal(err)
			}
			if err := config.NewStore(f.path).Update(map[string]any{"media.media_server": ""}); err != nil {
				t.Fatal(err)
			}
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			job := organizationCreateJob(t, f, input, plan)
			status, _, body = organizationMoveRequest(t, f, job.ID, "execute")
			if status != 200 {
				t.Fatal(status, body)
			}
			for _, item := range plan.Items {
				assertOrganizationMoveBytes(t, filepath.Join(tv, item.Target), "original-media")
			}
			service := f.runner.recognition.service
			service.client = &http.Client{Transport: f.transport}
			recognition := mediaNameAPI{service: service, categories: mediaCategoryAPI{config: service.configStore, configPath: f.path}}
			result, failure := recognition.recognize(t.Context(), "Show.S01E01.1080p.mkv", "")
			if failure != nil || result == nil {
				t.Fatal(failure)
			}
			if text(result.data["category"]) != "Animation" || enabled != (result.libraryCategory == "Animation") {
				t.Fatal(result.data, result.libraryCategory)
			}
			coverage, err := service.nativeMediaExistence(t.Context(), result.meta, result.detail, result.kind, result.libraryCategory)
			if err != nil || !coverage.Complete {
				t.Fatal(coverage, err)
			}
		})
	}
}
