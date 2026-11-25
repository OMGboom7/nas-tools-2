package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type doubanDownloadFixture struct {
	*subscriptionRunFixture
	kind      string
	localized string
	indexer   func(*http.Request) (*http.Response, error)
	api       *doubanSyncAPI
}

func (f *doubanDownloadFixture) restart(t *testing.T) {
	t.Helper()
	var runner *rssRunAPI
	h, err := buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "www.douban.com":
			return jsonResponse(r, `<rss><channel><item><title>想看测试</title><link>https://movie.douban.com/subject/333/</link><pubDate>Wed, 25 Nov 2026 11:00:00 GMT</pubDate></item></channel></rss>`), nil
		case "frodo.douban.com":
			if r.URL.Path != "/api/v2/"+f.kind+"/333" {
				return jsonResponse(r, "{}"), nil
			}
			title := map[string]string{"movie": "Movie", "tv": "Show"}[f.kind]
			localized := f.localized
			if localized == "" {
				localized = title
			}
			return jsonResponse(r, fmt.Sprintf(`{"id":"333","title":%q,"original_title":%q,"year":"2026"}`, localized, title)), nil
		}
		if f.indexer != nil && r.URL.Hostname() == "indexer.local" && r.URL.Path == "/api/v1/search" {
			return f.indexer(r)
		}
		return f.transport.RoundTrip(r)
	}), &runner)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	f.runner = runner
	f.api = runner.siteSignin.douban
	f.api.pause = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
}

func (f *doubanDownloadFixture) enable(t *testing.T) {
	t.Helper()
	for _, v := range []struct {
		action string
		form   url.Values
	}{
		{"install", url.Values{"id": {"DoubanSync"}}},
		{"config", url.Values{"id": {"DoubanSync"}, "config": {`{"users":"fixture","types":"wish","sync_type":1,"rss_interval":300,"enable":true,"onlyonce":true,"auto_search":true,"auto_rss":false}`}}},
	} {
		w := performFormRequest(f.handler, "/api/v1/plugin/"+v.action, f.token, v.form)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func (f *doubanDownloadFixture) control(t *testing.T) doubanControl {
	t.Helper()
	raw, err := f.runner.download.system.Get(t.Context(), doubanControlKey)
	if err != nil {
		t.Fatal(err)
	}
	c, err := doubanState(raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func (f *doubanDownloadFixture) runDirect(t *testing.T) string {
	t.Helper()
	c := f.control(t)
	at := c.Once
	if at.IsZero() {
		at = c.Next
	}
	if at.IsZero() {
		t.Fatal(c)
	}
	if err := f.api.tick(t.Context(), at); err != nil {
		t.Fatal(err)
	}
	return f.control(t).Last
}
func (f *doubanDownloadFixture) check(t *testing.T, state string, receipts int) {
	t.Helper()
	s, err := f.api.historyState(t.Context(), "333")
	if err != nil || s != state {
		t.Fatal("history", s, state, err, f.control(t))
	}
	var n int
	if err = f.db.QueryRow("SELECT COUNT(*) FROM RSS_MOVIES").Scan(&n); err != nil || n != 1 {
		t.Fatal("movie subscriptions", n, err)
	}
	if err = f.db.QueryRow("SELECT COUNT(*) FROM RSS_TVS WHERE STATE='D' AND LACK=2").Scan(&n); err != nil || n != 1 {
		t.Fatal("changed existing series subscription", n, err)
	}
	if receipts > 0 {
		if err = f.db.QueryRow("SELECT COUNT(*) FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS WHERE json_extract(DOCUMENT,'$.direct.Kind')=?", f.kind).Scan(&n); err != nil || n != receipts {
			t.Fatal("receipts", n, receipts, err)
		}
	}
}

func TestDoubanDirectMovieAcknowledgedOrUncertainAcrossRestart(t *testing.T) {
	for _, mode := range []string{"success", "unknown", "inventory", "scope-change"} {
		t.Run(mode, func(t *testing.T) {
			f := &doubanDownloadFixture{subscriptionRunFixture: newSubscriptionRunFixture(t), kind: "movie"}
			f.restart(t)
			f.enable(t)
			f.addError = mode == "unknown"
			f.libraryComplete = mode == "inventory"
			if mode == "scope-change" {
				f.afterSearch = func() {
					if err := f.runner.download.system.Set(t.Context(), "UserIndexerSites", "[]"); err != nil {
						t.Fatal(err)
					}
				}
			}
			last := f.runDirect(t)
			switch mode {
			case "inventory":
				f.check(t, "DOWNLOADED", 0)
				if f.queries != 0 || f.adds != 0 {
					t.Fatal(f.queries, f.adds)
				}
			case "scope-change":
				f.check(t, "", 0)
				if f.adds != 0 {
					t.Fatal(f.adds)
				}
			default:
				want := "DOWNLOADED"
				if f.addError {
					want = ""
					if !strings.Contains(last, "待核对 1") {
						t.Fatal(last)
					}
				}
				f.check(t, want, 1)
				queries := f.queries
				f.restart(t)
				f.runDirect(t)
				f.check(t, want, 1)
				if f.adds != 1 || f.queries != queries {
					t.Fatal("retried", f.adds, f.queries)
				}
			}
		})
	}
}

func TestDoubanDirectOnlyRemainingEpisodesAreSearchedAgain(t *testing.T) {
	f := &doubanDownloadFixture{subscriptionRunFixture: newSubscriptionRunFixture(t), kind: "tv"}
	more := false
	f.indexer = func(r *http.Request) (*http.Response, error) {
		episodes := []int{1}
		if more {
			episodes = []int{1, 2, 3}
		}
		rows := []map[string]any{}
		for _, n := range episodes {
			rows = append(rows, map[string]any{"indexerId": 42, "title": fmt.Sprintf("Show.S01E%02d.1080p", n), "size": 1024, "downloadUrl": fmt.Sprintf("magnet:?xt=urn:btih:%040x", n)})
		}
		b, _ := json.Marshal(rows)
		return jsonResponse(r, string(b)), nil
	}
	f.restart(t)
	f.enable(t)
	last := f.runDirect(t)
	if !strings.Contains(last, "未下齐 1") || f.adds != 1 {
		t.Fatal(last, f.adds)
	}
	f.check(t, "", 1)
	f.restart(t)
	more = true
	f.runDirect(t)
	f.check(t, "DOWNLOADED", 3)
	if f.adds != 3 {
		t.Fatal("repeated episode", f.adds)
	}
}

func TestDoubanDirectRejectedAttemptNotifiesOnceAndCanRetry(t *testing.T) {
	f := &doubanDownloadFixture{subscriptionRunFixture: newSubscriptionRunFixture(t), kind: "movie"}
	f.restart(t)
	f.enable(t)
	f.loginFailure = true
	f.runDirect(t)
	f.check(t, "", 1)
	var rejected int
	if err := f.db.QueryRow("SELECT count(*) FROM GO_BUSINESS_EVENTS WHERE KIND='download_fail'").Scan(&rejected); err != nil || rejected != 1 || f.adds != 0 {
		t.Fatal("failed download event", rejected, f.adds, err)
	}
	f.loginFailure = false
	f.restart(t)
	f.runDirect(t)
	f.check(t, "DOWNLOADED", 2)
	if err := f.db.QueryRow("SELECT count(*) FROM GO_BUSINESS_EVENTS WHERE KIND='download_fail'").Scan(&rejected); err != nil || rejected != 1 || f.adds != 1 {
		t.Fatal("retry", rejected, f.adds, err)
	}
}

func TestDoubanDirectExplicitEpisodeChain(t *testing.T) {
	f := &doubanDownloadFixture{subscriptionRunFixture: newSubscriptionRunFixture(t), kind: "tv", localized: "Show S01E01E03"}
	f.indexer = func(r *http.Request) (*http.Response, error) {
		var rows []map[string]any
		for n := 1; n <= 3; n++ {
			rows = append(rows, map[string]any{"indexerId": 42, "title": fmt.Sprintf("Show.S01E%02d.1080p", n), "size": 1024, "downloadUrl": fmt.Sprintf("magnet:?xt=urn:btih:%040x", n)})
		}
		b, _ := json.Marshal(rows)
		return jsonResponse(r, string(b)), nil
	}
	f.restart(t)
	f.enable(t)
	f.runDirect(t)
	f.check(t, "DOWNLOADED", 2)
	units, err := f.runner.download.downloaders.DirectCoverage(t.Context(), "tv", "200", 1)
	if err != nil || len(units) != 2 || !units[1] || !units[3] || f.adds != 2 {
		t.Fatal(units, f.adds, err)
	}
}

func TestDoubanDirectTriesSecondTitleAndKeepsPublicProviderZeroSeeders(t *testing.T) {
	f := &doubanDownloadFixture{subscriptionRunFixture: newSubscriptionRunFixture(t), kind: "movie"}
	upstream := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "tmdb.test" && r.URL.Path == "/3/movie/100" && r.URL.Query().Get("language") == "en" {
			return jsonResponse(r, `{"id":100,"title":"Alternate","release_date":"2026-01-01"}`), nil
		}
		return upstream.RoundTrip(r)
	})
	var titles []string
	f.indexer = func(r *http.Request) (*http.Response, error) {
		title := r.URL.Query().Get("query")
		titles = append(titles, title)
		if title != "Alternate" {
			return jsonResponse(r, "[]"), nil
		}
		return jsonResponse(r, fmt.Sprintf(`[{"indexerId":42,"title":"Movie.2026.1080p","size":1024,"seeders":0,"downloadUrl":%q}]`, testMagnet)), nil
	}
	f.restart(t)
	f.enable(t)
	f.runDirect(t)
	f.check(t, "DOWNLOADED", 1)
	if !slices.Equal(titles, []string{"Movie", "Alternate"}) || f.adds != 1 {
		t.Fatal(titles, f.adds)
	}
}

func TestDoubanDirectPacksUseVerifiedFileSelectionAndOriginalRecovery(t *testing.T) {
	for _, kind := range []string{"qb5", "tr-new", "lost-start"} {
		t.Run(kind, func(t *testing.T) {
			client := kind
			if client == "lost-start" {
				client = "qb5"
			}
			w := newSubscriptionFilesFixture(t, client)
			w.expectedPaths = []string{"Show.S01/Show.S01E01.mkv", "Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E03.mkv", "Show.S01/Show.S01E02.zh.srt"}
			f := &doubanDownloadFixture{subscriptionRunFixture: w.subscriptionRunFixture, kind: "tv"}
			f.restart(t)
			f.enable(t)
			if kind == "lost-start" {
				w.loss = "start"
			}
			last := f.runDirect(t)
			if kind == "lost-start" {
				f.check(t, "", 1)
				f.restart(t)
				f.runDirect(t)
				r := w.review(t)
				if r.Next != "acknowledge" {
					t.Fatal(last, r.Next)
				}
				w.recover(t, r, true)
				r = w.review(t)
				if r.Next != "record" {
					t.Fatal(r.Next)
				}
				w.recover(t, r, true)
				f.runDirect(t)
			}
			f.check(t, "DOWNLOADED", 1)
			if w.adds != 1 || w.starts != 1 || !w.record(t).Committed {
				t.Fatal(last, w.operations, w.record(t))
			}
		})
	}
}

func TestDoubanDirectCrossSeasonPackAccountsForAllSelectedSeasons(t *testing.T) {
	w := newSeasonFixture(t, "qb5")
	w.expectedPaths = append([]string{"Show.S01-S02/S01/Show.S01E01.mkv"}, w.expectedPaths...)
	f := &doubanDownloadFixture{subscriptionRunFixture: w.subscriptionRunFixture, kind: "tv"}
	f.restart(t)
	f.enable(t)
	last := f.runDirect(t)
	if state, err := f.api.historyState(t.Context(), "333"); err != nil || state != "DOWNLOADED" || w.adds != 1 || w.starts != 1 {
		t.Fatal(state, err, w.operations, last)
	}
	for season, n := range map[int]int{1: 3, 2: 2} {
		units, err := f.runner.download.downloaders.DirectCoverage(t.Context(), "tv", "200", season)
		if err != nil || len(units) != n {
			t.Fatal(units, n, err)
		}
	}
	if !w.record(t).Committed {
		t.Fatal("missing source receipt")
	}
}
