package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestDiscoveryReadsTMDBAndNativeSubscriptionState(t *testing.T) {
	t.Parallel()
	seen := map[string]int{}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Host != "tmdb.test" || request.URL.Query().Get("api_key") != "server-only-key" || request.URL.Query().Get("language") != "zh" || request.URL.Query().Get("page") != "2" {
			t.Errorf("unexpected TMDB request: %s", request.URL.String())
		}
		seen[request.URL.Path]++
		body := `{"results":[{"id":100,"title":"热门电影","release_date":"2026-01-02","poster_path":"/poster.jpg","backdrop_path":"/backdrop.jpg","vote_average":8.23,"overview":"简介"}]}`
		if request.URL.Path == "/3/tv/popular" || request.URL.Path == "/3/tv/on_the_air" {
			body = `{"results":[{"id":200,"name":"热门剧集","first_air_date":"2025-02-03","poster_path":"/tv.jpg","vote_average":7.8}]}`
		}
		if request.URL.Path == "/3/trending/all/week" {
			body = `{"results":[{"id":200,"media_type":"tv","name":"热门剧集","first_air_date":"2025-02-03"},{"id":999,"media_type":"person","name":"演员"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-only-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, TMDBID TEXT)`,
		`CREATE TABLE RSS_TVS (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, TMDBID TEXT)`,
		`INSERT INTO RSS_MOVIES (ID, NAME, YEAR, TMDBID) VALUES (7, '热门电影', '2026', '100')`,
		`INSERT INTO RSS_TVS (ID, NAME, YEAR, TMDBID) VALUES (8, '热门剧集', '2025', '200')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct{ category, path, kind string }{
		{"popular-movies", "/3/movie/popular", "MOV"},
		{"popular-series", "/3/tv/popular", "TV"},
		{"new-movies", "/3/movie/now_playing", "MOV"},
		{"new-series", "/3/tv/on_the_air", "TV"},
		{"trending", "/3/trending/all/week", "TV"},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/discovery", strings.NewReader(`{"category":"`+testCase.category+`","page":2}`))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "server-only-key") {
			t.Fatalf("%s: status = %d: %s", testCase.category, response.Code, response.Body.String())
		}
		var result struct {
			Data discoveryData `json:"data"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if len(result.Data.Items) != 1 || !result.Data.Items[0].Subscribed || result.Data.Items[0].Type != testCase.kind || seen[testCase.path] != 1 {
			t.Fatalf("%s: unexpected discovery data: %#v", testCase.category, result.Data)
		}
		if testCase.category == "popular-movies" && (result.Data.Items[0].Image == "" || result.Data.Items[0].Image == "https://image.tmdb.org/t/p/w500/poster.jpg" || result.Data.Items[0].Vote != "8.2") {
			t.Fatalf("image/vote not normalized: %#v", result.Data.Items[0])
		}
	}
}

func TestDiscoveryMissingTMDBKeyNeverUsesLegacy(t *testing.T) {
	t.Parallel()
	handler, token, _ := nativeServicesFixture(t, "", nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/discovery", strings.NewReader(`{"category":"popular-movies","page":1}`))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "TMDB") {
		t.Fatalf("missing key = %d: %s", response.Code, response.Body.String())
	}
}

func TestDiscoverySubscriptionMatchesLegacyTitleAndYear(t *testing.T) {
	t.Parallel()
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, TMDBID TEXT)`,
		`INSERT INTO RSS_MOVIES (NAME, YEAR, TMDBID) VALUES ('旧电影', '2026', '')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct {
		kind, id, title, year string
		want                  bool
	}{
		{"MOV", "123", "旧电影", "2026", true},
		{"MOV", "123", "旧电影", "2025", false},
		{"TV", "123", "旧剧集", "2026", false},
	} {
		got, err := discoverySubscribed(context.Background(), database, testCase.kind, testCase.id, testCase.title, testCase.year)
		if err != nil || got != testCase.want {
			t.Fatalf("%+v: subscribed=%v err=%v", testCase, got, err)
		}
	}
}

func TestDiscoveryRejectsTMDBRedirectWithoutLeakingKey(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://other.example/steal"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-only-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/discovery", strings.NewReader(`{"category":"trending","page":1}`))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "server-only-key") || strings.Contains(response.Body.String(), "other.example") {
		t.Fatalf("redirect = %d: %s", response.Code, response.Body.String())
	}
}
