package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestNativeMediaNameRecognitionCombinesWordsMetadataIdentityAndCategory(t *testing.T) {
	var queries []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "tmdb.test" {
			t.Fatalf("unexpected Python request: %s", request.URL)
		}
		switch request.URL.Path {
		case "/3/search/tv":
			queries = append(queries, request.URL.Query().Get("query"))
			return jsonResponse(request, `{"results":[{"id":100,"name":"AA","first_air_date":"2025-01-01"}]}`), nil
		case "/3/tv/100":
			return jsonResponse(request, `{"id":100,"name":"AA","first_air_date":"2025-01-01","genres":[{"id":16}],"origin_country":["CN"],"original_language":"zh"}`), nil
		default:
			t.Fatalf("unexpected endpoint: %s", request.URL)
		}
		return nil, nil
	})
	handler, token, path := nativeServicesFixture(t, "media:\n  category: custom\n  anime_path: /anime\n", nil, transport)
	if err := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "secret-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "custom.yaml"), []byte("anime:\n  国漫:\n    genre_ids: '16'\n    origin_country: 'CN'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	word := url.Values{"id": {"0"}, "gid": {"-1"}, "group_type": {"1"}, "type": {"2"}, "enabled": {"1"}, "regex": {"0"}, "new_replaced": {"A"}, "new_replace": {"AA"}}
	if response := performFormRequest(handler, "/api/v1/words/item/update", token, word); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	offset := url.Values{"id": {"0"}, "gid": {"-1"}, "group_type": {"1"}, "type": {"4"}, "enabled": {"1"}, "new_front": {"S02E"}, "new_back": {""}, "new_offset": {"EP+1"}}
	if response := performFormRequest(handler, "/api/v1/words/item/update", token, offset); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response := performFormRequest(handler, "/api/v1/service/name/test", token, url.Values{"name": {"[WiKi] A.2025.1080p.WEB-DL.H264.S02E01-E03"}})
	var payload struct {
		Code int
		Data map[string]any
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != 200 || payload.Code != 0 {
		t.Fatalf("recognition=%d %s err=%v", response.Code, response.Body.String(), err)
	}
	for key, want := range map[string]string{"name": "AA", "title": "AA", "type": "动漫", "category": "国漫", "season_episode": "S02E02-E04", "pix": "1080p", "team": "WiKi", "restype": "WEB-DL", "tmdb_S_E_link": "https://www.themoviedb.org/tv/100/season/2/episode/2"} {
		if payload.Data[key] != want {
			t.Errorf("%s=%v want=%q", key, payload.Data[key], want)
		}
	}
	if len(queries) != 1 || queries[0] != "AA" {
		t.Fatalf("words applied twice: queries=%v", queries)
	}
	if values, ok := payload.Data["offset_words"].([]any); !ok || len(values) != 1 {
		t.Fatalf("offset diagnostics=%v", payload.Data["offset_words"])
	}
}

func TestNativeNameRecognitionFailureNeverFallsBackToPython(t *testing.T) {
	handler, err := newHandler(config.Config{DisableLegacy: true}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected network request %s", request.URL)
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/service/name/test", strings.NewReader("name=Movie"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatalf("no native configuration=%d %s", response.Code, response.Body.String())
	}
	handler, token, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "tmdb.test" {
			t.Fatalf("unexpected Python request=%s", request.URL)
		}
		result := jsonResponse(request, `{"error":"secret-key"}`)
		result.StatusCode = 500
		return result, nil
	}))
	if err := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "secret-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/service/name/test", token, url.Values{"name": {"Movie"}})
	if response.Code != 502 || strings.Contains(response.Body.String(), "secret-key") {
		t.Fatalf("upstream failure=%d %s", response.Code, response.Body.String())
	}
}

func TestNativeNameRecognitionNoMatchAuthenticationAndInputValidation(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "tmdb.test" || request.URL.Path != "/3/search/movie" && request.URL.Path != "/3/search/tv" {
			t.Fatalf("unexpected legacy request=%s", request.URL)
		}
		return jsonResponse(request, `{"results":[]}`), nil
	})
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	if err := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "secret-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(handler, "/api/v1/service/name/test", "", url.Values{"name": {"Movie"}}); response.Code != 401 {
		t.Fatal(response.Code)
	}
	if calls != 0 {
		t.Fatal("unauthenticated request called upstream")
	}
	response := performFormRequest(handler, "/api/v1/service/name/test", token, url.Values{"name": {"Movie"}})
	var payload struct{ Data struct{ Name string } }
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != 200 || payload.Data.Name != "无法识别" || calls != 2 {
		t.Fatalf("no match=%d %s calls=%d err=%v", response.Code, response.Body.String(), calls, err)
	}
	if response := performFormRequest(handler, "/api/v1/service/name/test", token, url.Values{"name": {"Show.E10-E01"}}); response.Code != 400 {
		t.Fatal(response.Body.String())
	}
	if calls != 2 {
		t.Fatal("invalid metadata called upstream")
	}
}
