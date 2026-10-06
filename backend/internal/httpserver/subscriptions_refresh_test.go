package httpserver

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func refreshFixture(t *testing.T, transport http.RoundTripper) (http.Handler, string, *sql.DB, string) {
	t.Helper()
	handler, token, path := nativeServicesFixture(t, "", nil, transport)
	appPath := filepath.Join(filepath.Dir(path), "config.yaml")
	if err := config.NewStore(appPath).Update(map[string]any{"app.rmt_tmdbkey": "private-key", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{nativeSubscriptionSchema("MOV"), nativeSubscriptionSchema("TV"), `CREATE TABLE RSS_TV_EPISODES (ID INTEGER PRIMARY KEY,RSSID TEXT,EPISODES TEXT)`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	handler, err = newHandler(config.Config{ApplicationConfigPath: appPath, DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	return handler, token, db, appPath
}

func TestNativeSubscriptionRefreshPreservesProgressAndSettings(t *testing.T) {
	var calls atomic.Int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "tmdb.test" || r.URL.Query().Get("api_key") != "private-key" {
			t.Error("unexpected Python/external request")
			return nil, context.Canceled
		}
		calls.Add(1)
		if strings.HasPrefix(r.URL.Path, "/3/movie/") {
			return jsonResponse(r, `{"id":70,"title":"新电影名","release_date":"2026-01-01","overview":"新简介","poster_path":"/movie.jpg"}`), nil
		}
		return jsonResponse(r, `{"id":80,"name":"新剧名","first_air_date":"2025-01-01","seasons":[{"season_number":1,"episode_count":8}]}`), nil
	})
	handler, token, db, _ := refreshFixture(t, transport)
	for _, statement := range []string{
		`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,FILTER_INCLUDE,SAVE_PATH,NOTE) VALUES (1,'旧电影','2020','70','R',0,'keep-filter','/keep','{"private":"keep","poster":"/old"}')`,
		`INSERT INTO RSS_TVS(ID,NAME,YEAR,TMDBID,SEASON,STATE,FUZZY_MATCH,TOTAL,LACK,TOTAL_EP,NOTE,DOWNLOAD_SETTING) VALUES (2,'旧剧','2020','80','S01','R',0,5,2,0,'{}',9)`,
		`INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES ('2','2,5')`,
		`INSERT INTO RSS_TVS(ID,NAME,YEAR,TMDBID,SEASON,STATE,FUZZY_MATCH,TOTAL,LACK,TOTAL_EP,NOTE,DESC) VALUES (3,'固定总数','2020','80','S01','R',0,6,1,6,'{}','{"restype":"BluRay","total":6,"unknown":"keep"}')`,
		`INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES ('3','4')`,
		`INSERT INTO RSS_MOVIES(ID,NAME,STATE,FUZZY_MATCH) VALUES (4,'模糊订阅','R',1),(5,'搜索中的订阅','S',0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	response := performFormRequest(handler, "/api/v1/subscriptions/refresh", token, url.Values{})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"updated":3`) || !strings.Contains(response.Body.String(), `"skipped":2`) || strings.Contains(response.Body.String(), "private") {
		t.Fatal(response.Code, response.Body.String())
	}
	var name, note, filter, path string
	if err := db.QueryRow(`SELECT NAME,NOTE,FILTER_INCLUDE,SAVE_PATH FROM RSS_MOVIES WHERE ID=1`).Scan(&name, &note, &filter, &path); err != nil || name != "新电影名" || !strings.Contains(note, `"private":"keep"`) || filter != "keep-filter" || path != "/keep" {
		t.Fatal(name, note, filter, path, err)
	}
	var total, lack, setting int
	var missing string
	if err := db.QueryRow(`SELECT TOTAL,LACK,DOWNLOAD_SETTING FROM RSS_TVS WHERE ID=2`).Scan(&total, &lack, &setting); err != nil || total != 8 || lack != 5 || setting != 9 {
		t.Fatal(total, lack, setting, err)
	}
	if err := db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='2'`).Scan(&missing); err != nil || missing != "2,5,6,7,8" {
		t.Fatal(missing, err)
	}
	var description string
	if err := db.QueryRow(`SELECT TOTAL,LACK,DESC FROM RSS_TVS WHERE ID=3`).Scan(&total, &lack, &description); err != nil || total != 6 || lack != 1 || !strings.Contains(description, `"unknown":"keep"`) {
		t.Fatal(total, lack, description, err)
	}
	if calls.Load() != 3 {
		t.Fatal("fuzzy/in-flight subscriptions were queried", calls.Load())
	}
}

func TestNativeSubscriptionRefreshRollsBackEpisodesAndRejectsStaleWrite(t *testing.T) {
	var db *sql.DB
	edit := false
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if edit {
			if _, err := db.Exec(`UPDATE RSS_TVS SET NAME='用户新名称',SAVE_PATH='/edited' WHERE ID=1`); err != nil {
				t.Error(err)
			}
		}
		return jsonResponse(r, `{"id":80,"name":"TMDB新名称","first_air_date":"2025-01-01","seasons":[{"season_number":1,"episode_count":8}]}`), nil
	})
	handler, token, store, path := refreshFixture(t, transport)
	db = store
	if _, err := db.Exec(`INSERT INTO RSS_TVS(ID,NAME,YEAR,TMDBID,SEASON,STATE,FUZZY_MATCH,TOTAL,LACK,NOTE) VALUES (1,'旧名称','2020','80','S01','R',0,5,2,'{}'); INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES ('1','2,5'); CREATE TRIGGER fail_episode_insert BEFORE INSERT ON RSS_TV_EPISODES BEGIN SELECT RAISE(ABORT,'private-database-error'); END`); err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/subscriptions/refresh", token, url.Values{})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"failed":1`) || strings.Contains(response.Body.String(), "private") {
		t.Fatal(response.Body.String())
	}
	var name, missing string
	var total int
	if err := db.QueryRow(`SELECT NAME,TOTAL FROM RSS_TVS WHERE ID=1`).Scan(&name, &total); err != nil || name != "旧名称" || total != 5 {
		t.Fatal(name, total, err)
	}
	if err := db.QueryRow(`SELECT EPISODES FROM RSS_TV_EPISODES WHERE RSSID='1'`).Scan(&missing); err != nil || missing != "2,5" {
		t.Fatal(missing, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_episode_insert`); err != nil {
		t.Fatal(err)
	}
	edit = true
	response = performFormRequest(handler, "/api/v1/subscriptions/refresh", token, url.Values{})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"skipped":1`) {
		t.Fatal(response.Body.String())
	}
	if err := db.QueryRow(`SELECT NAME,TOTAL FROM RSS_TVS WHERE ID=1`).Scan(&name, &total); err != nil || name != "用户新名称" || total != 5 {
		t.Fatal(name, total, err)
	}
	edit = false
	if err := config.NewStore(path).Update(map[string]any{"media.name_follow_tmdb_changed": false}); err != nil {
		t.Fatal(err)
	}
	response = performFormRequest(handler, "/api/v1/subscriptions/refresh", token, url.Values{})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"updated":1`) {
		t.Fatal(response.Body.String())
	}
	if err := db.QueryRow(`SELECT NAME,TOTAL FROM RSS_TVS WHERE ID=1`).Scan(&name, &total); err != nil || name != "用户新名称" || total != 8 {
		t.Fatal(name, total, err)
	}
}

func TestSubscriptionRefreshMissingEpisodes(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "episodes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE RSS_TV_EPISODES(ID INTEGER PRIMARY KEY,RSSID TEXT,EPISODES TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		raw     string
		present bool
		next    int
		want    string
		fail    bool
	}{
		{"", false, 8, "4,5,6,7,8", false},
		{"2,5", true, 3, "2", false},
		{"2", true, 8, "", true},
		{"2,2", true, 8, "", true},
		{"2,6", true, 8, "", true},
	} {
		if _, err := db.Exec(`DELETE FROM RSS_TV_EPISODES`); err != nil {
			t.Fatal(err)
		}
		if test.present {
			if _, err := db.Exec(`INSERT INTO RSS_TV_EPISODES(RSSID,EPISODES) VALUES ('1',?)`, test.raw); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		missing, err := refreshMissingEpisodes(context.Background(), tx, "1", 5, 2, test.next)
		tx.Rollback()
		if (err != nil) != test.fail {
			t.Fatal(test, err)
		}
		if !test.fail {
			parts := []string{}
			for _, n := range missing {
				parts = append(parts, strconv.Itoa(n))
			}
			if strings.Join(parts, ",") != test.want {
				t.Fatal(test, missing)
			}
		}
	}
}

func TestSubscriptionRefreshUpstreamFailureAndViewer(t *testing.T) {
	var calls atomic.Int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		response := jsonResponse(r, `{"private":"upstream failure"}`)
		response.StatusCode = 500
		return response, nil
	})
	handler, token, db, _ := refreshFixture(t, transport)
	if _, err := db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH) VALUES (1,'原电影名','2026','70','R',0)`); err != nil {
		t.Fatal(err)
	}
	response := performFormRequest(handler, "/api/v1/subscriptions/refresh", token, url.Values{})
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"failed":1`) || !strings.Contains(response.Body.String(), `"updated":0`) || strings.Contains(response.Body.String(), "private") {
		t.Fatal(response.Body.String())
	}
	created := performFormRequest(handler, "/api/v1/user/manage", token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	viewer := loginForTest(t, handler, "viewer", "strong-password")
	response = performFormRequest(handler, "/api/v1/subscriptions/refresh", viewer, url.Values{})
	if response.Code != 403 || calls.Load() != 1 {
		t.Fatal(response.Code, calls.Load())
	}
}

func TestSubscriptionRefreshWorkerSharesGateAndCancels(t *testing.T) {
	started := make(chan struct{}, 1)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	handler, token, db, path := refreshFixture(t, transport)
	if _, err := db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH) VALUES (1,'电影','2026','70','R',0)`); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	handler, err := buildHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks, done := make(chan time.Time), make(chan struct{})
	go func() { defer close(done); runSubscriptionRefreshWorker(ctx, runner.refresh, ticks) }()
	ticks <- time.Now()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	response := performFormRequest(handler, "/api/v1/subscriptions/refresh", token, url.Values{})
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not cancel")
	}
	if response := performFormRequest(handler, "/api/v1/subscriptions/refresh", "", url.Values{}); response.Code != 401 {
		t.Fatal(response.Code)
	}
	transition, err := newHandler(config.Config{ApplicationConfigPath: path, LegacyBackendURL: "http://legacy.local"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	if response := performFormRequest(transition, "/api/v1/subscriptions/refresh", token, url.Values{}); response.Code != 501 {
		t.Fatal(response.Code)
	}
}
