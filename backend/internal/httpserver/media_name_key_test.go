package httpserver

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestNativeMediaInfoAPIKeyAuthenticationAndRotation(t *testing.T) {
	calls := 0
	handler, jwt, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "tmdb.test" {
			t.Fatalf("unexpected Python request: %s", request.URL)
		}
		return jsonResponse(request, `{"results":[]}`), nil
	}))
	store := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-secret", "app.tmdb_domain": "tmdb.test", "security.api_key": "key.with.dots", "security.check_apikey": false}); err != nil {
		t.Fatal(err)
	}
	perform := func(header, key string, want int) {
		t.Helper()
		query := url.Values{"name": {"Movie"}}
		if key != "" {
			query.Set("apikey", key)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/service/mediainfo?"+query.Encode(), nil)
		request.Header.Set("Authorization", header)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want || strings.Contains(response.Body.String(), "server-secret") || strings.Contains(response.Body.String(), "key.with.dots") {
			t.Fatalf("authentication=%d %s want=%d", response.Code, response.Body.String(), want)
		}
	}
	perform("", "", 401)
	perform(jwt, "", 401)
	perform("wrong", "wrong", 401)
	if calls != 0 {
		t.Fatal("rejected credentials reached TMDB")
	}
	perform("key.with.dots", "", 200)
	perform("Bearer key.with.dots", "", 200)
	perform("", "key.with.dots", 200)
	perform("wrong", "key.with.dots", 200)
	if calls != 8 {
		t.Fatalf("native search calls=%d want=8", calls)
	}
	if err := store.Update(map[string]any{"security.api_key": "rotated-key"}); err != nil {
		t.Fatal(err)
	}
	perform("key.with.dots", "", 401)
	perform("rotated-key", "", 200)
	if err := store.Update(map[string]any{"security.api_key": ""}); err != nil {
		t.Fatal(err)
	}
	perform("", "", 401)
	perform("rotated-key", "", 401)
	before := calls
	request := httptest.NewRequest(http.MethodPost, "/api/v1/service/mediainfo", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 405 || calls != before {
		t.Fatalf("unsupported method=%d calls=%d", response.Code, calls)
	}
}

func TestNativeMediaInfoGETReturnsVerifiedIdentityWithoutForwardingClientKey(t *testing.T) {
	handler, _, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "tmdb.test" || request.URL.Query().Get("apikey") != "" || request.Header.Get("Authorization") != "" || request.URL.Query().Get("api_key") != "server-secret" {
			t.Fatalf("unexpected identity request: %s", request.URL.Path)
		}
		switch request.URL.Path {
		case "/3/search/movie":
			return jsonResponse(request, `{"results":[{"id":100,"title":"Movie","release_date":"2025-01-01"}]}`), nil
		case "/3/movie/100":
			return jsonResponse(request, `{"id":100,"title":"Movie","release_date":"2025-01-01"}`), nil
		default:
			t.Fatalf("unexpected endpoint=%s", request.URL.Path)
		}
		return nil, nil
	}))
	if err := config.NewStore(filepath.Join(filepath.Dir(path), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "server-secret", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/service/mediainfo?"+url.Values{"name": {"Movie.2025.1080p"}, "apikey": {"native-services-secret"}}.Encode(), nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"tmdbid":100`) || !strings.Contains(response.Body.String(), `"pix":"1080p"`) || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("verified GET=%d %s", response.Code, response.Body.String())
	}
}
