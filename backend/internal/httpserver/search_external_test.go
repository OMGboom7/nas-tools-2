package httpserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestQuickExternalSearchAndDownloadEndToEndWithoutPython(t *testing.T) {
	searchCalls, downloadCalls := 0, 0
	failed := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "indexer.local":
			if r.Header.Get("X-Api-Key") != "private-key" {
				t.Fatal("missing API key")
			}
			if failed {
				response := jsonResponse(r, "bad")
				response.StatusCode = 500
				return response, nil
			}
			if r.URL.Path == "/api/v1/indexerstats" {
				return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
			}
			if r.URL.Path != "/api/v1/search" || r.URL.Query().Get("query") != "电影 & Query" {
				t.Fatalf("bad search %s", r.URL)
			}
			searchCalls++
			return jsonResponse(r, `[{"indexerId":42,"title":"Movie.2026.1080p.WEB-DL","size":1234567,"seeders":null,"guid":"https://tracker.local/details?private=secret","downloadUrl":"`+testMagnet+`"}]`), nil
		case "client.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				return jsonResponse(r, "Ok."), nil
			}
			if r.URL.Path != "/api/v2/torrents/add" || r.ParseForm() != nil || r.PostForm.Get("urls") != testMagnet {
				t.Fatalf("bad download %s", r.URL)
			}
			downloadCalls++
			return jsonResponse(r, "Ok."), nil
		default:
			t.Fatalf("unexpected request (Python prohibited): %s", r.URL)
			return nil, nil
		}
	})
	downloader := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"client.local","port":8080,"username":"admin","password":"secret"}`}
	handler, token, path := nativeServicesFixture(t, "", &downloader, transport)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for key, value := range map[string]string{"UserInstalledPlugins": `["Prowlarr"]`, "UserIndexerSites": `["Selected-prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-key"}`} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	result := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"电影 & Query","quick":true}`)
	var payload struct{ Data searchData }
	if result.Code != 200 || json.Unmarshal(result.Body.Bytes(), &payload) != nil || payload.Data.Total != 1 || len(payload.Data.Items) != 1 || searchCalls != 1 {
		t.Fatalf("search=%d %s", result.Code, result.Body.String())
	}
	if strings.Contains(result.Body.String(), "private-key") || strings.Contains(result.Body.String(), "private=secret") || strings.Contains(result.Body.String(), "magnet:") {
		t.Fatal("search leaked private links")
	}
	resource := payload.Data.Items[0].Resources[0]
	if !strings.HasPrefix(resource.ID, searchcache.Prefix) || resource.PromotionKnown == nil || *resource.PromotionKnown || resource.SeedersKnown == nil || *resource.SeedersKnown || resource.Resolution != "1080p" {
		t.Fatalf("resource=%+v", resource)
	}
	body := `{"resourceId":"` + resource.ID + `"}`
	for i := 0; i < 2; i++ {
		w := nativeJSONRequest(handler, "POST", "/api/v1/downloads/resource", token, body)
		if w.Code != 200 {
			t.Fatalf("download=%d %s", w.Code, w.Body.String())
		}
	}
	if downloadCalls != 1 {
		t.Fatalf("duplicate submissions=%d", downloadCalls)
	}
	created := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"other"}, "password": {"strong-password"}, "pris": {"下载管理,资源搜索"}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	other := loginForTest(t, handler, "other", "strong-password")
	w := nativeJSONRequest(handler, "POST", "/api/v1/downloads/resource", other, body)
	if w.Code != 404 {
		t.Fatalf("other user accessed private resource: %d %s", w.Code, w.Body.String())
	}
	failed = true
	w = nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"电影 & Query","quick":true}`)
	if w.Code != 502 || strings.Contains(w.Body.String(), "private-key") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestNormalExternalSearchGroupsVerifiedMedia(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "indexer.local":
			if r.URL.Path == "/api/v1/indexerstats" {
				return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
			}
			if r.URL.Path != "/api/v1/search" {
				t.Fatal(r.URL)
			}
			return jsonResponse(r, `[{"indexerId":42,"title":"Movie.2026.1080p.WEB-DL","size":1000,"seeders":3,"downloadUrl":"`+testMagnet+`"},{"indexerId":42,"title":"Movie.2026.2160p.WEB-DL","size":2000,"seeders":9,"downloadUrl":"`+testMagnet+`"}]`), nil
		case "tmdb.test":
			if r.URL.Query().Get("api_key") != "metadata-secret" {
				t.Fatal("missing TMDB credentials")
			}
			switch r.URL.Path {
			case "/3/search/movie":
				return jsonResponse(r, `{"results":[{"id":100,"title":"Movie","release_date":"2026-01-01"}]}`), nil
			case "/3/movie/100":
				return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","overview":"Verified description","vote_average":8.2,"poster_path":"/poster.jpg"}`), nil
			default:
				t.Fatal(r.URL)
			}
		default:
			t.Fatalf("unexpected upstream (Python prohibited): %s", r.URL)
		}
		return nil, nil
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for key, value := range map[string]string{"UserInstalledPlugins": `["Prowlarr"]`, "UserIndexerSites": `["Selected-prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-key"}`} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "metadata-secret", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	w := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":false}`)
	var payload struct{ Data searchData }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &payload) != nil || len(payload.Data.Items) != 1 || payload.Data.Total != 2 {
		t.Fatalf("normal=%d %s", w.Code, w.Body.String())
	}
	media := payload.Data.Items[0]
	if media.Key != "movie:100" || media.Title != "Movie" || media.Year != "2026" || media.TMDBID != "100" || media.Overview != "Verified description" || media.Vote != "8.2" || len(media.Resources) != 2 || media.Resources[0].Seeders != 9 || media.Resources[0].ID == media.Resources[1].ID {
		t.Fatalf("media=%+v", media)
	}
	if media.ExistsKnown == nil || *media.ExistsKnown || media.Exists {
		t.Fatal("unqueried inventory presented as known")
	}
	if strings.Contains(w.Body.String(), "metadata-secret") || strings.Contains(w.Body.String(), "private-key") || strings.Contains(w.Body.String(), "magnet:") {
		t.Fatal("credentials leaked")
	}
	if len(payload.Data.Warnings) != 1 {
		t.Fatal("unavailable inventory warning missing")
	}
	root := t.TempDir()
	configuration := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml"))
	if err := configuration.Update(map[string]any{"media.movie_path": root}); err != nil {
		t.Fatal(err)
	}
	check := func(known, exists bool) {
		t.Helper()
		w := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":false}`)
		var result struct{ Data searchData }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data.Items) != 1 {
			t.Fatalf("inventory search=%d %s", w.Code, w.Body.String())
		}
		media := result.Data.Items[0]
		if media.ExistsKnown == nil || *media.ExistsKnown != known || media.Exists != exists {
			t.Fatalf("known=%t exists=%t result=%+v", known, exists, media)
		}
		for _, resource := range media.Resources {
			if resource.ExistsKnown == nil || *resource.ExistsKnown != known || resource.Exists == nil || *resource.Exists != exists {
				t.Fatalf("resource inventory=%+v", resource)
			}
		}
		if known && len(result.Data.Warnings) != 0 || !known && len(result.Data.Warnings) != 1 {
			t.Fatalf("warnings=%v", result.Data.Warnings)
		}
		if strings.Contains(w.Body.String(), root) {
			t.Fatal("private library directory leaked")
		}
	}
	check(true, false)
	movieDirectory := filepath.Join(root, "Movie (2026)")
	if err := os.MkdirAll(movieDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(movieDirectory, "Movie.mkv"), []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	check(true, true)
	if err := configuration.Update(map[string]any{"media.movie_path": filepath.Join(root, "unmounted")}); err != nil {
		t.Fatal(err)
	}
	check(false, false)
}

func TestSearchTVInventoryDoesNotConflateDifferentEpisodes(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "Show (2026)", "Season 1")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "Show.S01E01.mkv"), []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "indexer.local" {
			if r.URL.Path == "/api/v1/indexerstats" {
				return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
			}
			return jsonResponse(r, `[{"indexerId":42,"title":"Show.2026.S01E01.1080p","size":1000,"seeders":3,"downloadUrl":"`+testMagnet+`"},{"indexerId":42,"title":"Show.2026.S01E02.1080p","size":1000,"seeders":2,"downloadUrl":"`+testMagnet+`"}]`), nil
		}
		if r.URL.Hostname() == "tmdb.test" {
			switch r.URL.Path {
			case "/3/search/tv":
				return jsonResponse(r, `{"results":[{"id":200,"name":"Show","first_air_date":"2026-01-01"}]}`), nil
			case "/3/tv/200":
				return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2026-01-01","seasons":[{"season_number":1,"episode_count":2}]}`), nil
			}
		}
		t.Fatalf("unexpected upstream: %s", r.URL)
		return nil, nil
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	configuration := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml"))
	if err := configuration.Update(map[string]any{"app.rmt_tmdbkey": "metadata-secret", "app.tmdb_domain": "tmdb.test", "media.tv_path": root}); err != nil {
		t.Fatal(err)
	}
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for key, value := range map[string]string{"UserInstalledPlugins": `["Prowlarr"]`, "UserIndexerSites": `["Selected-prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-key"}`} {
		if err := store.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	w := nativeJSONRequest(handler, "POST", "/api/v1/search/resources", token, `{"keyword":"Show","quick":false}`)
	var result struct{ Data searchData }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Data.Items) != 1 {
		t.Fatalf("TV search=%d %s", w.Code, w.Body.String())
	}
	media := result.Data.Items[0]
	if media.ExistsKnown == nil || !*media.ExistsKnown || media.Exists || len(media.Resources) != 2 {
		t.Fatalf("group=%+v", media)
	}
	if media.Resources[0].Exists == nil || !*media.Resources[0].Exists || media.Resources[1].Exists == nil || *media.Resources[1].Exists {
		t.Fatalf("episode coverage conflated: %+v", media.Resources)
	}
}
