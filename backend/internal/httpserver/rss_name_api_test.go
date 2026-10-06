package httpserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestRSSNameTestUsesNativeRecognitionFiltersAndRealInventory(t *testing.T) {
	missingMovie, failInventory := false, false
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host == "tmdb.test" {
			switch request.URL.Path {
			case "/3/search/movie":
				return jsonResponse(request, `{"results":[{"id":100,"title":"Movie","release_date":"2025-01-01"}]}`), nil
			case "/3/movie/100":
				return jsonResponse(request, `{"id":100,"title":"Movie","release_date":"2025-01-01"}`), nil
			case "/3/search/tv":
				return jsonResponse(request, `{"results":[{"id":200,"name":"Show","first_air_date":"2025-01-01"}]}`), nil
			case "/3/tv/200":
				if request.URL.Query().Get("language") == "en" {
					return jsonResponse(request, `{"id":200,"name":"English Show","external_ids":{"id":200,"imdb_id":"tt200"}}`), nil
				}
				return jsonResponse(request, `{"id":200,"name":"Show","first_air_date":"2025-01-01","external_ids":{"id":200,"imdb_id":"tt200"},"seasons":[{"season_number":1,"episode_count":3}]}`), nil
			}
		}
		if request.URL.Host == "emby.test" {
			if failInventory {
				result := jsonResponse(request, `{"error":"secret"}`)
				result.StatusCode = 500
				return result, nil
			}
			switch request.URL.Path {
			case "/emby/Items":
				if request.URL.Query().Get("IncludeItemTypes") == "Series" {
					return jsonResponse(request, `{"Items":[{"Id":"series","Name":"Show","ProductionYear":2025,"ProviderIds":{"Tmdb":"200"}}]}`), nil
				}
				if missingMovie {
					return jsonResponse(request, `{"Items":[]}`), nil
				}
				return jsonResponse(request, `{"Items":[{"Id":"movie","Name":"Movie","ProductionYear":2025,"ProviderIds":{"Tmdb":"100"}}]}`), nil
			case "/emby/Shows/series/Episodes":
				return jsonResponse(request, `{"Items":[{"ParentIndexNumber":1,"IndexNumber":1,"IndexNumberEnd":2}]}`), nil
			}
		}
		t.Fatalf("unexpected Python/network request=%s", request.URL.Path)
		return nil, nil
	})
	handler, token, path := nativeServicesFixture(t, "media:\n  media_server: emby\nemby:\n  host: http://emby.test\n  api_key: library-secret\n", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "tmdb-secret", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,USES,INCLUDE,EXCLUDE,FILTER,PROCESS_COUNT,MEDIAINFOS) VALUES (1,'Test','D','NoMatch','','-1','0','[]')`); err != nil {
		t.Fatal(err)
	}
	check := func(title string, matched, exists bool) {
		t.Helper()
		response := performFormRequest(handler, "/api/v1/rss/name/test", token, url.Values{"taskid": {"1"}, "title": {title}})
		var payload struct {
			Data struct {
				Match  bool `json:"match_flag"`
				Exists bool `json:"exist_flag"`
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != 200 || payload.Data.Match != matched || payload.Data.Exists != exists {
			t.Fatalf("test %q=%d %s err=%v", title, response.Code, response.Body.String(), err)
		}
	}
	check("Movie.2025.1080p", false, true)
	if _, err := database.Exec(`UPDATE CONFIG_USER_RSS SET INCLUDE='Movie',EXCLUDE='CAM' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	missingMovie = true
	check("Movie.2025.1080p", true, false)
	if _, err := database.Exec(`UPDATE CONFIG_USER_RSS SET INCLUDE='' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	check("Show.S01", true, false)
	check("Show.S01E01-E02", true, true)
	if response := performFormRequest(handler, "/api/v1/rss/name/test", token, url.Values{"taskid": {"1"}, "title": {"Show.S02"}}); response.Code != 422 {
		t.Fatalf("unknown totals=%d %s", response.Code, response.Body.String())
	}
	failInventory = true
	if response := performFormRequest(handler, "/api/v1/rss/name/test", token, url.Values{"taskid": {"1"}, "title": {"Movie.2025"}}); response.Code != 502 {
		t.Fatalf("inventory failure=%d %s", response.Code, response.Body.String())
	}
	failInventory = false
	if err := store.Update(map[string]any{"media.media_server": ""}); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/rss/name/test", token, url.Values{"taskid": {"1"}, "title": {"Movie.2025"}}); response.Code != 503 {
		t.Fatalf("local fallback=%d %s", response.Code, response.Body.String())
	}
	localRoot := t.TempDir()
	if err := store.Update(map[string]any{"media.movie_path": localRoot, "media.tv_path": localRoot}); err != nil {
		t.Fatal(err)
	}
	check("Movie.2025", true, false)
	for _, relative := range []string{"Movie (2025)/Movie.mkv", "Show (2025)/Season 1/Show.S01E01-E02.strm"} {
		localPath := filepath.Join(localRoot, relative)
		if err := os.MkdirAll(filepath.Dir(localPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(localPath, []byte("media"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check("Movie.2025", true, true)
	check("Show.S01", true, false)
	check("Show.S01E01-E02", true, true)
	if err := store.Update(map[string]any{"media.media_server": "emby"}); err != nil {
		t.Fatal(err)
	}
	failInventory = true
	check("Movie.2025", true, true)
	check("Show.S01", true, false)
	check("Show.S01E01-E02", true, true)
	failInventory = false
	check("Movie.2025", true, false) // The successful empty server response remains authoritative.
	failInventory = true
	if err := store.Update(map[string]any{"media.movie_path": filepath.Join(localRoot, "unmounted")}); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/rss/name/test", token, url.Values{"taskid": {"1"}, "title": {"Movie.2025"}}); response.Code != 502 {
		t.Fatalf("server and local failure=%d %s", response.Code, response.Body.String())
	}
	if err := store.Update(map[string]any{"media.tv_name_format": "{en_title} ({year})/{imdbid}/Season {season:0>2}/unused"}); err != nil {
		t.Fatal(err)
	}
	englishDirectory := filepath.Join(localRoot, "English Show (2025)", "tt200", "Season 01")
	if err := os.MkdirAll(englishDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(englishDirectory, "Show.S01E01-E02.mkv"), []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	check("Show.S01", true, false)
	check("Show.S01E01-E02", true, true)
	before := calls
	if response := performFormRequest(handler, "/api/v1/rss/name/test", "", url.Values{"taskid": {"1"}, "title": {"Movie"}}); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if response := performFormRequest(handler, "/api/v1/rss/name/test", token, url.Values{"taskid": {"999"}, "title": {"Movie"}}); response.Code != 404 {
		t.Fatal(response.Code)
	}
	if calls != before {
		t.Fatal("unauthorized or nonexistent task called upstream")
	}
	var count int
	var processed string
	if err := database.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("name test mutated RSS processing: %d %v", count, err)
	}
	if err := database.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=1`).Scan(&processed); err != nil || processed != "0" {
		t.Fatalf("name test changed counter=%s %v", processed, err)
	}
}
