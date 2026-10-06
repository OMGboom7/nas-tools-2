package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func rssSubscriptionFixture(t *testing.T, transport http.RoundTripper) (http.Handler, string, *sql.DB, string) {
	t.Helper()
	handler, token, db, path := rssRunFixture(t, transport)
	if err := config.NewStore(path).Update(map[string]any{"app.rmt_tmdbkey": "private-server-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	format := `{"list":"$.items","item":{"title":{"path":"title"},"type":{"path":"type"},"year":{"path":"year"}}}`
	if _, err := db.Exec(`UPDATE CONFIG_RSS_PARSER SET FORMAT=? WHERE ID=1`, format); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET USES='R',INCLUDE='',EXCLUDE='',RECOGNIZATION='N',FILTER='42',SITES='{"rss_sites":[1,"site2"],"search_sites":["site3"]}',FILTER_ARGS='{"restype":"WEB-DL","pix":"1080p","team":"team"}',SAVE_PATH='/downloads/movies',DOWNLOAD_SETTING=9,OVER_EDITION=1 WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	return handler, token, db, path
}

func rssSubscriptionTMDB(r *http.Request) *http.Response {
	switch r.URL.Path {
	case "/3/search/movie":
		if r.URL.Query().Get("query") == "测试电影" {
			return jsonResponse(r, `{"results":[{"id":100,"title":"测试电影","release_date":"2026-01-01"}]}`)
		}
		return jsonResponse(r, `{"results":[]}`)
	case "/3/movie/100":
		return jsonResponse(r, `{"id":100,"title":"测试电影","release_date":"2026-01-01"}`)
	case "/3/search/tv":
		return jsonResponse(r, `{"results":[{"id":200,"name":"测试剧","first_air_date":"2025-01-01"}]}`)
	case "/3/tv/200":
		return jsonResponse(r, `{"id":200,"name":"测试剧","first_air_date":"2025-01-01","seasons":[{"season_number":1,"episode_count":10},{"season_number":2,"episode_count":8}]}`)
	}
	return jsonResponse(r, `{}`)
}

func TestRSSSubscriptionsCreateMovieTVAndHonorHistoryWithoutDownloads(t *testing.T) {
	feed := `{"items":[{"title":"测试电影.2026.1080p","type":"movie","year":"2026"},{"title":"测试电影.2026.1080p","type":"movie","year":"2026"},{"title":"测试剧.S02.1080p","type":"tv"},{"title":"排除测试电影","type":"movie"},{"title":""},{"title":"未识别电影","type":"movie"},{"title":"历史电影","type":"movie","year":"2026"}]}`
	tmdb := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "feed.local" {
			return jsonResponse(r, feed), nil
		}
		if r.URL.Hostname() != "tmdb.test" || r.URL.Query().Get("api_key") != "private-server-key" {
			t.Error("unexpected downloader/Python request")
			return nil, context.Canceled
		}
		if r.URL.Query().Get("query") == "历史电影" || strings.Contains(r.URL.Query().Get("query"), "排除") {
			t.Error("filtered/history item reached TMDB")
		}
		tmdb++
		return rssSubscriptionTMDB(r), nil
	})
	handler, token, db, path := rssSubscriptionFixture(t, transport)
	if _, err := db.Exec(`DELETE FROM DOWNLOADER`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET INCLUDE='测试|未识别|历史',EXCLUDE='排除' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE RSS_HISTORY (ID INTEGER PRIMARY KEY,TYPE TEXT,NAME TEXT,YEAR TEXT,SEASON TEXT,TMDBID TEXT); INSERT INTO RSS_HISTORY(TYPE,NAME,YEAR) VALUES ('MOV','历史电影','2026')`); err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"subscribed":2`) || !strings.Contains(response.Body.String(), `"skipped":5`) || strings.Contains(response.Body.String(), "private") {
		t.Fatal(response.Code, response.Body.String())
	}
	var mediaID, rssSites, searchSites, quality, resolution, team, savePath string
	var filter, setting, over int
	if err := db.QueryRow(`SELECT TMDBID,RSS_SITES,SEARCH_SITES,FILTER_RESTYPE,FILTER_PIX,FILTER_TEAM,SAVE_PATH,FILTER_RULE,DOWNLOAD_SETTING,OVER_EDITION FROM RSS_MOVIES`).Scan(&mediaID, &rssSites, &searchSites, &quality, &resolution, &team, &savePath, &filter, &setting, &over); err != nil {
		t.Fatal(err)
	}
	if mediaID != "100" || rssSites != `["1","site2"]` || searchSites != `["site3"]` || quality != "WEB-DL" || resolution != "1080p" || team != "team" || savePath != "/downloads/movies" || filter != 42 || setting != 9 || over != 1 {
		t.Fatal(mediaID, rssSites, searchSites, quality, resolution, team, savePath, filter, setting, over)
	}
	var season, tvID, state, count string
	var total, lack int
	if err := db.QueryRow(`SELECT TMDBID,SEASON,TOTAL,LACK,STATE FROM RSS_TVS`).Scan(&tvID, &season, &total, &lack, &state); err != nil || tvID != "200" || season != "S02" || total != 8 || lack != 8 || state != "D" {
		t.Fatal(tvID, season, total, lack, state, err)
	}
	if err := db.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&count); err != nil || count != "2" {
		t.Fatal(count, err)
	}
	// S is a persisted alias for R, including the processed key after restart.
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET USES='S' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	feed = `{"items":[{"title":"测试电影.2026.1080p","type":"movie","year":"2026"},{"title":"测试剧.S02.1080p","type":"tv"}]}`
	before := tmdb
	handler, err := newHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"skipped":2`) || tmdb != before {
		t.Fatal(response.Code, response.Body.String(), tmdb)
	}
	// Removing the processed marker must still not overwrite an active subscription.
	if _, err := db.Exec(`DELETE FROM RSS_TORRENTS; UPDATE RSS_MOVIES SET NAME='保留已有标题',SAVE_PATH='/existing'`); err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"subscribed":0`) {
		t.Fatal(response.Body.String())
	}
	if err := db.QueryRow(`SELECT SAVE_PATH FROM RSS_MOVIES WHERE TMDBID=100`).Scan(&savePath); err != nil || savePath != "/existing" {
		t.Fatal(savePath, err)
	}
	// History may use another localized name: verified TMDB identity and season
	// must still block re-creation, without recording a false new completion.
	feed = `{"items":[{"title":"测试剧.S02.1080p","type":"tv"}]}`
	if _, err := db.Exec(`DELETE FROM RSS_TORRENTS; DELETE FROM RSS_TVS; INSERT INTO RSS_HISTORY(TYPE,NAME,YEAR,SEASON,TMDBID) VALUES ('TV','历史译名','2025','S02','200')`); err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"skipped":1`) {
		t.Fatal(response.Code, response.Body.String())
	}
	var n int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM RSS_TVS)+(SELECT COUNT(*) FROM RSS_TORRENTS)`).Scan(&n); err != nil || n != 0 {
		t.Fatal("history media recreated or falsely marked", n, err)
	}
}

func TestRSSSubscriptionFailureRollsBackAndCanRetry(t *testing.T) {
	failMetadata := true
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "feed.local" {
			return jsonResponse(r, `{"items":[{"title":"测试电影.2026.1080p","type":"movie"}]}`), nil
		}
		if r.URL.Hostname() != "tmdb.test" {
			t.Error("unexpected downloader/Python request")
			return nil, context.Canceled
		}
		if r.URL.Path == "/3/movie/100" && failMetadata {
			response := jsonResponse(r, `{"private":"upstream credentials"}`)
			response.StatusCode = 500
			return response, nil
		}
		return rssSubscriptionTMDB(r), nil
	})
	handler, token, db, _ := rssSubscriptionFixture(t, transport)
	run := func(status int) {
		t.Helper()
		response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
		if response.Code != status || strings.Contains(response.Body.String(), "private") {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	assertEmpty := func() {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&n); err != nil || n != 0 {
			t.Fatal(n, err)
		}
		var count string
		if err := db.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&count); err != nil || count != "0" {
			t.Fatal(count, err)
		}
	}
	run(502)
	assertEmpty()
	failMetadata = false
	if _, err := db.Exec(nativeSubscriptionSchema("MOV")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_rss_marker BEFORE INSERT ON RSS_TORRENTS BEGIN SELECT RAISE(ABORT,'private-storage-error'); END`); err != nil {
		t.Fatal(err)
	}
	run(503)
	assertEmpty()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES`).Scan(&n); err != nil || n != 0 {
		t.Fatal("subscription write survived failed completion", n, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_rss_marker; CREATE TRIGGER fail_rss_counter BEFORE UPDATE OF PROCESS_COUNT ON CONFIG_USER_RSS BEGIN SELECT RAISE(ABORT,'counter-error'); END`); err != nil {
		t.Fatal(err)
	}
	run(503)
	assertEmpty()
	if _, err := db.Exec(`DROP TRIGGER fail_rss_counter`); err != nil {
		t.Fatal(err)
	}
	run(200)
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestRSSSubscriptionWorkerDispatchAndConcurrentStores(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "feed.local" {
			return jsonResponse(r, `{"items":[{"title":"测试剧","type":"tv"}]}`), nil
		}
		if r.URL.Hostname() != "tmdb.test" {
			t.Error("unexpected downloader/Python request")
			return nil, context.Canceled
		}
		return rssSubscriptionTMDB(r), nil
	})
	_, _, db, path := rssSubscriptionFixture(t, transport)
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET USES='S',STATE='Y',INTERVAL='1' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	if _, err := buildHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport, &runner); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks, done := make(chan time.Time), make(chan struct{})
	base := time.Now()
	go func() { defer close(done); runRSSWorker(ctx, runner, time.UTC, base, ticks) }()
	ticks <- base.Add(time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n string
		if err := db.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("S subscription worker did not persist")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	// No season in the RSS means the latest valid season, as in the old subscription API.
	var season string
	if err := db.QueryRow(`SELECT SEASON FROM RSS_TVS`).Scan(&season); err != nil || season != "S02" {
		t.Fatal(season, err)
	}
	if _, err := db.Exec(`DELETE FROM RSS_TORRENTS; DELETE FROM RSS_TVS; UPDATE CONFIG_USER_RSS SET PROCESS_COUNT='0'`); err != nil {
		t.Fatal(err)
	}
	input := subscriptionUpsertRequest{Type: "TV", Name: "测试剧", Season: "2", MediaID: "200"}
	metadata := &nativeSubscriptionMetadata{ID: "200", Title: "测试剧", Year: "2025", Season: "S02", Total: 8, Lack: 8}
	service := runner.recognition.service
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each call opens its own connection; SQLite serializes the same key.
			_, _ = service.completeRSSSubscription(context.Background(), 7, "same-title", input, metadata)
		}()
	}
	wg.Wait()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_TVS`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var count string
	if err := db.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&count); err != nil || count != "1" {
		t.Fatal("concurrent subscription counted twice", count, err)
	}
	// The worker's own subscription DB is the same native database as the task store.
	if filepath.Dir(service.databasePath) != filepath.Dir(path) {
		t.Fatal("RSS state and subscriptions are not in one database")
	}
}

func TestRSSSubscriptionInvalidSettingsFailBeforeFetch(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("invalid settings reached an external service")
		return nil, context.Canceled
	})
	handler, token, db, _ := rssSubscriptionFixture(t, transport)
	for _, statement := range []string{`UPDATE CONFIG_USER_RSS SET SITES='null'`, `UPDATE CONFIG_USER_RSS SET SITES='{}',FILTER_ARGS='[]'`, `UPDATE CONFIG_USER_RSS SET FILTER_ARGS='{}',INCLUDE='['`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		response := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
		if response.Code != 422 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	for _, raw := range []string{`[true]`, `[1.5]`, `[{}]`, `[""]`, `"1"`} {
		if _, err := rssSubscriptionSiteList(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid sites accepted", raw)
		}
	}
}
