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

func TestDoubanSyncHistoryAndSubscriptionTransactionAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  login_user: admin\n  login_password: password\n  rmt_tmdbkey: fixture\nsecurity:\n  api_key: fixture-admin-key\nmedia:\n  media_server: emby\nemby:\n  host: https://media.test\n  api_key: fixture-emby\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const apiPath = "/api/v1/plugins/DoubanSync"
	var calls, subjectCalls int
	id := "123"
	sourceTitle := "Fixture Film"
	tmdb := "32"
	owned := false
	start := func() (http.Handler, *doubanSyncAPI) {
		var runner *rssRunAPI
		t.Helper()
		h, err := buildHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Header.Get("Cookie") != "" {
				t.Fatal("cookie left movie web host")
			}
			switch r.URL.Host + r.URL.Path {
			case "www.douban.com/feed/people/fixture/interests":
				return jsonResponse(r, fmt.Sprintf("<rss><channel><item><title>想看%s</title><link>https://movie.douban.com/subject/%s/</link><pubDate>Wed, 25 Nov 2026 11:00:00 GMT</pubDate></item></channel></rss>", sourceTitle, id)), nil
			case "frodo.douban.com/api/v2/movie/" + id:
				subjectCalls++
				return jsonResponse(r, fmt.Sprintf(`{"id":"%s","title":"%s","year":"2020","rating":{"value":8.2}}`, id, sourceTitle)), nil
			case "media.test/emby/Items":
				if owned {
					return jsonResponse(r, fmt.Sprintf(`{"Items":[{"Id":"stored","Name":"%s","ProductionYear":2020,"ProviderIds":{"Tmdb":"%s"}}]}`, sourceTitle, tmdb)), nil
				}
				return jsonResponse(r, `{"Items":[]}`), nil
			case "api.themoviedb.org/3/search/movie":
				return jsonResponse(r, fmt.Sprintf(`{"results":[{"id":%s,"title":"%s","release_date":"2020-01-01"}]}`, tmdb, sourceTitle)), nil
			case "api.themoviedb.org/3/movie/" + tmdb:
				return jsonResponse(r, fmt.Sprintf(`{"id":%s,"title":"%s","release_date":"2020-01-01"}`, tmdb, sourceTitle)), nil
			default:
				t.Fatal("unexpected request", r.URL.Host, r.URL.Path)
				return nil, nil
			}
		}), &runner)
		if err != nil {
			t.Fatal(err)
		}
		api := runner.archivePlugin.doubanSync
		api.pause = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
		return h, api
	}
	h, api := start()
	token := loginForTest(t, h, "admin", "password")
	ctx := t.Context()
	if err := api.plugins.system.Set(ctx, doubanSettingsKey, `{"extra":{"keep":true},"cookie":"secret-fixture","enable":true,"onlyonce":true,"users":["fixture"],"types":"wish","sync_type":"1","rss_interval":1}`); err != nil {
		t.Fatal(err)
	}
	if w := nativeJSONRequest(h, "POST", apiPath+"/install", token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	tick := func(at time.Time) {
		t.Helper()
		if err := api.tick(ctx, at); err != nil {
			t.Fatal(err)
		}
	}
	get := func() (string, pluginConfigDetail) {
		t.Helper()
		w := nativeJSONRequest(h, "GET", apiPath, token, "")
		var v struct{ Data pluginConfigDetail }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || strings.Contains(w.Body.String(), "secret-fixture") {
			t.Fatal(w.Code, w.Body.String())
		}
		return v.Data.Meta.Fingerprint, v.Data
	}
	control := func() doubanControl {
		t.Helper()
		v, err := api.plugins.system.Get(ctx, doubanControlKey)
		if err != nil {
			t.Fatal(err)
		}
		c, err := doubanState(v)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	save := func(values string, status int) {
		t.Helper()
		fp, _ := get()
		w := nativeJSONRequest(h, "PUT", apiPath, token, `{"values":`+values+`,"fingerprint":"`+fp+`"}`)
		if w.Code != status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	tick(time.Now().Add(time.Hour))
	if calls != 0 {
		t.Fatal("inherited flags executed")
	}
	save(`{"auto_search":true,"auto_rss":false,"onlyonce":true}`, 501)
	save(`{"onlyonce":true,"auto_search":false,"auto_rss":false}`, 200)
	c := control()
	if c.Next.Sub(c.Once) < 296*time.Second || c.Next.Sub(c.Once) > 298*time.Second {
		t.Fatal(c)
	}
	h, api = start()
	tick(c.Once)
	if state, err := api.historyState(ctx, id); err != nil || state != "NEW" {
		t.Fatal(state, err, control())
	}
	tick(c.Next)
	if subjectCalls != 1 {
		t.Fatal("already stored item fetched again", subjectCalls)
	}
	if err := api.plugins.system.Set(ctx, "DefaultRssSettingMOV", `{"restype":"BluRay","include":"Wanted"}`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TRIGGER fixture_no_history BEFORE UPDATE ON PLUGIN_HISTORY BEGIN SELECT RAISE(IGNORE); END"); err != nil {
		t.Fatal(err)
	}
	save(`{"onlyonce":true,"auto_rss":true}`, 200)
	tick(control().Once)
	if c = control(); !strings.Contains(c.Last, "失败 1") {
		t.Fatal(c)
	}
	state, err := api.historyState(ctx, id)
	if err != nil || state != "NEW" {
		t.Fatal(state, err)
	}
	var tables int
	if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='RSS_MOVIES'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		var n int
		if err = db.QueryRow("SELECT COUNT(*) FROM RSS_MOVIES").Scan(&n); err != nil || n != 0 {
			t.Fatal(n, err)
		}
	}
	if _, err = db.Exec("DROP TRIGGER fixture_no_history"); err != nil {
		t.Fatal(err)
	}
	save(`{"onlyonce":true,"auto_rss":true}`, 200)
	tick(control().Once)
	var quality, include, rssState string
	if err = db.QueryRow("SELECT FILTER_RESTYPE,FILTER_INCLUDE,STATE FROM RSS_MOVIES WHERE TMDBID='32'").Scan(&quality, &include, &rssState); err != nil || quality != "BluRay" || include != "Wanted" || rssState != "R" {
		t.Fatal(err, quality, include, rssState, control())
	}
	if state, err = api.historyState(ctx, id); err != nil || state != "RSS" {
		t.Fatal(state, err)
	}
	before := subjectCalls
	h, api = start()
	tick(control().Next)
	if subjectCalls != before {
		t.Fatal("restart repeated subscription metadata fetch")
	}
	// Search+subscription first checks current media inventory. An existing
	// movie records DOWNLOADED; a missing one enters the search queue in D.
	id, sourceTitle, tmdb, owned = "124", "Owned Movie", "33", true
	save(`{"onlyonce":true,"auto_search":true,"auto_rss":true}`, 200)
	tick(control().Once)
	if state, err = api.historyState(ctx, id); err != nil || state != "DOWNLOADED" {
		t.Fatal(state, err, control())
	}
	id, sourceTitle, tmdb, owned = "125", "Missing Movie", "34", false
	tick(control().Next)
	if state, err = api.historyState(ctx, id); err != nil || state != "RSS" {
		t.Fatal(state, err, control())
	}
	if err = db.QueryRow("SELECT STATE FROM RSS_MOVIES WHERE TMDBID='34'").Scan(&rssState); err != nil || rssState != "D" {
		t.Fatal(rssState, err)
	}
	var ownedRows int
	if err = db.QueryRow("SELECT COUNT(*) FROM RSS_MOVIES WHERE TMDBID='33'").Scan(&ownedRows); err != nil || ownedRows != 0 {
		t.Fatal(ownedRows, err)
	}
	w := performFormRequest(h, "/api/v1/plugin/list", token, url.Values{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "rss_interval") || strings.Contains(w.Body.String(), "secret-fixture") {
		t.Fatal(w.Code, w.Body.String())
	}
	v, _ := api.plugins.system.Get(ctx, doubanSettingsKey)
	if !strings.Contains(v, `"extra":{"keep":true}`) || !strings.Contains(v, `"onlyonce":false`) {
		t.Fatal(v)
	}
	if w := nativeJSONRequest(h, "DELETE", apiPath, token, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	before = calls
	tick(time.Now().Add(24 * time.Hour))
	if calls != before {
		t.Fatal("uninstall failed to stop worker")
	}
}
