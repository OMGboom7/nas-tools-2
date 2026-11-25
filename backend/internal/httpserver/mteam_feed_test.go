package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type mteamFixture struct {
	path, token string
	paired      bool
	handler     http.Handler
	feed        *mteamFeedService
	db          *sql.DB
	requests    atomic.Int32
	hook        func(*http.Request, string) (*http.Response, error)
}

const mtResponse = `{"code":"0","data":{"data":[{"id":"42","name":"Fixture & Movie","category":"401","size":"2147483649","createdDate":"2026-11-25 08:00:00","status":{"discount":"FREE"}}]}}`

func newMteamFixture(t *testing.T, paired bool) *mteamFixture {
	t.Helper()
	f := &mteamFixture{path: filepath.Join(t.TempDir(), "config.yaml"), paired: paired}
	if err := os.WriteFile(f.path, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.token = loginForTest(t, f.handler, "admin", "password")
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(f.path), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(`INSERT INTO CONFIG_SITE(PRI,SIGNURL,APIKEY,COOKIE,NOTE) VALUES
	('2','https://kp.m-team.io/login','fixture-key','fixture-cookie','{"ua":"Fixture Agent"}'),
	('1','https://kp.m-team.cc/login','other-key','','{}'),
	('3','https://m-team.io.invalid/login','third-key','','{}')`); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *mteamFixture) start(t *testing.T) {
	t.Helper()
	var runner *rssRunAPI
	cfg := config.Config{ApplicationConfigPath: f.path, DisableLegacy: !f.paired}
	if f.paired {
		cfg.ManagedMTeamFeed = true
		cfg.LegacyBackendURL = "http://127.0.0.1:3002"
	}
	h, err := buildHandler(cfg, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.m-team.io/api/torrent/search" || r.UserAgent() != "Fixture Agent" || r.Header.Get("x-api-key") != "fixture-key" || r.Header.Get("cookie") != "" {
			t.Errorf("unexpected request to %s", r.URL.Host)
			return nil, context.Canceled
		}
		var q struct{ Mode string }
		if json.NewDecoder(r.Body).Decode(&q) != nil {
			t.Error("bad search body")
		}
		f.requests.Add(1)
		if f.hook != nil {
			return f.hook(r, q.Mode)
		}
		return jsonResponse(r, mtResponse), nil
	}), &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	f.feed = runner.archivePlugin.mteamFeed
	if f.feed == nil || f.paired && runner.archivePlugin.helper != nil {
		t.Fatal("unexpected feed or downloader helper ownership")
	}
}
func (f *mteamFixture) action(t *testing.T, action, cfg string, want int) {
	t.Helper()
	w := performFormRequest(f.handler, "/api/v1/plugin/"+action, f.token, url.Values{"id": {mteamFeedID}, "config": {cfg}})
	if w.Code != want {
		t.Fatal(w.Code, w.Body.String())
	}
}
func (f *mteamFixture) read(t *testing.T, path, method string, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.feed.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	if w.Code != want {
		t.Fatal(w.Code, w.Body.String())
	}
	return w
}
func TestMTeamFeedCompatibilityAndEdits(t *testing.T) {
	f := newMteamFixture(t, false)
	f.action(t, "install", "", 200)
	if err := f.feed.system.Set(t.Context(), mteamFeedKey, `{"watch_categories":[],"future":"private-fixture"}`); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/plugins/MteamRssGen"
	w := nativeJSONRequest(f.handler, "GET", path, f.token, "")
	var got struct{ Data pluginConfigDetail }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Data.Fields) != 3 || strings.Contains(w.Body.String(), "private-fixture") {
		t.Fatal(w.Body.String())
	}
	body := fmt.Sprintf(`{"fingerprint":%q,"values":{"enabled":true,"torrent_min_size":"2","watch_categories":["normal","movie","normal"]}}`, got.Data.Meta.Fingerprint)
	if w = nativeJSONRequest(f.handler, "PUT", path, f.token, body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = nativeJSONRequest(f.handler, "PUT", path, f.token, body); w.Code != 409 {
		t.Fatal(w.Code)
	}
	for _, cfg := range []string{`{"torrent_min_size":-1}`, `{"torrent_min_size":"8589934592"}`, `{"watch_categories":["unknown"]}`, `{"watch_categories":[]}`, `{"future":"change"}`} {
		f.action(t, "config", cfg, 400)
	}
	f.start(t)
	w = f.read(t, "/rss_pub.xml", "GET", 200)
	rows, err := parseSubscriptionRSS(w.Body.Bytes())
	if err != nil || len(rows) != 1 || rows[0].Size != 2147483649 || rows[0].Title != "Fixture & Movie" || rows[0].DownloadURL != "https://kp.m-team.io/detail/42" || rows[0].PageURL != rows[0].DownloadURL || f.requests.Load() != 2 {
		t.Fatal(rows, err, f.requests.Load())
	}
	if strings.Contains(w.Body.String(), "fixture-key") || strings.Contains(w.Body.String(), "fixture-cookie") {
		t.Fatal("private fields in feed")
	}
	f.read(t, "/config.yaml", "GET", 404)
	f.read(t, "/rss_pub.xml", "POST", 405)
	f.read(t, "/nested/rss_pub.xml", "GET", 404)
	f.action(t, "uninstall", "", 200)
	f.read(t, "/rss_pub.xml", "GET", 503)
	if f.requests.Load() != 2 {
		t.Fatal(f.requests.Load())
	}
	raw, _ := f.feed.system.Get(t.Context(), mteamFeedKey)
	if !strings.Contains(raw, "private-fixture") {
		t.Fatal(raw)
	}
}
func TestMTeamFeedFailedCategoriesAndChangedSource(t *testing.T) {
	for _, mode := range []string{"partial", "all-failed", "changed-site", "stopped", "missing-proxy"} {
		t.Run(mode, func(t *testing.T) {
			f := newMteamFixture(t, false)
			f.action(t, "install", "", 200)
			f.action(t, "config", `{"enabled":true,"watch_categories":["movie","tvshow"]}`, 200)
			if mode == "missing-proxy" {
				if _, err := f.db.Exec(`UPDATE CONFIG_SITE SET NOTE='{"proxy":"Y"}' WHERE PRI='2'`); err != nil {
					t.Fatal(err)
				}
			}
			f.hook = func(r *http.Request, category string) (*http.Response, error) {
				switch mode {
				case "changed-site":
					if _, err := f.db.Exec("UPDATE CONFIG_SITE SET APIKEY='rotated' WHERE PRI='2'"); err != nil {
						t.Fatal(err)
					}
				case "stopped":
					f.action(t, "config", `{"enabled":false}`, 200)
				case "partial":
					if category == "movie" {
						return jsonResponse(r, mtResponse), nil
					}
					fallthrough
				case "all-failed":
					return jsonResponse(r, `{"code":"error"}`), nil
				}
				return jsonResponse(r, mtResponse), nil
			}
			switch mode {
			case "partial":
				w := f.read(t, "/rss_pub.xml", "GET", 200)
				if w.Header().Get("X-Nastool-Failed-Categories") != "1" || !strings.Contains(w.Body.String(), "/detail/42") {
					t.Fatal(w.Result())
				}
			case "all-failed":
				f.read(t, "/rss_pub.xml", "GET", 502)
			case "missing-proxy":
				f.read(t, "/rss_pub.xml", "GET", 502)
				if f.requests.Load() != 0 {
					t.Fatal(f.requests.Load())
				}
			default:
				f.read(t, "/rss_pub.xml", "GET", 503)
				if f.requests.Load() != 1 {
					t.Fatal(f.requests.Load())
				}
			}
		})
	}
}
func TestMTeamFeedListenerRestartPortConflictAndUninstall(t *testing.T) {
	t.Run("standalone", func(t *testing.T) { testMTeamListener(t, false) })
	t.Run("paired", func(t *testing.T) { testMTeamListener(t, true) })
}

func testMTeamListener(t *testing.T, paired bool) {
	f := newMteamFixture(t, paired)
	port, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	address := port.Addr().String()
	f.feed.address = address
	ctx, cancel := context.WithCancel(t.Context())
	wait := startMTeamFeed(ctx, f.feed)
	defer func() { cancel(); wait() }()
	f.action(t, "install", "", 200)
	f.action(t, "config", `{"enabled":true,"watch_categories":["normal"]}`, 503)
	if f.requests.Load() != 0 {
		t.Fatal("unexpected polling")
	}
	if err := port.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.feed.reload(); err != nil {
		t.Fatal(err)
	}
	read := func() {
		res, err := http.Get("http://" + address + "/rss_pub.xml")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != 200 || !strings.Contains(string(data), "Fixture &amp; Movie") {
			t.Fatal(res.StatusCode, err)
		}
	}
	read()
	state := performFormRequest(f.handler, "/api/v1/plugin/status", f.token, url.Values{"id": {mteamFeedID}})
	if state.Code != 200 || !strings.Contains(state.Body.String(), `"state":true`) {
		t.Fatal(state.Code, state.Body.String())
	}
	cancel()
	wait()
	f.start(t)
	f.feed.address = address
	restart, stop := context.WithCancel(t.Context())
	joined := startMTeamFeed(restart, f.feed)
	defer func() { stop(); joined() }()
	if f.requests.Load() != 1 {
		t.Fatal("unexpected replay")
	}
	read()
	w := nativeJSONRequest(f.handler, "DELETE", "/api/v1/plugins/"+mteamFeedID, f.token, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	free, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal("listener retained after uninstall", err)
	}
	free.Close()
	if f.requests.Load() != 2 {
		t.Fatal(f.requests.Load())
	}
}
