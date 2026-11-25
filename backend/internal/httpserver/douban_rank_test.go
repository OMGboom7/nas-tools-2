package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type rankFixture struct {
	path, token     string
	api             *doubanSyncAPI
	h               http.Handler
	db              *sql.DB
	feed            string
	owned           bool
	calls, subjects int
	duringFeed      func()
}

func newRankFixture(t *testing.T) *rankFixture {
	t.Helper()
	dir := t.TempDir()
	f := &rankFixture{path: filepath.Join(dir, "config.yaml"), feed: "<item><title>榜单一</title><link>https://movie.douban.com/subject/123/</link></item>"}
	if err := os.WriteFile(f.path, []byte("app:\n  login_user: admin\n  login_password: password\n  rmt_tmdbkey: fixture\nsecurity:\n  api_key: fixture-token\nmedia:\n  media_server: emby\nemby:\n  host: https://media.test\n  api_key: fixture-emby\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.token = loginForTest(t, f.h, "admin", "password")
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { db.Close() })
	return f
}

func (f *rankFixture) start(t *testing.T) {
	t.Helper()
	var runner *rssRunAPI
	h, err := buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" {
			t.Fatal("unexpected cookie")
		}
		switch r.URL.Host + r.URL.Path {
		case "rsshub.app/douban/movie/ustop", "feed.test/custom":
			f.calls++
			if f.duringFeed != nil {
				f.duringFeed()
			}
			return jsonResponse(r, "<rss><channel>"+f.feed+"</channel></rss>"), nil
		case "frodo.douban.com/api/v2/movie/123":
			f.subjects++
			return jsonResponse(r, `{"id":"123","title":"Movie","year":"2026"}`), nil
		case "api.themoviedb.org/3/search/movie":
			if strings.Contains(r.URL.Query().Get("query"), "Show") {
				return jsonResponse(r, `{"results":[]}`), nil
			}
			return jsonResponse(r, `{"results":[{"id":100,"title":"Movie","release_date":"2026-01-01"}]}`), nil
		case "api.themoviedb.org/3/movie/100":
			return jsonResponse(r, `{"id":100,"title":"Movie","release_date":"2026-01-01","vote_average":8.5,"poster_path":"/poster.jpg"}`), nil
		case "api.themoviedb.org/3/search/tv":
			return jsonResponse(r, `{"results":[{"id":200,"name":"Show","first_air_date":"2020-01-01"}]}`), nil
		case "api.themoviedb.org/3/tv/200":
			return jsonResponse(r, `{"id":200,"name":"Show","first_air_date":"2020-01-01","vote_average":9,"seasons":[{"season_number":1,"episode_count":5},{"season_number":2,"episode_count":6}]}`), nil
		case "media.test/emby/Items":
			if f.owned {
				return jsonResponse(r, `{"Items":[{"Id":"stored","Name":"Show","Type":"Series","ProductionYear":2020,"ProviderIds":{"Tmdb":"200"}}]}`), nil
			}
			return jsonResponse(r, `{"Items":[]}`), nil
		case "media.test/emby/Shows/stored/Episodes":
			return jsonResponse(r, `{"Items":[{"ParentIndexNumber":1,"IndexNumber":3}]}`), nil
		}
		t.Errorf("unexpected request %s %s", r.URL.Host, r.URL.Path)
		return nil, context.Canceled
	}), &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	f.api = runner.archivePlugin.doubanSync
}
func (f *rankFixture) action(t *testing.T, action string, values string) {
	t.Helper()
	form := url.Values{"id": {"DoubanRank"}}
	if values != "" {
		form.Set("config", values)
	}
	w := performFormRequest(f.h, "/api/v1/plugin/"+action, f.token, form)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func (f *rankFixture) state(t *testing.T) doubanControl {
	t.Helper()
	v, err := f.api.plugins.system.Get(t.Context(), rankControlKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := doubanState(v)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func (f *rankFixture) run(t *testing.T) string {
	t.Helper()
	c := f.state(t)
	at := c.Once
	if at.IsZero() {
		at = c.Next
	}
	if at.IsZero() {
		t.Fatal(c)
	}
	if err := f.api.rankTick(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	return f.state(t).Last
}
func (f *rankFixture) count(t *testing.T, query string, want int) {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query).Scan(&n); err != nil || n != want {
		t.Fatal(query, n, want, err)
	}
}

const rankOnce = `{"onlyonce":true,"ranks":["movie-ustop"],"rss_addrs":"https://feed.test/custom","cron":"0 8 * * *","enable":true,"release_date":"2019","vote":"8.5"}`

func TestDoubanRankLifecycleScopeAndAtomicSubscription(t *testing.T) {
	f := newRankFixture(t)
	if err := f.api.plugins.system.Set(t.Context(), rankSettingsKey, `{"onlyonce":true,"unknown":{"keep":true},"ranks":["movie-ustop"]}`); err != nil {
		t.Fatal(err)
	}
	f.action(t, "install", "")
	if err := f.api.rankTick(t.Context(), time.Now().Add(24*time.Hour)); err != nil || f.calls != 0 {
		t.Fatal(err, f.calls)
	}
	if err := f.api.plugins.system.Set(t.Context(), "DefaultRssSettingMOV", `{"include":"Wanted"}`); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/plugins/DoubanRank"
	w := nativeJSONRequest(f.h, "GET", path, f.token, "")
	var got struct{ Data pluginConfigDetail }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Data.Fields) != 7 {
		t.Fatal(w.Code, w.Body.String())
	}
	body := fmt.Sprintf(`{"values":%s,"fingerprint":%q}`, rankOnce, got.Data.Meta.Fingerprint)
	if w = nativeJSONRequest(f.h, "PUT", path, f.token, body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = nativeJSONRequest(f.h, "PUT", path, f.token, body); w.Code != 409 {
		t.Fatal(w.Code)
	}
	f.start(t)
	if last := f.run(t); !strings.Contains(last, "新增订阅 1 条") {
		t.Fatal(last)
	}
	f.count(t, "SELECT count(*) FROM RSS_MOVIES WHERE TMDBID='100' AND STATE='D' AND FILTER_INCLUDE='Wanted'", 1)
	f.count(t, "SELECT count(*) FROM RSS_TORRENTS WHERE TORRENT_NAME='doubanrank: 榜单一 (DB:123)' AND ENCLOSURE IS NULL", 1)
	f.count(t, "SELECT count(*) FROM PLUGIN_HISTORY WHERE PLUGIN_ID='DoubanRank' AND KEY='100' AND json_extract(VALUE,'$.state')='RSS'", 1)
	f.count(t, "SELECT count(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_added'", 1)
	if v, _ := f.api.plugins.system.Get(t.Context(), rankSettingsKey); !strings.Contains(v, `"unknown":{"keep":true}`) || !strings.Contains(v, `"onlyonce":false`) {
		t.Fatal(v)
	}
	f.start(t)
	f.run(t)
	if f.calls != 4 || f.subjects != 1 {
		t.Fatal(f.calls, f.subjects)
	}
	f.action(t, "uninstall", "")
	if err := f.api.rankTick(t.Context(), time.Now().Add(2*24*time.Hour)); err != nil || f.calls != 4 {
		t.Fatal(err, f.calls)
	}
}
func TestDoubanRankFiltersRecoveryAndSeasonPresence(t *testing.T) {
	for _, mode := range []string{"rating", "year", "rollback", "season", "season-present", "cancel", "old-marker", "old-history"} {
		t.Run(mode, func(t *testing.T) {
			f := newRankFixture(t)
			f.action(t, "install", "")
			cfg := rankOnce
			switch mode {
			case "rating":
				cfg = strings.Replace(cfg, "8.5", "9", 1)
			case "year":
				cfg = strings.Replace(cfg, "2019", "2027", 1)
			case "season", "season-present":
				f.feed = "<item><title>Show</title></item>"
				f.owned = mode == "season-present"
			case "cancel":
				f.duringFeed = func() { f.duringFeed = nil; f.action(t, "uninstall", "") }
			case "old-marker":
				if _, err := f.db.Exec("INSERT INTO RSS_TORRENTS(TORRENT_NAME) VALUES('doubanrank: 榜单一 (DB:123)')"); err != nil {
					t.Fatal(err)
				}
			case "old-history":
				if _, err := f.db.Exec("CREATE TABLE IF NOT EXISTS RSS_HISTORY(ID INTEGER PRIMARY KEY,TYPE TEXT,NAME TEXT,YEAR TEXT,TMDBID TEXT,SEASON TEXT);INSERT INTO RSS_HISTORY(TYPE,NAME,YEAR,TMDBID,SEASON) VALUES('MOV','Movie','2026','100','')"); err != nil {
					t.Fatal(err)
				}
			case "rollback":
				if _, err := f.db.Exec("CREATE TABLE IF NOT EXISTS PLUGIN_HISTORY(ID INTEGER PRIMARY KEY,PLUGIN_ID TEXT,KEY TEXT,VALUE TEXT,DATE TEXT);CREATE TRIGGER fixture_rank_reject BEFORE INSERT ON PLUGIN_HISTORY BEGIN SELECT RAISE(IGNORE); END"); err != nil {
					t.Fatal(err)
				}
			}
			f.action(t, "config", cfg)
			if mode == "cancel" {
				if err := f.api.rankTick(t.Context(), f.state(t).Once); err == nil || f.subjects != 0 {
					t.Fatal(err, f.subjects)
				}
				return
			}
			last := f.run(t)
			switch mode {
			case "rollback":
				f.count(t, "SELECT count(*) FROM RSS_TORRENTS", 0)
				if !strings.Contains(last, "失败") {
					t.Fatal(last)
				}
				if _, err := f.db.Exec("DROP TRIGGER fixture_rank_reject"); err != nil {
					t.Fatal(err)
				}
				f.start(t)
				f.run(t)
				f.count(t, "SELECT count(*) FROM RSS_MOVIES", 1)
			case "season":
				f.count(t, "SELECT count(*) FROM RSS_TVS WHERE TMDBID='200' AND SEASON='S02' AND TOTAL=6", 1)
				f.count(t, "SELECT count(*) FROM RSS_TORRENTS WHERE TORRENT_NAME='doubanrank: Show (DB:[])'", 1)
			case "season-present":
				f.count(t, "SELECT count(*) FROM PLUGIN_HISTORY WHERE KEY='200' AND json_extract(VALUE,'$.state')='DOWNLOADED'", 1)
				f.count(t, "SELECT count(*) FROM RSS_TORRENTS", 0)
			case "old-history":
				f.count(t, "SELECT count(*) FROM PLUGIN_HISTORY WHERE KEY='100' AND json_extract(VALUE,'$.state')='RSS'", 1)
				f.count(t, "SELECT count(*) FROM RSS_TORRENTS", 0)
			case "old-marker":
				if f.subjects != 0 {
					t.Fatal(f.subjects)
				}
			default:
				f.count(t, "SELECT count(*) FROM RSS_TORRENTS", 0)
				if !strings.Contains(last, "失败 0") {
					t.Fatal(last)
				}
			}
		})
	}
}

func TestDoubanRankInterruptedOnceAndExistingSubscription(t *testing.T) {
	f := newRankFixture(t)
	f.action(t, "install", "")
	const cfg = `{"enable":true,"onlyonce":true,"ranks":["movie-ustop"]}`
	f.action(t, "config", cfg)
	f.run(t)
	if c := f.state(t); !c.Once.IsZero() || !c.Next.IsZero() || c.Running {
		t.Fatal(c)
	}
	// Simulate termination after the item committed and before the slot finished.
	c := f.state(t)
	c.Running = true
	b, _ := json.Marshal(c)
	if err := f.api.plugins.system.Set(t.Context(), rankControlKey, string(b)); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	if err := f.api.rankTick(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT count(*) FROM RSS_MOVIES", 1)
	f.count(t, "SELECT count(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_added'", 1)
	if c := f.state(t); c.Running || !strings.Contains(c.Last, "新增订阅 0 条") {
		t.Fatal(c)
	}
	f.feed = strings.ReplaceAll(f.feed, "榜单一", "另一榜单")
	f.action(t, "config", cfg)
	f.run(t)
	f.count(t, "SELECT count(*) FROM RSS_MOVIES", 1)
	f.count(t, "SELECT count(*) FROM RSS_TORRENTS", 2)
	f.count(t, "SELECT count(*) FROM PLUGIN_HISTORY WHERE PLUGIN_ID='DoubanRank'", 1)
}
