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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type randomFixture struct {
	path, token string
	api         *doubanSyncAPI
	h           http.Handler
	db          *sql.DB
	pages       int
	ids         []int
	owned       map[int]bool
	queries     []url.Values
	onDiscover  func()
}

func newRandomFixture(t *testing.T) *randomFixture {
	t.Helper()
	f := &randomFixture{path: filepath.Join(t.TempDir(), "config.yaml"), pages: 1, ids: []int{21}, owned: map[int]bool{}}
	if err := os.WriteFile(f.path, []byte("app:\n  login_user: admin\n  login_password: password\n  rmt_tmdbkey: fixture\nsecurity:\n  api_key: fixture-token\nmedia:\n  media_server: emby\nemby:\n  host: https://media.test\n  api_key: fixture-emby\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.token = loginForTest(t, f.h, "admin", "password")
	var err error
	f.db, err = sql.Open("sqlite", filepath.Join(filepath.Dir(f.path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	return f
}
func (f *randomFixture) start(t *testing.T) {
	t.Helper()
	var runner *rssRunAPI
	h, err := buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Host+r.URL.Path == "api.themoviedb.org/3/discover/movie":
			f.queries = append(f.queries, r.URL.Query())
			if f.onDiscover != nil {
				f.onDiscover()
			}
			var rows []string
			for _, id := range f.ids {
				poster := "/poster.jpg"
				if id == 24 {
					poster = ""
				}
				rows = append(rows, fmt.Sprintf(`{"id":%d,"title":"Film %d","original_language":"en","genre_ids":[18],"release_date":"2025-01-01","vote_average":8.25,"poster_path":%q}`, id, id, poster))
			}
			return jsonResponse(r, fmt.Sprintf(`{"total_pages":%d,"results":[%s]}`, f.pages, strings.Join(rows, ","))), nil
		case r.URL.Host == "api.themoviedb.org" && strings.HasPrefix(r.URL.Path, "/3/movie/"):
			id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/3/movie/"))
			if err != nil {
				t.Fatal(err)
			}
			return jsonResponse(r, fmt.Sprintf(`{"id":%d,"title":"Film %d","release_date":"2025-01-01","vote_average":8.25,"poster_path":"/poster.jpg"}`, id, id)), nil
		case r.URL.Host+r.URL.Path == "media.test/emby/Items":
			var rows []string
			for id := range f.owned {
				rows = append(rows, fmt.Sprintf(`{"Id":"item%d","Name":"Film %d","Type":"Movie","ProductionYear":2025,"ProviderIds":{"Tmdb":"%d"}}`, id, id, id))
			}
			return jsonResponse(r, `{"Items":[`+strings.Join(rows, ",")+`]}`), nil
		}
		t.Errorf("unexpected request: %s %s", r.URL.Host, r.URL.Path)
		return nil, context.Canceled
	}), &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	f.api = runner.archivePlugin.doubanSync
}
func (f *randomFixture) action(t *testing.T, action, values string, want int) {
	t.Helper()
	w := performFormRequest(f.h, "/api/v1/plugin/"+action, f.token, url.Values{"id": {movieRandomID}, "config": {values}})
	if w.Code != want {
		t.Fatal(w.Code, w.Body.String())
	}
}
func (f *randomFixture) state(t *testing.T) doubanControl {
	t.Helper()
	raw, err := f.api.plugins.system.Get(t.Context(), movieRandomControlKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := doubanState(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func (f *randomFixture) sql(t *testing.T, query string) {
	t.Helper()
	if _, err := f.db.Exec(query); err != nil {
		t.Fatal(err)
	}
}
func (f *randomFixture) count(t *testing.T, query string, want int) {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query).Scan(&n); err != nil || n != want {
		t.Fatal(query, n, want, err)
	}
}

const randomOnce = `{"enable":true,"onlyonce":true,"cron":"0 7 * * *","date":2020,"vote":"8.1","language":"英语","genres":"剧情"}`

func TestMovieRandomScheduleRestartAndExistingLibrary(t *testing.T) {
	f := newRandomFixture(t)
	if err := f.api.plugins.system.Set(t.Context(), movieRandomSettingsKey, `{"future":{"keep":true},"onlyonce":true}`); err != nil {
		t.Fatal(err)
	}
	f.action(t, "install", "", 200)
	if err := f.api.movieRandomTick(t.Context(), time.Now().Add(24*time.Hour)); err != nil || len(f.queries) != 0 {
		t.Fatal(err)
	}
	if err := f.api.plugins.system.Set(t.Context(), "DefaultRssSettingMOV", `{"include":"Wanted"}`); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/plugins/MovieRandom"
	w := nativeJSONRequest(f.h, "GET", path, f.token, "")
	var got struct{ Data pluginConfigDetail }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Data.Fields) != 7 || got.Data.Values["language"] != "全部" {
		t.Fatal(w.Body.String())
	}
	body := fmt.Sprintf(`{"values":%s,"fingerprint":%q}`, randomOnce, got.Data.Meta.Fingerprint)
	if w = nativeJSONRequest(f.h, "PUT", path, f.token, body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = nativeJSONRequest(f.h, "PUT", path, f.token, body); w.Code != 409 {
		t.Fatal(w.Code)
	}
	f.start(t)
	at := f.state(t).Once
	if err := f.api.movieRandomTick(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	if len(f.queries) != 1 {
		t.Fatal(f.queries)
	}
	q := f.queries[0]
	for k, v := range map[string]string{"page": "1", "vote_average.gte": "8.1", "primary_release_date.gte": "2020-01-01", "with_original_language": "en", "with_genres": "18", "api_key": "fixture"} {
		if q.Get(k) != v {
			t.Fatal(k, q.Get(k))
		}
	}
	f.count(t, "SELECT COUNT(*) FROM RSS_MOVIES WHERE TMDBID='21' AND STATE='D' AND FILTER_INCLUDE='Wanted'", 1)
	f.count(t, "SELECT COUNT(*) FROM PLUGIN_HISTORY WHERE PLUGIN_ID='MovieRandom' AND KEY='21' AND json_extract(VALUE,'$.rating')=8.2", 1)
	f.count(t, "SELECT COUNT(*) FROM RSS_TORRENTS WHERE TORRENT_NAME='movierandom: Film 21 (DB:21)' AND ENCLOSURE IS NULL", 1)
	if raw, _ := f.api.plugins.system.Get(t.Context(), movieRandomSettingsKey); !strings.Contains(raw, `"future":{"keep":true}`) || !strings.Contains(raw, `"onlyonce":false`) {
		t.Fatal(raw)
	}
	f.start(t)
	if err := f.api.movieRandomTick(t.Context(), at); err != nil || len(f.queries) != 1 {
		t.Fatal(err, len(f.queries))
	}
	f.ids = []int{21, 22, 23, 24}
	f.owned[22] = true
	f.sql(t, "CREATE TABLE IF NOT EXISTS RSS_HISTORY(ID INTEGER PRIMARY KEY,TYPE TEXT,NAME TEXT,YEAR TEXT,TMDBID TEXT,SEASON TEXT); INSERT INTO RSS_HISTORY(TYPE,NAME,YEAR,TMDBID) VALUES('MOV','Film 23','2025','23')")
	if err := f.api.movieRandomTick(t.Context(), f.state(t).Next); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT COUNT(*) FROM PLUGIN_HISTORY WHERE KEY='22' AND json_extract(VALUE,'$.state')='DOWNLOADED'", 1)
	f.count(t, "SELECT COUNT(*) FROM PLUGIN_HISTORY WHERE KEY='23' AND json_extract(VALUE,'$.state')='RSS'", 1)
	f.count(t, "SELECT COUNT(*) FROM RSS_MOVIES", 1)
	f.ids = append(f.ids, 25, 26)
	if err := f.api.movieRandomTick(t.Context(), f.state(t).Next); err != nil {
		t.Fatal(err)
	}
	f.count(t, "SELECT COUNT(*) FROM RSS_MOVIES", 2)
	f.count(t, "SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_added'", 2)
}
func TestMovieRandomSlotCancellationRollbackAndLimits(t *testing.T) {
	for _, mode := range []string{"uninstall", "rollback", "existing", "small-pages", "max-pages", "overlapping"} {
		t.Run(mode, func(t *testing.T) {
			f := newRandomFixture(t)
			f.action(t, "install", "", 200)
			f.action(t, "config", `{"onlyonce":true}`, 200)
			at := f.state(t).Once
			switch mode {
			case "uninstall":
				f.onDiscover = func() { f.action(t, "uninstall", "", 200) }
			case "overlapping":
				f.onDiscover = func() {
					f.onDiscover = nil
					if err := f.api.movieRandomTick(t.Context(), at); err != nil {
						t.Fatal(err)
					}
				}
			case "rollback":
				f.sql(t, "CREATE TRIGGER reject_movie_slot BEFORE UPDATE ON SYSTEM_DICT WHEN NEW.KEY='GoMovieRandom' BEGIN SELECT RAISE(IGNORE); END")
			case "existing":
				f.sql(t, nativeSubscriptionSchema("MOV"))
				f.sql(t, "INSERT INTO RSS_MOVIES(NAME,YEAR,TMDBID,STATE) VALUES('Film 21','2025','21','R')")
			case "small-pages":
				f.pages = 2
				f.ids = nil
			case "max-pages":
				f.pages = 700
				f.ids = nil
			}
			err := f.api.movieRandomTick(t.Context(), at)
			switch mode {
			case "uninstall":
				if err == nil {
					t.Fatal("uninstall ignored")
				}
				f.count(t, "SELECT COUNT(*) FROM RSS_TORRENTS", 0)
			case "rollback":
				if err == nil || !f.state(t).Once.Equal(at) {
					t.Fatal("slot advanced", err)
				}
				f.count(t, "SELECT COUNT(*) FROM RSS_TORRENTS", 0)
				f.sql(t, "DROP TRIGGER reject_movie_slot")
				f.start(t)
				if err = f.api.movieRandomTick(t.Context(), at); err != nil {
					t.Fatal(err)
				}
				f.count(t, "SELECT COUNT(*) FROM RSS_MOVIES", 1)
				f.count(t, "SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='rss_added'", 1)
			case "overlapping":
				if err != nil || len(f.queries) != 1 {
					t.Fatal(err, len(f.queries))
				}
				f.count(t, "SELECT COUNT(*) FROM RSS_MOVIES", 1)
			case "existing":
				if err != nil {
					t.Fatal(err)
				}
				f.count(t, "SELECT COUNT(*) FROM RSS_MOVIES WHERE TMDBID='21' AND STATE='R'", 1)
				f.count(t, "SELECT COUNT(*) FROM PLUGIN_HISTORY WHERE KEY='21' AND json_extract(VALUE,'$.state')='RSS'", 1)
				if !strings.Contains(f.state(t).Last, "未新增") {
					t.Fatal(f.state(t))
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				visited := map[int]bool{}
				for _, q := range f.queries {
					n, e := strconv.Atoi(q.Get("page"))
					if e != nil || n < 1 || n > min(500, f.pages) || visited[n] {
						t.Fatal(n, visited)
					}
					visited[n] = true
				}
				if mode == "small-pages" && len(visited) != 2 || mode == "max-pages" && (len(visited) < 5 || len(visited) > 6) {
					t.Fatal(visited)
				}
			}
		})
	}
}
func TestMovieRandomInvalidSettings(t *testing.T) {
	f := newRandomFixture(t)
	f.action(t, "install", "", 200)
	for _, cfg := range []string{`{"genres":"typo"}`, `{"language":"en"}`, `{"vote":"NaN"}`, `{"date":"2020-01-01"}`, `{"vote":10.1}`, `{"enable":true}`, `{"onlyonce":true,"cron":"wrong"}`, `{"unknown":"change"}`} {
		f.action(t, "config", cfg, 400)
	}
	if c := f.state(t); c.Actor != "" || !c.Once.IsZero() {
		t.Fatal(c)
	}
}

func TestMovieRandomVerifiesDiscoverFilters(t *testing.T) {
	item := tmdbRecommendation{ID: 21, Title: "Film", PosterPath: "/film.jpg", ReleaseDate: "2025-01-01", VoteAverage: 8.5, OriginalLanguage: "en", GenreIDs: []int{12, 18}}
	for _, tc := range []struct {
		key, value string
		want       bool
	}{
		{"primary_release_date.gte", "2025-01-01", true}, {"primary_release_date.gte", "2026-01-01", false},
		{"vote_average.gte", "8.5", true}, {"vote_average.gte", "8.6", false},
		{"with_original_language", "en", true}, {"with_original_language", "fr", false},
		{"with_genres", "18", true}, {"with_genres", "35", false},
	} {
		if matchesRandomFilters(item, map[string]string{tc.key: tc.value}) != tc.want {
			t.Fatal(tc)
		}
	}
}
