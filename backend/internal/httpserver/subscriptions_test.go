package httpserver

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestSubscriptionsListReadsNativeDatabase(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, TMDBID TEXT, IMAGE TEXT, STATE TEXT, RSS_SITES TEXT, FILTER_PIX TEXT, OVER_EDITION INTEGER)`,
		`INSERT INTO RSS_MOVIES VALUES (7, '测试电影', '2025', '100', 'https://media.local/movie.jpg', 'R', '["DemoPT"]', '4K', 1)`,
		`CREATE TABLE RSS_TVS (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, SEASON TEXT, TMDBID TEXT, IMAGE TEXT, STATE TEXT, TOTAL INTEGER, LACK INTEGER, SEARCH_SITES TEXT)`,
		`INSERT INTO RSS_TVS VALUES (8, '测试剧集', '2024', 'S02', '200', 'https://media.local/tv.jpg', 'S', 10, 4, '["OtherPT"]')`,
		`CREATE TABLE RSS_HISTORY (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, TYPE TEXT, TMDBID TEXT, IMAGE TEXT, DESC TEXT, FINISH_TIME TEXT)`,
		`INSERT INTO RSS_HISTORY VALUES (9, '历史电影', '2023', 'MOV', '300', 'https://media.local/history.jpg', '完成记录', '2026-09-18 10:30:00')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/subscriptions", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Success bool              `json:"success"`
		Data    subscriptionsData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Success || len(result.Data.Items) != 2 || len(result.Data.History) != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	var series subscriptionItem
	for _, item := range result.Data.Items {
		if item.Type == "TV" {
			series = item
		}
	}
	if series.Progress != 60 || series.StateLabel != "正在搜索" || series.Image == "https://media.local/tv.jpg" {
		t.Fatalf("unexpected series: %#v", series)
	}
	for _, testCase := range []struct{ path, body, expected string }{
		{"/api/v1/subscribe/movie/list", "", "测试电影"},
		{"/api/v1/subscribe/tv/list", "", "测试剧集"},
		{"/api/v1/subscribe/history", url.Values{"type": {"MOV"}}.Encode(), "历史电影"},
	} {
		legacyRequest := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		legacyRequest.Header.Set("Authorization", token)
		legacyRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		legacyResponse := httptest.NewRecorder()
		handler.ServeHTTP(legacyResponse, legacyRequest)
		if legacyResponse.Code != http.StatusOK || !strings.Contains(legacyResponse.Body.String(), testCase.expected) {
			t.Fatalf("%s = %d: %s", testCase.path, legacyResponse.Code, legacyResponse.Body.String())
		}
	}
	unauthorized := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/movie/list", nil)
	unauthorized.Header.Set("Authorization", "invalid-token")
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token = %d: %s", unauthorizedResponse.Code, unauthorizedResponse.Body.String())
	}
}

func TestSubscriptionsListHandlesMissingLegacyTablesWithoutPython(t *testing.T) {
	t.Parallel()
	handler, token, _ := nativeServicesFixture(t, "", nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/subscriptions", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Data subscriptionsData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data.Items) != 0 || len(result.Data.History) != 0 {
		t.Fatalf("unexpected subscriptions in empty database: %#v", result.Data)
	}
}

func TestNativeSubscriptionRemovalDeletesTVEpisodesAndHistory(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY, NAME TEXT)`,
		`INSERT INTO RSS_MOVIES VALUES (7, '电影')`,
		`CREATE TABLE RSS_TVS (ID INTEGER PRIMARY KEY, NAME TEXT)`,
		`INSERT INTO RSS_TVS VALUES (8, '剧集')`,
		`CREATE TABLE RSS_TV_EPISODES (ID INTEGER PRIMARY KEY, RSSID INTEGER, EPISODES TEXT)`,
		`INSERT INTO RSS_TV_EPISODES VALUES (1, 8, '1,2')`,
		`CREATE TABLE RSS_HISTORY (ID INTEGER PRIMARY KEY, NAME TEXT)`,
		`INSERT INTO RSS_HISTORY VALUES (9, '历史')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/subscriptions/TV/8/remove", ""},
		{"/api/v1/subscribe/delete", "type=MOV&rssid=7"},
		{"/api/v1/subscriptions/history/MOV/9/remove", ""},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	for _, table := range []string{"RSS_MOVIES", "RSS_TVS", "RSS_TV_EPISODES", "RSS_HISTORY"} {
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count = %d, error = %v", table, count, err)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/history/delete", strings.NewReader("rssid=9"))
	request.Header.Set("Authorization", "invalid-token")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized delete = %d: %s", response.Code, response.Body.String())
	}
}

func TestNativeSubscriptionRemovalRollsBackEpisodesWhenTVDeleteFails(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_TVS (ID INTEGER PRIMARY KEY)`,
		`INSERT INTO RSS_TVS VALUES (8)`,
		`CREATE TABLE RSS_TV_EPISODES (ID INTEGER PRIMARY KEY, RSSID INTEGER)`,
		`INSERT INTO RSS_TV_EPISODES VALUES (1, 8)`,
		`CREATE TRIGGER reject_tv_delete BEFORE DELETE ON RSS_TVS BEGIN SELECT RAISE(ABORT, 'reject'); END`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscriptions/TV/8/remove", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failed delete = %d: %s", response.Code, response.Body.String())
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM RSS_TV_EPISODES WHERE RSSID=8`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("episodes after rollback = %d, error = %v", count, err)
	}
}

func TestNativeSubscriptionRemovalByLegacySelectors(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, TMDBID TEXT)`,
		`INSERT INTO RSS_MOVIES VALUES (1, '同名电影', '2020', '100')`,
		`INSERT INTO RSS_MOVIES VALUES (2, '另一名称', '2021', '100')`,
		`INSERT INTO RSS_MOVIES VALUES (3, '同名电影', '2020', '101')`,
		`INSERT INTO RSS_MOVIES VALUES (4, '同名电影', '2022', '102')`,
		`CREATE TABLE RSS_TVS (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, SEASON TEXT, TMDBID TEXT)`,
		`INSERT INTO RSS_TVS VALUES (5, '同名剧集', '2020', '1', '200')`,
		`INSERT INTO RSS_TVS VALUES (6, '同名剧集', '2020', '2', '200')`,
		`CREATE TABLE RSS_TV_EPISODES (ID INTEGER PRIMARY KEY, RSSID INTEGER, EPISODES TEXT)`,
		`INSERT INTO RSS_TV_EPISODES VALUES (7, 5, '1,2')`,
		`INSERT INTO RSS_TV_EPISODES VALUES (8, 6, '1,2')`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{
		"type=MOV&name=%E5%90%8C%E5%90%8D%E7%94%B5%E5%BD%B1&year=2020&tmdbid=100",
		"type=TV&name=%E5%90%8C%E5%90%8D%E5%89%A7%E9%9B%86&season=1&tmdbid=200",
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/delete", strings.NewReader(body))
		request.Header.Set("Authorization", token)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("selector delete = %d: %s", response.Code, response.Body.String())
		}
	}
	for _, testCase := range []struct {
		query string
		count int
	}{
		{`SELECT COUNT(*) FROM RSS_MOVIES`, 1},
		{`SELECT COUNT(*) FROM RSS_MOVIES WHERE ID=4`, 1},
		{`SELECT COUNT(*) FROM RSS_TVS WHERE ID=5`, 0},
		{`SELECT COUNT(*) FROM RSS_TVS WHERE ID=6`, 1},
		{`SELECT COUNT(*) FROM RSS_TV_EPISODES WHERE RSSID=5`, 0},
		{`SELECT COUNT(*) FROM RSS_TV_EPISODES WHERE RSSID=6`, 1},
	} {
		var count int
		if err := database.QueryRow(testCase.query).Scan(&count); err != nil || count != testCase.count {
			t.Fatalf("%s = %d, error = %v", testCase.query, count, err)
		}
	}
}

func TestNativeMovieSelectorRemovalRollsBackAllMatches(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_MOVIES (ID INTEGER PRIMARY KEY, NAME TEXT, YEAR TEXT, TMDBID TEXT)`,
		`INSERT INTO RSS_MOVIES VALUES (1, '电影', '2020', '100')`,
		`INSERT INTO RSS_MOVIES VALUES (2, '电影', '2020', '101')`,
		`CREATE TRIGGER reject_movie_delete BEFORE DELETE ON RSS_MOVIES WHEN OLD.ID=2 BEGIN SELECT RAISE(ABORT, 'reject'); END`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/delete", strings.NewReader(url.Values{
		"type": {"电影"}, "name": {"电影"}, "year": {"2020"}, "tmdbid": {"100"},
	}.Encode()))
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("failed selector delete = %d: %s", response.Code, response.Body.String())
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("movies after rollback = %d, error = %v", count, err)
	}
}

func TestNativeSubscriptionSelectorRejectsInvalidSeason(t *testing.T) {
	t.Parallel()
	handler, token, _ := nativeServicesFixture(t, "", nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/delete", strings.NewReader("type=TV&name=Series&season=wrong"))
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid season = %d: %s", response.Code, response.Body.String())
	}
}

func TestNativeFuzzySubscriptionUpsertWithoutPython(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	post := func(body string, expected int) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/subscriptions", strings.NewReader(body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("upsert status = %d, want %d: %s", response.Code, expected, response.Body.String())
		}
		return response.Body.String()
	}
	if body := post(`{"name":"测试电影","year":"2025","type":"MOV","fuzzyMatch":true,"rssSites":["DemoPT"],"searchSites":[]}`, http.StatusOK); !strings.Contains(body, `"id":"1"`) {
		t.Fatalf("created subscription ID missing: %s", body)
	}
	var name, year, sites, state string
	var fuzzy int
	if err := database.QueryRow(`SELECT NAME,YEAR,RSS_SITES,STATE,FUZZY_MATCH FROM RSS_MOVIES WHERE ID=1`).Scan(&name, &year, &sites, &state, &fuzzy); err != nil || name != "测试电影" || year != "2025" || sites != `["DemoPT"]` || state != "R" || fuzzy != 1 {
		t.Fatalf("movie = %q %q %q %q %d, error = %v", name, year, sites, state, fuzzy, err)
	}
	post(`{"name":"测试电影","year":"2025","type":"MOV","fuzzyMatch":true}`, http.StatusConflict)
	post(`{"id":"1","name":"修改电影","year":"2025","type":"MOV","fuzzyMatch":true}`, http.StatusOK)
	if err := database.QueryRow(`SELECT NAME FROM RSS_MOVIES WHERE ID=1`).Scan(&name); err != nil || name != "修改电影" {
		t.Fatalf("updated movie = %q, error = %v", name, err)
	}
	post(`{"name":"测试剧集","year":"2025","type":"TV","season":"S12","fuzzyMatch":true}`, http.StatusOK)
	var season string
	if err := database.QueryRow(`SELECT SEASON FROM RSS_TVS WHERE ID=1`).Scan(&season); err != nil || season != "S12" {
		t.Fatalf("TV season = %q, error = %v", season, err)
	}
	if _, err := database.Exec(`CREATE TABLE RSS_TV_EPISODES (ID INTEGER PRIMARY KEY, RSSID INTEGER, EPISODES TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO RSS_TV_EPISODES VALUES (1, 1, '1,2')`); err != nil {
		t.Fatal(err)
	}
	post(`{"id":"1","name":"测试剧集","year":"2025","type":"TV","season":"S13","fuzzyMatch":true}`, http.StatusOK)
	if err := database.QueryRow(`SELECT SEASON FROM RSS_TVS WHERE ID=1`).Scan(&season); err != nil || season != "S13" {
		t.Fatalf("updated TV season = %q, error = %v", season, err)
	}
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM RSS_TV_EPISODES`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("TV episodes after edit = %d, error = %v", count, err)
	}
}

func TestNativeExactSubscriptionUsesTMDBDetailsWithoutPython(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Host != "tmdb.test" || request.URL.Query().Get("api_key") != "subscription-secret" {
			t.Errorf("unexpected metadata request: %s", request.URL.String())
		}
		body := ""
		switch request.URL.Path {
		case "/3/movie/100":
			body = `{"id":100,"title":"电影详情","release_date":"2025-03-01","poster_path":"/movie.jpg","overview":"电影简介","vote_average":7.5}`
		case "/3/tv/200":
			body = `{"id":200,"name":"剧集详情","first_air_date":"2024-01-01","poster_path":"/tv.jpg","seasons":[{"season_number":1,"episode_count":8},{"season_number":2,"episode_count":10}]}`
		default:
			t.Errorf("unexpected TMDB path: %s", request.URL.Path)
		}
		return jsonResponse(request, body), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "subscription-secret", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	post := func(body string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/subscriptions", strings.NewReader(body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "subscription-secret") {
			t.Fatalf("upsert = %d: %s", response.Code, response.Body.String())
		}
	}
	post(`{"name":"电影","year":"2025","type":"MOV","mediaId":"100","fuzzyMatch":false}`)
	post(`{"name":"剧集","year":"2024","type":"TV","mediaId":"200","fuzzyMatch":false,"currentEpisode":2}`)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var name, year, tmdbID, state, image, note string
	var fuzzy int
	if err := database.QueryRow(`SELECT NAME,YEAR,TMDBID,STATE,IMAGE,NOTE,FUZZY_MATCH FROM RSS_MOVIES`).Scan(&name, &year, &tmdbID, &state, &image, &note, &fuzzy); err != nil || name != "电影详情" || year != "2025" || tmdbID != "100" || state != "D" || !strings.Contains(image, "/movie.jpg") || !strings.Contains(note, `"poster"`) || fuzzy != 0 {
		t.Fatalf("movie metadata = %q %q %q %q %q %q %d, error = %v", name, year, tmdbID, state, image, note, fuzzy, err)
	}
	var season string
	var total, lack int
	if err := database.QueryRow(`SELECT NAME,SEASON,TOTAL,LACK FROM RSS_TVS`).Scan(&name, &season, &total, &lack); err != nil || name != "剧集详情" || season != "S02" || total != 10 || lack != 7 {
		t.Fatalf("TV metadata = %q %q %d %d, error = %v", name, season, total, lack, err)
	}
}

func TestNativeExactSubscriptionRejectsTMDBRedirectWithoutWriting(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Host != "tmdb.test" {
			t.Errorf("unexpected request: %s", request.URL.String())
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://elsewhere.test/"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "hidden-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscriptions", strings.NewReader(`{"name":"电影","year":"2025","type":"MOV","mediaId":"100","fuzzyMatch":false}`))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || strings.Contains(response.Body.String(), "hidden-key") {
		t.Fatalf("redirect response = %d: %s", response.Code, response.Body.String())
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var exists int
	if err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='RSS_MOVIES')`).Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("RSS_MOVIES after rejected redirect = %d, error = %v", exists, err)
	}
}

func TestCompatSubscriptionAddUsesNativeStorageAndAutoDefaults(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/3/movie/100" || request.URL.Host != "tmdb.test" {
			t.Errorf("unexpected upstream request: %s", request.URL.String())
		}
		return jsonResponse(request, `{"id":100,"title":"电影详情","release_date":"2025-01-01"}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "subscription-secret", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	system, err := systemconfig.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Set(t.Context(), "DefaultRssSettingMOV", `{"pix":"4K","rss_sites":["DemoPT"],"download_setting":"2"}`); err != nil {
		t.Fatal(err)
	}
	_ = system.Close()
	post := func(form url.Values, expected int) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/add", strings.NewReader(form.Encode()))
		request.Header.Set("Authorization", token)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("compat add = %d, want %d: %s", response.Code, expected, response.Body.String())
		}
		return response.Body.String()
	}
	if body := post(url.Values{"type": {"MOV"}, "name": {"电影"}, "year": {"2025"}, "mediaid": {"100"}}, http.StatusOK); !strings.Contains(body, `"rssid":1`) {
		t.Fatalf("missing legacy response ID: %s", body)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var quality, sites, name string
	var setting int
	if err := database.QueryRow(`SELECT FILTER_PIX,RSS_SITES,DOWNLOAD_SETTING,NAME FROM RSS_MOVIES WHERE ID=1`).Scan(&quality, &sites, &setting, &name); err != nil || quality != "4K" || sites != `["DemoPT"]` || setting != 2 || name != "电影详情" {
		t.Fatalf("auto defaults = %q %q %d %q, error = %v", quality, sites, setting, name, err)
	}
	post(url.Values{"type": {"TV"}, "name": {"模糊剧集"}, "fuzzy_match": {"1"}, "in_form": {"manual"}}, http.StatusOK)
	var fuzzy int
	if err := database.QueryRow(`SELECT FUZZY_MATCH FROM RSS_TVS WHERE NAME='模糊剧集'`).Scan(&fuzzy); err != nil || fuzzy != 1 {
		t.Fatalf("native fuzzy compat add = %d, error = %v", fuzzy, err)
	}
	invalid := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/add", strings.NewReader("type=MOV&name=other&fuzzy_match=1"))
	invalid.Header.Set("Authorization", "invalid-token")
	invalid.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token = %d: %s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestSubscriptionNameSearchUsesVerifiedTMDBMatchWithoutPython(t *testing.T) {
	t.Parallel()
	var requests []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "tmdb.test" || request.URL.Query().Get("api_key") != "server-key" {
			t.Errorf("unexpected request: %s", request.URL.String())
		}
		requests = append(requests, request.URL.Path+"?"+request.URL.Query().Get("year"))
		switch request.URL.Path {
		case "/3/search/movie":
			if request.URL.Query().Get("query") != "测试电影" {
				t.Errorf("search query = %q", request.URL.Query().Get("query"))
			}
			if request.URL.Query().Get("year") == "2025" {
				return jsonResponse(request, `{"results":[{"id":99,"title":"同名错误作品","release_date":"2025-01-01"}]}`), nil
			}
			return jsonResponse(request, `{"results":[{"id":100,"title":"测试电影","release_date":"2026-01-01"}]}`), nil
		case "/3/movie/100":
			return jsonResponse(request, `{"id":100,"title":"测试电影","release_date":"2026-01-01"}`), nil
		case "/3/movie/99":
			return jsonResponse(request, `{"id":99,"alternative_titles":{"titles":[]},"translations":{"translations":[]}}`), nil
		default:
			t.Errorf("unexpected TMDB endpoint: %s", request.URL.Path)
			return jsonResponse(request, `{}`), nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/add", strings.NewReader(url.Values{
		"type": {"MOV"}, "name": {"测试电影"}, "year": {"2025"}, "in_form": {"manual"},
	}.Encode()))
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("name subscription = %d: %s", response.Code, response.Body.String())
	}
	if len(requests) != 4 || requests[0] != "/3/search/movie?2025" || requests[1] != "/3/movie/99?" || requests[2] != "/3/search/movie?2026" || requests[3] != "/3/movie/100?" {
		t.Fatalf("TMDB requests = %#v", requests)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var id, year string
	if err := database.QueryRow(`SELECT TMDBID,YEAR FROM RSS_MOVIES`).Scan(&id, &year); err != nil || id != "100" || year != "2026" {
		t.Fatalf("subscription = %q %q, error = %v", id, year, err)
	}
}

func TestSubscriptionNameSearchRejectsUnmatchedResult(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/3/search/tv" && request.URL.Path != "/3/tv/200" {
			t.Errorf("unexpected request: %s", request.URL.Path)
		}
		if request.URL.Path == "/3/tv/200" {
			return jsonResponse(request, `{"id":200,"alternative_titles":{"results":[]},"translations":{"translations":[]}}`), nil
		}
		return jsonResponse(request, `{"results":[{"id":200,"name":"别的剧集","first_air_date":"2025-01-01"}]}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscriptions", strings.NewReader(`{"name":"目标剧集","year":"2025","type":"TV","fuzzyMatch":false}`))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unmatched search = %d: %s", response.Code, response.Body.String())
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var exists int
	if err := database.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='RSS_TVS')`).Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("RSS_TVS after unmatched search = %d, error = %v", exists, err)
	}
}

func TestSubscriptionNameSearchAcceptsVerifiedAlternativeTitle(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/3/search/tv":
			return jsonResponse(request, `{"results":[{"id":200,"name":"Original Show","first_air_date":"2025-01-01"}]}`), nil
		case "/3/tv/200":
			if request.URL.Query().Get("append_to_response") != "alternative_titles,translations" {
				t.Errorf("missing alternate-title request: %s", request.URL.String())
			}
			return jsonResponse(request, `{"id":200,"alternative_titles":{"results":[{"title":"目标剧集"}]}}`), nil
		default:
			t.Errorf("unexpected request: %s", request.URL.Path)
			return jsonResponse(request, `{}`), nil
		}
	})
	_, _, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	service := subscriptionService{client: &http.Client{Transport: transport}, configStore: store}
	id, err := service.resolveNativeSubscriptionMediaID(t.Context(), subscriptionUpsertRequest{Name: "目标剧集", Year: "2025", Type: "TV"})
	if err != nil || id != "200" {
		t.Fatalf("alternative title ID = %q, error = %v", id, err)
	}
}

func TestBangumiSubscriptionResolvesThroughVerifiedTMDBMatch(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v0/subjects/300":
			if request.URL.Host != "api.bgm.tv" || !strings.Contains(request.Header.Get("User-Agent"), "nas-tools-go") {
				t.Errorf("unexpected Bangumi request: %s", request.URL.String())
			}
			return jsonResponse(request, `{"id":300,"name":"Original Anime","name_cn":"动画中文名","date":"2025-04-01"}`), nil
		case "/3/search/tv":
			if request.URL.Host != "tmdb.test" || request.URL.Query().Get("query") != "Original Anime" || request.URL.Query().Get("first_air_date_year") != "2025" {
				t.Errorf("unexpected TMDB search: %s", request.URL.String())
			}
			return jsonResponse(request, `{"results":[{"id":200,"name":"Original Anime","first_air_date":"2025-04-01"}]}`), nil
		case "/3/tv/200":
			return jsonResponse(request, `{"id":200,"name":"动画中文名","first_air_date":"2025-04-01","seasons":[{"season_number":1,"episode_count":12}]}`), nil
		default:
			t.Errorf("unexpected upstream path: %s", request.URL.Path)
			return jsonResponse(request, `{}`), nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/add", strings.NewReader(url.Values{
		"type": {"TV"}, "name": {"动画中文名"}, "mediaid": {"BG:300"}, "in_form": {"manual"},
	}.Encode()))
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Bangumi subscription = %d: %s", response.Code, response.Body.String())
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var id, year, season string
	var total int
	if err := database.QueryRow(`SELECT TMDBID,YEAR,SEASON,TOTAL FROM RSS_TVS`).Scan(&id, &year, &season, &total); err != nil || id != "200" || year != "2025" || season != "S01" || total != 12 {
		t.Fatalf("Bangumi subscription fields = %q %q %q %d, error = %v", id, year, season, total, err)
	}
}

func TestDoubanSubscriptionResolvesThroughVerifiedTMDBMatch(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/v2/movie/123":
			if request.URL.Host != "frodo.douban.com" || request.URL.Query().Get("apiKey") != doubanFrodoKey || request.URL.Query().Get("_sig") == "" || request.URL.Query().Get("_ts") == "" || request.Header.Get("User-Agent") != "MicroMessenger/" {
				t.Errorf("unexpected Douban request: %s", request.URL.String())
			}
			return jsonResponse(request, `{"id":"123","title":"电影中文名","original_title":"Original Movie","year":"2025"}`), nil
		case "/api/v2/tv/124":
			return jsonResponse(request, `{"id":"124","title":"剧集中文名","original_title":"Original TV","year":2025}`), nil
		case "/3/search/movie":
			if request.URL.Query().Get("query") != "Original Movie" || request.URL.Query().Get("year") != "2025" {
				t.Errorf("unexpected movie search: %s", request.URL.String())
			}
			return jsonResponse(request, `{"results":[{"id":100,"title":"Original Movie","release_date":"2025-01-01"}]}`), nil
		case "/3/movie/100":
			return jsonResponse(request, `{"id":100,"title":"电影中文名","release_date":"2025-01-01"}`), nil
		case "/3/search/tv":
			return jsonResponse(request, `{"results":[{"id":200,"name":"Original TV","first_air_date":"2025-01-01"}]}`), nil
		case "/3/tv/200":
			return jsonResponse(request, `{"id":200,"name":"剧集中文名","first_air_date":"2025-01-01","seasons":[{"season_number":1,"episode_count":10},{"season_number":2,"episode_count":20}]}`), nil
		default:
			t.Errorf("unexpected upstream path: %s", request.URL.Path)
			return jsonResponse(request, `{}`), nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	for _, form := range []url.Values{
		{"type": {"MOV"}, "name": {"电影中文名"}, "mediaid": {"DB:123"}, "in_form": {"manual"}},
		{"type": {"TV"}, "name": {"剧集中文名"}, "mediaid": {"DB:124"}, "in_form": {"manual"}},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/add", strings.NewReader(form.Encode()))
		request.Header.Set("Authorization", token)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("Douban subscription = %d: %s", response.Code, response.Body.String())
		}
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var movieID, tvID, season string
	var total int
	if err := database.QueryRow(`SELECT TMDBID FROM RSS_MOVIES`).Scan(&movieID); err != nil || movieID != "100" {
		t.Fatalf("movie ID = %q, error = %v", movieID, err)
	}
	if err := database.QueryRow(`SELECT TMDBID,SEASON,TOTAL FROM RSS_TVS`).Scan(&tvID, &season, &total); err != nil || tvID != "200" || season != "S01" || total != 10 {
		t.Fatalf("TV ID/season/total = %q %q %d, error = %v", tvID, season, total, err)
	}
}

func TestNativeSubscriptionHistoryRedoWithoutPython(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "tmdb.test" {
			t.Errorf("unexpected upstream request: %s", request.URL.String())
		}
		switch request.URL.Path {
		case "/3/movie/100":
			return jsonResponse(request, `{"id":100,"title":"历史电影","release_date":"2025-01-01"}`), nil
		case "/3/tv/200":
			return jsonResponse(request, `{"id":200,"name":"历史剧集","first_air_date":"2024-01-01","seasons":[{"season_number":2,"episode_count":10}]}`), nil
		default:
			t.Errorf("unexpected TMDB path: %s", request.URL.Path)
			return jsonResponse(request, `{}`), nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	store := config.NewStore(filepath.Join(filepath.Dir(databasePath), "config.yaml"))
	if err := store.Update(map[string]any{"app.rmt_tmdbkey": "server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range []string{
		`CREATE TABLE RSS_HISTORY (ID INTEGER PRIMARY KEY, TYPE TEXT, NAME TEXT, YEAR TEXT, TMDBID TEXT, SEASON TEXT, TOTAL INTEGER, START INTEGER)`,
		`INSERT INTO RSS_HISTORY VALUES (5, 'MOV', '历史电影', '2025', '100', NULL, NULL, NULL)`,
		`INSERT INTO RSS_HISTORY VALUES (6, 'TV', '历史剧集', '2024', '200', 'S02', 10, 2)`,
	} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/subscriptions/history/MOV/5/redo", ""},
		{"/api/v1/subscribe/redo", "type=TV&rssid=6"},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	var movieID, season string
	var total, lack int
	if err := database.QueryRow(`SELECT TMDBID FROM RSS_MOVIES`).Scan(&movieID); err != nil || movieID != "100" {
		t.Fatalf("redone movie ID = %q, error = %v", movieID, err)
	}
	if err := database.QueryRow(`SELECT SEASON,TOTAL,LACK FROM RSS_TVS`).Scan(&season, &total, &lack); err != nil || season != "S02" || total != 10 || lack != 7 {
		t.Fatalf("redone TV = %q %d %d, error = %v", season, total, lack, err)
	}
	duplicate := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/redo", strings.NewReader("type=TV&rssid=6"))
	duplicate.Header.Set("Authorization", token)
	duplicate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicate)
	if duplicateResponse.Code != http.StatusConflict {
		t.Fatalf("duplicate redo = %d: %s", duplicateResponse.Code, duplicateResponse.Body.String())
	}
	missing := httptest.NewRequest(http.MethodPost, "/api/v1/subscribe/redo", strings.NewReader("type=TV&rssid=999"))
	missing.Header.Set("Authorization", token)
	missing.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("missing redo = %d: %s", missingResponse.Code, missingResponse.Body.String())
	}
}

func TestNativeSubscriptionRowReadsLegacyJSONSettings(t *testing.T) {
	t.Parallel()
	row := normalizeNativeSubscriptionRow(map[string]any{
		"ID": 10, "TMDBID": "", "DESC": `{"rss_sites":["DemoPT"],"search_sites":["OtherPT"],"over_edition":"Y","restype":"BluRay","total":12}`,
		"NOTE": `{"poster":"https://media.local/poster.jpg"}`, "RSS_SITES": `["OldPT"]`, "SAVE_PATH": "/old",
	})
	if row["overview"] != "" || row["over_edition"] != true || row["filter_restype"] != "BluRay" || row["save_path"] != "" || row["poster"] != "https://media.local/poster.jpg" {
		t.Fatalf("legacy row not normalized: %#v", row)
	}
	if sites := stringsOf(row["rss_sites"]); len(sites) != 1 || sites[0] != "DemoPT" {
		t.Fatalf("legacy RSS sites = %#v", sites)
	}
}

func TestSubscriptionMutationsNeverFallbackWithoutNativeStorage(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("unconfigured native subscriptions reached external service: %s", request.URL)
		return nil, nil
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	paths := []string{
		"/api/v1/subscriptions/TV/8/refresh",
		"/api/v1/subscriptions/MOV/7/remove",
		"/api/v1/subscriptions/history/MOV/9/redo",
		"/api/v1/subscriptions/history/MOV/9/remove",
		"/api/v1/subscribe/add",
		"/api/v1/subscribe/delete",
		"/api/v1/subscribe/redo",
		"/api/v1/subscribe/history/delete",
		"/api/v1/subscribe/movie/list",
		"/api/v1/subscribe/tv/list",
		"/api/v1/subscribe/history",
		"/api/v1/subscribe/search",
	}
	for _, path := range paths {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		request.Header.Set("Authorization", "test-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d: %s", path, response.Code, response.Body.String())
		}
	}
}

func TestSubscriptionUpsertRequiresNativeStorage(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("missing storage used a legacy or metadata request: %s", request.URL)
		return nil, nil
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	body := `{"name":"测试剧集","year":"2025","type":"TV","season":"S02","mediaId":"200","fuzzyMatch":false,"overEdition":false,"rssSites":[],"searchSites":[]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/subscriptions", strings.NewReader(body))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestSubscriptionUnknownIdentifiersNeverFallback(t *testing.T) {
	handler, token, databasePath := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("invalid identifier reached an external service: %s", r.URL)
		return nil, nil
	}))
	for _, selector := range []string{"tt123456", "XX:123", "-1", "0", "9223372036854775808"} {
		for _, compat := range []bool{false, true} {
			path, body, contentType := "/api/v1/subscriptions", `{"name":"Test","type":"MOV","mediaId":"`+selector+`"}`, "application/json"
			if compat {
				path, body, contentType = "/api/v1/subscribe/add", url.Values{"name": {"Test"}, "type": {"MOV"}, "mediaid": {selector}, "in_form": {"manual"}}.Encode(), "application/x-www-form-urlencoded"
			}
			r := httptest.NewRequest("POST", path, strings.NewReader(body))
			r.Header.Set("Authorization", token)
			r.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 400 {
				t.Fatal(selector, compat, w.Code, w.Body.String())
			}
		}
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='RSS_MOVIES')`).Scan(&exists); err != nil || exists {
		t.Fatal("invalid requests created subscription data", exists, err)
	}
}
