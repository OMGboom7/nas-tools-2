package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func subscriptionFeedItem(title, enclosure, page, attrs string) string {
	return `<item><title>` + title + `</title><link>` + page + `</link><enclosure url="` + strings.ReplaceAll(enclosure, "&", "&amp;") + `" length="1024"/>` + attrs + `</item>`
}

func TestSubscriptionRSSParserBoundsAndUnknownAttributes(t *testing.T) {
	body := `<rss xmlns:t="urn:test"><channel>` + subscriptionFeedItem("Movie.2026", testMagnet, "https://tracker.test/details?id=1", `<t:attr name="seeders" value="0"/><t:attr name="downloadvolumefactor" value="0"/>`) + `</channel></rss>`
	items, err := parseSubscriptionRSS([]byte(body))
	if err != nil || len(items) != 1 || items[0].Seeders == nil || *items[0].Seeders != 0 || items[0].DownloadFactor == nil || *items[0].DownloadFactor != 0 || items[0].MinimumRatio != nil {
		t.Fatal(items, err)
	}
	unknown, err := parseSubscriptionRSS([]byte(`<rss><channel>` + subscriptionFeedItem("Movie", testMagnet, "", "") + `</channel></rss>`))
	if err != nil || unknown[0].Seeders != nil || unknown[0].DownloadFactor != nil {
		t.Fatal(unknown, err)
	}
	for _, bad := range []string{
		`<html><body>login</body></html>`, `<rss><channel>`, `<rss><channel/></rss><rss/>`, `<!DOCTYPE rss><rss><channel/></rss>`,
		`<rss><channel/><other>` + subscriptionFeedItem("Wrong parent", testMagnet, "", "") + `</other></rss>tail`,
		`<rss><channel>` + subscriptionFeedItem("Movie", "file:///tmp/torrent", "", "") + `</channel></rss>`,
		`<rss><channel>` + subscriptionFeedItem("Movie", testMagnet, "", `<attr name="seeders" value="-1"/>`) + `</channel></rss>`,
		`<rss><channel>` + subscriptionFeedItem("Movie", testMagnet, "", `<attr name="minimumratio" value="NaN"/>`) + `</channel></rss>`,
		`<rss><channel>` + subscriptionFeedItem("Movie", testMagnet, "", `<attr name="seeders" value="1"/><attr name="seeders" value="2"/>`) + `</channel></rss>`,
		`<rss><channel><item>` + strings.Repeat("<x>", 65) + strings.Repeat("</x>", 65) + `</item></channel></rss>`,
		`<rss><channel>` + strings.Repeat(`<item/>`, 1001) + `</channel></rss>`, strings.Repeat(" ", (4<<20)+1),
	} {
		if _, err := parseSubscriptionRSS([]byte(bad)); err == nil {
			t.Errorf("accepted invalid feed (%d bytes)", len(bad))
		}
	}
	if items, err := parseSubscriptionRSS([]byte(`<rss><channel/></rss>`)); err != nil || len(items) != 0 {
		t.Fatal(items, err)
	}
}

func subscriptionTestDetailRules() map[string]json.RawMessage {
	return map[string]json.RawMessage{"FREE": json.RawMessage(`["//span[@class='free']"]`), "2XFREE": json.RawMessage(`["//span[@class='double']"]`), "HR": json.RawMessage(`["//span[@class='hr']"]`), "PEER_COUNT": json.RawMessage(`["//span[@id='seeders']"]`)}
}

func TestSubscriptionRSSDetailRequiresVerifiedIdentityAndPolicy(t *testing.T) {
	for _, test := range []struct {
		markup           string
		upload, download float64
		seeders          bool
	}{
		{"", 1, 1, false}, {`<span class="free"/>`, 1, 0, false}, {`<span class="double"/><span id="seeders">1,234 seeders</span>`, 2, 0, true},
	} {
		resource, err := parseSubscriptionDetail(t.Context(), []byte(`<html><h1>Movie.2026</h1>`+test.markup+`</html>`), "Movie.2026", subscriptionTestDetailRules(), externalindexer.Resource{})
		if err != nil || resource.UploadFactor == nil || *resource.UploadFactor != test.upload || resource.DownloadFactor == nil || *resource.DownloadFactor != test.download || (resource.Seeders != nil) != test.seeders {
			t.Fatal(resource, err)
		}
		if test.seeders && *resource.Seeders != 1234 {
			t.Fatal(resource.Seeders)
		}
	}
	for _, body := range []string{`<h1>Wrong torrent</h1>`, `<h1>Movie.2026</h1><input type="password">`, `<h1>Movie.2026</h1><div id="challenge-form"/>`, `<h1>Movie.2026</h1><span id="seeders">unknown</span>`} {
		if _, err := parseSubscriptionDetail(t.Context(), []byte(body), "Movie.2026", subscriptionTestDetailRules(), externalindexer.Resource{}); err == nil {
			t.Fatal("unverified detail accepted", body)
		}
	}
	if _, err := parseSubscriptionDetail(t.Context(), []byte(`<h1>Movie.2026</h1><span class="hr"/>`), "Movie.2026", subscriptionTestDetailRules(), externalindexer.Resource{}); !errors.Is(err, errSubscriptionRSSUnsupported) {
		t.Fatal(err)
	}
	rules := subscriptionTestDetailRules()
	rules["FREE"] = json.RawMessage(`["//["]`)
	if _, err := parseSubscriptionDetail(t.Context(), []byte(`<h1>Movie.2026</h1>`), "Movie.2026", rules, externalindexer.Resource{}); err == nil {
		t.Fatal("invalid XPath accepted")
	}
	catalog := indexercatalog.Catalog{Conf: map[string]map[string]json.RawMessage{"tracker.test": subscriptionTestDetailRules()}}
	if _, err := subscriptionDetailRules(catalog, "https://other.test/details"); !errors.Is(err, errSubscriptionRSSUnsupported) {
		t.Fatal(err)
	}
	catalog.Conf["tracker.test"]["RENDER"] = json.RawMessage(`true`)
	if _, err := subscriptionDetailRules(catalog, "https://tracker.test/details"); !errors.Is(err, errSubscriptionRSSUnsupported) {
		t.Fatal(err)
	}
}

func newSubscriptionFeedFixture(t *testing.T, tv bool, note string, feed func(*http.Request) (*http.Response, error)) *subscriptionRunFixture {
	t.Helper()
	f := newSubscriptionRunFixture(t)
	if tv {
		if _, err := f.db.Exec(`DELETE FROM RSS_MOVIES; UPDATE RSS_TVS SET STATE='R',RSS_SITES='["Tracker"]'`); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := f.db.Exec(`DELETE FROM RSS_TVS; UPDATE RSS_MOVIES SET STATE='R',RSS_SITES='["Tracker"]'`); err != nil {
			t.Fatal(err)
		}
	}
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tracker.test" {
			if r.URL.Path == "/rss" && r.Header.Get("Cookie") != "" {
				t.Fatal("feed leaked cookie")
			}
			if r.URL.Path != "/rss" && r.Header.Get("Cookie") != "session=private-site" {
				t.Fatal("missing site credentials")
			}
			return feed(r)
		}
		return base.RoundTrip(r)
	})
	catalog := indexercatalog.Catalog{Indexers: []indexercatalog.Definition{}, Conf: map[string]map[string]json.RawMessage{"tracker.test": subscriptionTestDetailRules()}}
	contents, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(t.TempDir(), "sites.dat")
	if err := os.WriteFile(catalogPath, []byte(base64.StdEncoding.EncodeToString(contents)), 0600); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true, SiteCatalogPath: catalogPath}, f.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.runner = runner
	if _, err := runner.download.sites.Upsert(t.Context(), siteconfig.Site{Name: "Tracker", Include: "D", RSSURL: "https://tracker.test/rss?passkey=private-feed", SignURL: "https://tracker.test", Cookie: "session=private-site", Note: note}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSubscriptionRSSExecutionTVProgressDetailAndProcessedDeduplication(t *testing.T) {
	var fetched, details atomic.Int32
	f := newSubscriptionFeedFixture(t, true, `{"parse":"Y","rule":"-1"}`, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/rss" {
			fetched.Add(1)
			body := ""
			for _, episode := range []int{1, 2, 3} {
				body += subscriptionFeedItem(fmt.Sprintf("Show.S01E%02d.1080p.WEB-DL", episode), fmt.Sprintf("magnet:?xt=urn:btih:%040x", episode), fmt.Sprintf("https://tracker.test/details?id=%d", episode), "")
			}
			return jsonResponse(r, `<rss><channel>`+body+`</channel></rss>`), nil
		}
		details.Add(1)
		return jsonResponse(r, fmt.Sprintf(`<html><h1>Show.S01E%02s.1080p.WEB-DL</h1><span class="free"/></html>`, r.URL.Query().Get("id"))), nil
	})
	if _, err := f.db.Exec(`INSERT INTO RSS_TORRENTS(ENCLOSURE) VALUES (?)`, fmt.Sprintf("magnet:?xt=urn:btih:%040x", 1)); err != nil {
		t.Fatal(err)
	}
	r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"submitted":2`) || !strings.Contains(r.Body.String(), `"completed":1`) || f.adds != 2 || f.queries != 0 || fetched.Load() != 1 || details.Load() != 2 {
		t.Fatal(r.Code, r.Body.String(), f.adds, f.queries, fetched.Load(), details.Load())
	}
	if strings.Contains(r.Body.String(), "private") || strings.Contains(r.Body.String(), "magnet:") {
		t.Fatal("private feed locator leaked")
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_TVS`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{}); r.Code != 200 || f.adds != 2 || fetched.Load() != 1 {
		t.Fatal("completed TV resubmitted", r.Body.String())
	}
}

func TestSubscriptionRSSExecutionSourceFailureAndUncertainSubmission(t *testing.T) {
	for _, test := range []struct {
		name, note, body string
		uncertain        bool
	}{
		{"bad-feed", "", `<html>login</html>`, false}, {"unknown-promotion", `{"parse":"Y"}`, `<rss><channel>` + subscriptionFeedItem("Movie.2026.1080p.WEB-DL", testMagnet, "https://tracker.test/details", "") + `</channel></rss>`, false},
		{"configured-limits", `{"limit_seconds":"1"}`, `<rss><channel/></rss>`, false}, {"uncertain", "", `<rss><channel>` + subscriptionFeedItem("Movie.2026.1080p.WEB-DL", testMagnet, "", "") + `</channel></rss>`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSubscriptionFeedFixture(t, false, test.note, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/rss" {
					return jsonResponse(r, `<h1>Login required</h1>`), nil
				}
				return jsonResponse(r, test.body), nil
			})
			f.addError = test.uncertain
			r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
			if r.Code != 200 || !strings.Contains(r.Body.String(), `"failed":1`) || !strings.Contains(r.Body.String(), `"completed":0`) || f.queries != 0 {
				t.Fatal(r.Code, r.Body.String())
			}
			want := 0
			if test.uncertain {
				want = 1
			}
			if f.adds != want {
				t.Fatal(f.adds)
			}
			f.addError = false
			performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
			if f.adds != want {
				t.Fatal("uncertain submission repeated")
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES WHERE STATE='R'`).Scan(&n); err != nil || n != 1 {
				t.Fatal(n, err)
			}
		})
	}
}

func TestSubscriptionRSSScopeAndPermissionsNeverWiden(t *testing.T) {
	var calls atomic.Int32
	f := newSubscriptionFeedFixture(t, false, "", func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(r, `<rss><channel/></rss>`), nil
	})
	if r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", "", url.Values{}); r.Code != 401 {
		t.Fatal(r.Code)
	}
	created := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	if r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", loginForTest(t, f.handler, "viewer", "strong-password"), url.Values{}); r.Code != 403 {
		t.Fatal(r.Code)
	}
	for _, scope := range []string{`["Missing"]`, `invalid-json`, `["Tracker","Missing"]`} {
		if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET RSS_SITES=?`, scope); err != nil {
			t.Fatal(err)
		}
		r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
		if !strings.Contains(r.Body.String(), `"failed":1`) || calls.Load() != 0 || f.adds != 0 {
			t.Fatal(scope, r.Body.String(), calls.Load())
		}
	}
	transition, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: "http://legacy.local"}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	if r := performFormRequest(transition, "/api/v1/subscriptions/rss/run", f.token, url.Values{}); r.Code != 501 || calls.Load() != 0 {
		t.Fatal(r.Code, r.Body.String())
	}
}

func TestSubscriptionRSSSiteSelectionAndOrigin(t *testing.T) {
	sites := []siteconfig.Site{{ID: 1, Name: "Tracker", Include: "D", RSSURL: "https://tracker.test/rss"}, {ID: 2, Name: "Disabled", RSSURL: "https://disabled.test/rss"}}
	for _, scope := range [][]string{nil, {"1"}, {"Tracker"}, {"1", "Tracker"}} {
		selected, err := selectedSubscriptionRSSSites(sites, scope)
		if err != nil || len(selected) != 1 {
			t.Fatal(selected, err)
		}
	}
	if _, err := selectedSubscriptionRSSSites(sites, []string{"Disabled"}); err == nil {
		t.Fatal("disabled site selected")
	}
	for _, raw := range []string{"https://other.test/path", "https://user:secret@tracker.test/path", "http://tracker.test/path", "https://tracker.test/path#frag"} {
		if subscriptionSiteOriginAllowed(sites[0], raw) {
			t.Fatal("unsafe credential scope", raw)
		}
	}
	for _, note := range []string{`{"parse":"invalid"}`, `{"rule":"bad"}`, `{"limit_seconds":"-1"}`, `{"chrome":"Y"}`, `{"limit_count":"1"}`} {
		if _, _, _, err := subscriptionRSSSiteNote(siteconfig.Site{Note: note}); err == nil {
			t.Fatal("unsupported note accepted", note)
		}
	}
}

func TestSubscriptionRSSCandidateSiteFilterAndPriority(t *testing.T) {
	low, high := int64(1), int64(10)
	first := candidateFixture(t, "Movie.2026.1080p.WEB-DL", 1, &low)
	second := candidateFixture(t, "Movie.2026.1080p.WEB-DL", 2, &high)
	first.siteOrder = 100
	second.siteOrder = 99
	selected, _, err := planSubscriptionCandidates(t.Context(), subscriptionCandidateInput{Group: -1}, nil, []identifiedSubscriptionResource{second, first})
	if err != nil || len(selected) != 1 || selected[0].resource.DownloadURL != first.resource.DownloadURL {
		t.Fatal(selected, err)
	}
	group := int64(1)
	first.siteGroup = &group
	groups := []filterconfig.GroupInfo{{Group: filterconfig.Group{ID: 1}, Rules: []filterconfig.Rule{{ID: 1, Priority: "1", Free: "1 0"}}}}
	selected, _, err = planSubscriptionCandidates(t.Context(), subscriptionCandidateInput{Group: 0}, groups, []identifiedSubscriptionResource{first})
	if err != nil || len(selected) != 0 {
		t.Fatal("unknown promotion bypassed site default filter", selected, err)
	}
	selected, _, err = planSubscriptionCandidates(t.Context(), subscriptionCandidateInput{Group: -1}, groups, []identifiedSubscriptionResource{first})
	if err != nil || len(selected) != 1 {
		t.Fatal("explicit no filter lost", selected, err)
	}
}

func TestSubscriptionRSSAuthenticatedTorrentAndRedirectProtection(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			var downloads atomic.Int32
			f := newSubscriptionFeedFixture(t, false, `{"parse":"N","ua":"native-test"}`, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/rss" {
					return jsonResponse(r, `<rss><channel>`+subscriptionFeedItem("Movie.2026.1080p.WEB-DL", "https://tracker.test/download?passkey=private-torrent", "", "")+`</channel></rss>`), nil
				}
				downloads.Add(1)
				if r.Header.Get("User-Agent") != "native-test" {
					t.Fatal("lost site user agent")
				}
				response := jsonResponse(r, string(testTorrent))
				if redirect {
					response.StatusCode = 302
					response.Header.Set("Location", "https://other.test/download")
				}
				return response, nil
			})
			base := f.transport
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "qb.local" && strings.HasSuffix(r.URL.Path, "/torrents/add") {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatal(err)
					}
					defer r.MultipartForm.RemoveAll()
					file, _, err := r.FormFile("torrents")
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					body, err := io.ReadAll(file)
					if err != nil || string(body) != string(testTorrent) || r.FormValue("tags") != "NASTOOL" {
						t.Fatal("lost torrent or preset", err)
					}
					f.adds++
					return jsonResponse(r, "Ok."), nil
				}
				return base.RoundTrip(r)
			})
			var err error
			f.handler, err = newHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, transport)
			if err != nil {
				t.Fatal(err)
			}
			r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
			want := 1
			if redirect {
				want = 0
			}
			if r.Code != 200 || f.adds != want || downloads.Load() != 1 || f.queries != 0 {
				t.Fatal(r.Code, r.Body.String(), f.adds, downloads.Load())
			}
			if redirect && !strings.Contains(r.Body.String(), `"failed":1`) {
				t.Fatal(r.Body.String())
			}
		})
	}
}

func TestSubscriptionRSSBatchFetchesSiteOnceAcrossSubscriptions(t *testing.T) {
	var calls atomic.Int32
	f := newSubscriptionFeedFixture(t, false, "", func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return jsonResponse(r, `<rss><channel/></rss>`), nil
	})
	for id := 2; id <= 7; id++ {
		if _, err := f.db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,RSS_SITES) VALUES (?,'Movie','2026','100','R',0,0,-1,'["Tracker"]')`, id); err != nil {
			t.Fatal(err)
		}
	}
	r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"subscriptions":7`) || !strings.Contains(r.Body.String(), `"failed":0`) || calls.Load() != 1 || f.adds != 0 || f.queries != 0 {
		t.Fatal(r.Code, r.Body.String(), calls.Load())
	}
}

func TestSubscriptionRSSSourceFailureDoesNotSubmitPartialFeed(t *testing.T) {
	var feeds atomic.Int32
	f := newSubscriptionFeedFixture(t, false, "", func(r *http.Request) (*http.Response, error) {
		feeds.Add(1)
		if r.URL.Query().Get("broken") == "1" {
			response := jsonResponse(r, "upstream failure")
			response.StatusCode = 500
			return response, nil
		}
		return jsonResponse(r, `<rss><channel>`+subscriptionFeedItem("Movie.2026.1080p.WEB-DL", testMagnet, "", "")+`</channel></rss>`), nil
	})
	if _, err := f.runner.download.sites.Upsert(t.Context(), siteconfig.Site{Name: "Broken", Include: "D", RSSURL: "https://tracker.test/rss?broken=1", SignURL: "https://tracker.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET RSS_SITES='[]'`); err != nil {
		t.Fatal(err)
	}
	r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"failed":1`) || f.adds != 0 || feeds.Load() != 2 {
		t.Fatal(r.Code, r.Body.String(), f.adds, feeds.Load())
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&n); err != nil || n != 0 {
		t.Fatal("partial feed claimed a resource", n, err)
	}
}

func TestSubscriptionRSSWorkerPersistsAcceptedMovie(t *testing.T) {
	f := newSubscriptionFeedFixture(t, false, "", func(r *http.Request) (*http.Response, error) {
		return jsonResponse(r, `<rss><channel>`+subscriptionFeedItem("Movie.2026.1080p.WEB-DL", testMagnet, "", "")+`</channel></rss>`), nil
	})
	if err := config.NewStore(f.path).Update(map[string]any{"pt.pt_check_interval": 301}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks, done := make(chan time.Time), make(chan struct{})
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	go func() { defer close(done); runSubscriptionSearchWorker(ctx, f.runner.search, base, ticks) }()
	ticks <- base.Add(301 * time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_MOVIES WHERE ID=1`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RSS worker failed to persist accepted submission")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	waitSubscriptionWorker(t, done)
	if f.adds != 1 || f.queries != 0 {
		t.Fatal(f.adds, f.queries)
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM RSS_HISTORY WHERE TMDBID='100'`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestSubscriptionRSSScheduleCompatibilityHotReloadAndCancellation(t *testing.T) {
	for _, test := range []struct {
		value   any
		seconds int
		bad     bool
	}{{nil, 0, false}, {"0.5", 0, false}, {1, 300, false}, {"300", 300, false}, {300.5, 300, false}, {301.5, 302, false}, {-1, 0, true}, {"bad", 0, true}, {math.Inf(1), 0, true}, {3153600001, 0, true}} {
		got, err := subscriptionFeedInterval(test.value)
		if got != time.Duration(test.seconds)*time.Second || (err != nil) != test.bad {
			t.Fatal(test, got, err)
		}
	}
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	plan := subscriptionWorkerPlan{}
	if plan.reconcileFeed(base, 300*time.Second) || plan.reconcileFeed(base.Add(299*time.Second), 300*time.Second) || !plan.reconcileFeed(base.Add(901*time.Second), 300*time.Second) || !plan.nextFeed.Equal(base.Add(1200*time.Second)) {
		t.Fatal(plan)
	}
	if plan.reconcileFeed(base.Add(902*time.Second), 600*time.Second) || !plan.nextFeed.Equal(base.Add(1502*time.Second)) || plan.reconcileFeed(base.Add(2000*time.Second), 0) || !plan.nextFeed.IsZero() {
		t.Fatal(plan)
	}
	started := make(chan struct{})
	f := newSubscriptionFeedFixture(t, false, "", func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	if err := config.NewStore(f.path).Update(map[string]any{"pt.pt_check_interval": 301}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks, done := make(chan time.Time), make(chan struct{})
	go func() { defer close(done); runSubscriptionSearchWorker(ctx, f.runner.search, base, ticks) }()
	ticks <- base.Add(301 * time.Second)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("RSS worker did not dispatch")
	}
	if r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{}); r.Code != 409 {
		t.Fatal("manual scan overlapped worker", r.Code, r.Body.String())
	}
	cancel()
	waitSubscriptionWorker(t, done)
	if f.adds != 0 || f.queries != 0 {
		t.Fatal("cancelled feed submitted", f.adds, f.queries)
	}
}
