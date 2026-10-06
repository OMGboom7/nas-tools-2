package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestTVSeasonListUsesNativeTMDB(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.themoviedb.org" || request.URL.Path != "/3/tv/200" || request.URL.Query().Get("api_key") != "season-key" || request.URL.Query().Get("language") != "zh-CN" {
			t.Fatalf("unexpected TV request: %s", request.URL)
		}
		return jsonResponse(request, `{"id":200,"seasons":[{"season_number":0},{"season_number":1},{"season_number":10},{"season_number":11},{"season_number":20},{"season_number":101}]}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	configureTMDBSeasonTest(t, databasePath)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/media/tv/seasons", strings.NewReader(url.Values{"tmdbid": {"200"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var payload struct {
		Code    int              `json:"code"`
		Seasons []tvSeasonOption `json:"seasons"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != http.StatusOK || payload.Code != 0 {
		t.Fatalf("status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	want := []tvSeasonOption{{"第一百零一季", 101}, {"第二十季", 20}, {"第十一季", 11}, {"第十季", 10}, {"第一季", 1}}
	if !reflect.DeepEqual(payload.Seasons, want) {
		t.Fatalf("seasons=%+v, want %+v", payload.Seasons, want)
	}
}

func TestTVSeasonListRequiresLoginAndValidID(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("invalid request contacted upstream: %s", request.URL)
		return nil, nil
	})
	handler, token, _ := nativeServicesFixture(t, "", nil, transport)
	for _, test := range []struct {
		id, auth string
		status   int
	}{
		{"200", "", http.StatusUnauthorized},
		{"200", "bad", http.StatusUnauthorized},
		{"not-an-id", token, http.StatusBadRequest},
		{"", token, http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/media/tv/seasons", strings.NewReader(url.Values{"tmdbid": {test.id}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", test.auth)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("id=%q status=%d body=%s", test.id, response.Code, response.Body.String())
		}
	}
}

func TestTVSeasonListRejectsRedirectWithoutLegacyFallback(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "api.themoviedb.org" {
			t.Fatalf("unexpected backend request: %s", request.URL)
		}
		result := jsonResponse(request, `{}`)
		result.StatusCode = http.StatusFound
		result.Header.Set("Location", "http://legacy:3000/api/v1/media/tv/seasons")
		return result, nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	configureTMDBSeasonTest(t, databasePath)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/media/tv/seasons", strings.NewReader("tmdbid=200"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("redirect status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTVSeasonListResolvesBangumiIDWithoutPython(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v0/subjects/300":
			if request.URL.Host != "api.bgm.tv" {
				t.Fatalf("unexpected Bangumi host: %s", request.URL)
			}
			return jsonResponse(request, `{"id":300,"name":"Original Anime","name_cn":"动画","date":"2025-04-01"}`), nil
		case "/3/search/tv":
			if request.URL.Host != "tmdb.test" || request.URL.Query().Get("query") != "Original Anime" {
				t.Fatalf("unexpected TMDB search: %s", request.URL)
			}
			return jsonResponse(request, `{"results":[{"id":200,"name":"Original Anime","first_air_date":"2025-04-01"}]}`), nil
		case "/3/tv/200":
			return jsonResponse(request, `{"id":200,"seasons":[{"season_number":0},{"season_number":1},{"season_number":2}]}`), nil
		default:
			t.Fatalf("unexpected upstream request: %s", request.URL)
			return nil, nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "season-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/media/tv/seasons", strings.NewReader("tmdbid=BG%3A300"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"text":"第二季"`) || !strings.Contains(response.Body.String(), `"text":"第一季"`) {
		t.Fatalf("Bangumi seasons=%d %s", response.Code, response.Body.String())
	}
}

func TestChineseSeasonNumber(t *testing.T) {
	for number, want := range map[int]string{1: "一", 10: "十", 11: "十一", 20: "二十", 100: "一百", 101: "一百零一", 110: "一百一十", 1001: "一千零一"} {
		if got := chineseSeasonNumber(number); got != want {
			t.Fatalf("%d -> %s, want %s", number, got, want)
		}
	}
}

func configureTMDBSeasonTest(t *testing.T, databasePath string) {
	t.Helper()
	contents := "app:\n  login_user: admin\n  login_password: password\n  rmt_tmdbkey: season-key\nsecurity:\n  api_key: native-services-secret\nmedia:\n  tmdb_language: zh-CN\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(databasePath), "config.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
