package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestSiteFeedCompatibility(t *testing.T) {
	const body = `<?xml version="1.0"?><rdf:RDF xmlns:rdf="urn:fixture"><item><title>[国语] [Movie.2026]</title><description><![CDATA[<p>说明</p>]]></description><enclosure url="https://fixture.invalid/t?a=1&amp;b=2" length="bad"/><pubDate>Fri, 14 Oct 2022 07:48:04 GMT</pubDate></item><item><title>Title [release]</title><link>magnet:?xt=fixture</link></item><item><enclosure url="ignored"/></item><item><title>ignored</title></item></rdf:RDF>`
	got, err := parseClassicSiteFeed([]byte(body), "www.pt.keepfrds.com")
	if err != nil || len(got) != 2 || got[0].Title != "Movie.2026 [国语]" || got[0].Enclosure != "https://fixture.invalid/t?a=1&b=2" || got[0].Description != "<p>说明</p>" || got[0].PubDate != "Fri, 14 Oct 2022 07:48:04 GMT" || got[0].Size != 0 || got[0].Link == nil || *got[0].Link != "" || got[1].Title != "release Title" || got[1].Link != nil {
		t.Fatal(got, err)
	}
	if _, err := parseSubscriptionRSS([]byte(body)); err == nil {
		t.Fatal("native subscription accepted RDF")
	}
	if _, err := parseClassicSiteFeed([]byte(body+"<"), ""); err == nil {
		t.Fatal("partial feed accepted")
	}
	for _, s := range []string{" RSS Link has expired, You need to get a new one!\n", "RSS 链接已过期，您需要获得一个新的！"} {
		if _, err := parseClassicSiteFeed([]byte(s), ""); !errors.Is(err, errExpiredSiteFeed) {
			t.Fatal(err)
		}
	}
}
func TestSiteFeedProxyRedirectCharsetAndHistory(t *testing.T) {
	ctx := context.Background()
	const body = `<rss><channel><item><title>影片</title><enclosure url="fixture-a" length="42"/></item><item><title>另一部</title><link>fixture-b</link></item></channel></rss>`
	encoded, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" || r.Header.Get("Cookie") != "" || r.UserAgent() != "fixture-agent" {
			t.Error("unexpected feed headers")
		}
		w.Header().Set("Content-Type", "application/rss+xml; charset=gbk")
		w.Write(encoded)
	}))
	defer dest.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "original.invalid" {
			t.Error("unexpected proxy destination")
		}
		http.Redirect(w, r, dest.URL, http.StatusFound)
	}))
	defer proxy.Close()
	app := map[string]any{"user_agent": "fixture-agent", "proxies": map[string]any{"http": proxy.URL}}
	items, err := fetchSiteFeed(ctx, "http://original.invalid/feed?passkey=fixture", true, app)
	if err != nil || len(items) != 2 || items[0].Title != "影片" || items[0].Size != 42 {
		t.Fatal(items, err)
	}
	if _, err := fetchSiteFeed(ctx, "file:///fixture", false, app); err == nil {
		t.Fatal("accepted non-HTTP URL")
	}
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE RSS_TORRENTS(ENCLOSURE TEXT);INSERT INTO RSS_TORRENTS VALUES('fixture-a'),('fixture-a')`); err != nil {
		t.Fatal(err)
	}
	items = append(items, items[1])
	items, err = filterRecordedArticles(ctx, filepath.Join(dir, "config.yaml"), items)
	if err != nil || len(items) != 2 || items[0].Title != "另一部" || items[1].Title != "另一部" {
		t.Fatal(items, err)
	}
}

func TestSiteFeedReadErrors(t *testing.T) {
	for _, test := range []struct {
		status  int
		body    string
		expired bool
	}{{404, "RSS Link has expired, You need to get a new one!", false}, {200, "RSS Link has expired, You need to get a new one!", true}, {200, "<rss>", false}, {200, strings.Repeat(" ", siteFeedLimit+1), false}} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.status); w.Write([]byte(test.body)) }))
		_, err := fetchSiteFeed(context.Background(), s.URL, false, nil)
		s.Close()
		if err == nil || errors.Is(err, errExpiredSiteFeed) != test.expired {
			t.Fatalf("status %d: %v", test.status, err)
		}
	}
	var out bytes.Buffer
	err := SiteFeedCommand(context.Background(), "", strings.NewReader(`{"url":"abc","unexpected":true}`), &out)
	if err == nil || out.Len() != 0 {
		t.Fatal(err, out.String())
	}
	empty, err := parseClassicSiteFeed([]byte("<rss><channel/></rss>"), "")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal(empty, err)
	}
}

func TestSiteFeedCacheResetUsesSubscriptionPermissionAndRollsBack(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	const route = "/api/v1/subscribe/cache/delete"
	if _, err := f.db.Exec(`INSERT INTO RSS_TORRENTS(TORRENT_NAME) VALUES('fixture');
		CREATE TRIGGER fail_clear BEFORE DELETE ON RSS_TV_EPISODES BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if res := performFormRequest(f.handler, route, "", nil); res.Code != 401 {
		t.Fatal(res.Code)
	}
	if res := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"fixture-pass"}, "pris": {""}}); res.Code != 200 {
		t.Fatal(res.Code)
	}
	viewer := loginForTest(t, f.handler, "viewer", "fixture-pass")
	if res := performFormRequest(f.handler, route, viewer, nil); res.Code != 403 {
		t.Fatal(res.Code)
	}
	if res := performFormRequest(f.handler, route, f.token, nil); res.Code != 502 {
		t.Fatal(res.Code)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS WHERE TORRENT_NAME='fixture'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, err := f.db.Exec("DROP TRIGGER fail_clear"); err != nil {
		t.Fatal(err)
	}
	if res := performFormRequest(f.handler, route, f.token, nil); res.Code != 200 {
		t.Fatal(res.Code)
	}
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM RSS_TV_EPISODES)+(SELECT COUNT(*) FROM RSS_TORRENTS)`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err := f.db.QueryRow(`SELECT LACK FROM RSS_TVS WHERE ID=2`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}
