package httpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestSiteRequestPolicyUnitsBoundsAndPairs(t *testing.T) {
	policy, err := parseSiteRequestPolicy(map[string]any{"limit_interval": "2", "limit_count": 3, "limit_seconds": "4"})
	if err != nil || policy.interval != 2*time.Minute || policy.count != 3 || policy.gap != 4*time.Second {
		t.Fatal(policy, err)
	}
	for _, note := range []map[string]any{nil, {}, {"limit_seconds": 0}, {"limit_interval": "0", "limit_count": "0"}} {
		if policy, err := parseSiteRequestPolicy(note); err != nil || policy != (siteRequestPolicy{}) {
			t.Fatal(policy, err)
		}
	}
	for _, note := range []map[string]any{
		{"limit_interval": "1"}, {"limit_count": "1"}, {"limit_interval": "1", "limit_count": "0"},
		{"limit_seconds": "-1"}, {"limit_seconds": "1.5"}, {"limit_seconds": true}, {"limit_seconds": "NaN"},
		{"limit_seconds": "3153600001"}, {"limit_interval": "52560001", "limit_count": "1"}, {"limit_interval": "1", "limit_count": "1000000001"},
	} {
		if _, err := parseSiteRequestPolicy(note); err == nil {
			t.Fatal("invalid policy accepted", note)
		}
	}
}

func TestSiteRequestLimiterIdleWindowSpacingAndPolicyChanges(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	limiter := &siteRequestLimiter{now: func() time.Time { return now }}
	policy := siteRequestPolicy{interval: time.Minute, count: 2, gap: 10 * time.Second}
	acquire := func(want time.Duration) {
		t.Helper()
		delay, err := limiter.acquire(t.Context(), 1, policy)
		if err != nil || delay != want {
			t.Fatal(delay, want, err)
		}
	}
	acquire(0)
	acquire(10 * time.Second)
	now = now.Add(10 * time.Second)
	acquire(0)
	now = now.Add(10 * time.Second)
	acquire(50 * time.Second)
	// This is the legacy idle reset, not a fixed window from the first visit.
	now = now.Add(40 * time.Second)
	acquire(10 * time.Second)
	now = now.Add(10 * time.Second)
	acquire(0)
	policy.gap = 20 * time.Second
	acquire(20 * time.Second)
	if limiter.states[1].count != 1 {
		t.Fatal("policy edit reset history", limiter.states[1])
	}
	if delay, err := limiter.acquire(t.Context(), 2, policy); err != nil || delay != 0 {
		t.Fatal("sites share a budget", delay, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := limiter.states[1]
	if _, err := limiter.acquire(ctx, 1, policy); !errors.Is(err, context.Canceled) || limiter.states[1] != before {
		t.Fatal("cancelled request consumed budget", err)
	}
}

func TestSiteRequestLimiterConcurrentAdmissionIsAtomic(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	limiter := &siteRequestLimiter{now: func() time.Time { return now }}
	policy := siteRequestPolicy{interval: time.Minute, count: 3}
	var allowed atomic.Int32
	var group sync.WaitGroup
	for n := 0; n < 32; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			delay, err := limiter.acquire(t.Context(), 1, policy)
			if err != nil {
				t.Error(err)
				return
			}
			if delay == 0 {
				allowed.Add(1)
			} else if delay != time.Minute {
				t.Error(delay)
			}
		}()
	}
	group.Wait()
	if allowed.Load() != 3 || limiter.states[1].count != 3 {
		t.Fatal("parallel callers bypassed rate limit", allowed.Load(), limiter.states[1])
	}
}

func TestSiteRequestLimiterWaitHotReloadDeletionAndCancellation(t *testing.T) {
	for _, operation := range []string{"disable", "delete", "cancel", "invalid"} {
		t.Run(operation, func(t *testing.T) {
			store, err := siteconfig.Open(filepath.Join(t.TempDir(), "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			site, err := store.Upsert(t.Context(), siteconfig.Site{Name: "Tracker", SignURL: "https://tracker.test", Note: `{"limit_seconds":"60"}`})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
			limiter := &siteRequestLimiter{now: func() time.Time { return now }}
			if err := limiter.Wait(t.Context(), store, site.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := false
			limiter.wait = func(ctx context.Context, delay time.Duration) error {
				if called || delay > time.Second {
					t.Fatal("wait ignored reload bound", delay)
				}
				called = true
				switch operation {
				case "disable":
					site.Note = `{}`
					_, err = store.Upsert(ctx, site)
				case "delete":
					err = store.Delete(ctx, site.ID)
				case "invalid":
					site.Note = `{"limit_count":"1"}`
					_, err = store.Upsert(ctx, site)
				case "cancel":
					cancel()
					return ctx.Err()
				}
				return err
			}
			err = limiter.Wait(ctx, store, site.ID)
			if !called || (err == nil) != (operation == "disable") {
				t.Fatal(operation, called, err)
			}
			if operation == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if limiter.states[site.ID].count != 1 {
				t.Fatal("waiter pre-reserved a request", limiter.states[site.ID])
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitSiteRequestDelay(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSubscriptionRSSConfiguredLimitsShareFeedDetailAndTorrentBudget(t *testing.T) {
	var clockMu sync.Mutex
	clock := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	var visits []time.Time
	f := newSubscriptionFeedFixture(t, false, `{"parse":"Y","limit_interval":"1","limit_count":"2","limit_seconds":"2"}`, func(r *http.Request) (*http.Response, error) {
		clockMu.Lock()
		visits = append(visits, clock)
		clockMu.Unlock()
		switch r.URL.Path {
		case "/rss":
			return jsonResponse(r, `<rss><channel>`+subscriptionFeedItem("Movie.2026.1080p.WEB-DL", "https://tracker.test/download?passkey=private", "https://tracker.test/details", "")+`</channel></rss>`), nil
		case "/details":
			return jsonResponse(r, `<h1>Movie.2026.1080p.WEB-DL</h1><span class="free"/>`), nil
		case "/download":
			return jsonResponse(r, testMagnet), nil
		default:
			t.Fatal("unexpected site request")
			return nil, context.Canceled
		}
	})
	limits := f.runner.download.service.siteLimits
	limits.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return clock }
	limits.wait = func(ctx context.Context, delay time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		clockMu.Lock()
		clock = clock.Add(delay)
		clockMu.Unlock()
		return nil
	}
	r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"completed":1`) || f.adds != 1 || f.queries != 0 || len(visits) != 3 {
		t.Fatal(r.Code, r.Body.String(), visits)
	}
	if visits[1].Sub(visits[0]) != 2*time.Second || visits[2].Sub(visits[1]) != time.Minute {
		t.Fatal("site clients did not share the configured budget", visits)
	}
}

func TestSubscriptionRSSRateWaitCancellationMakesNoClaim(t *testing.T) {
	var calls atomic.Int32
	f := newSubscriptionFeedFixture(t, false, `{"parse":"Y","limit_seconds":"60"}`, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Path != "/rss" {
			t.Fatal("rate limited detail was requested")
		}
		return jsonResponse(r, `<rss><channel>`+subscriptionFeedItem("Movie.2026.1080p.WEB-DL", testMagnet, "https://tracker.test/details", "")+`</channel></rss>`), nil
	})
	started := make(chan struct{})
	f.runner.download.service.siteLimits.wait = func(ctx context.Context, delay time.Duration) error { close(started); <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, failure := f.runner.search.feeds.execute(ctx)
		if failure == nil || failure.status != 504 {
			t.Error("cancelled scan reported success", failure)
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("RSS did not wait on site limit")
	}
	cancel()
	waitSubscriptionWorker(t, done)
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&n); err != nil || n != 0 || f.adds != 0 || calls.Load() != 1 {
		t.Fatal("cancelled wait reserved or submitted", n, f.adds, calls.Load(), err)
	}
}

func TestSiteRequestLimiterBuiltinSearchSharesRSSBudget(t *testing.T) {
	clock := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	var visits []time.Time
	f := newSubscriptionFeedFixture(t, false, `{"parse":"N","limit_interval":"1","limit_count":"1"}`, func(r *http.Request) (*http.Response, error) {
		visits = append(visits, clock)
		if r.URL.Path == "/torrents.php" {
			return jsonResponse(r, `<table class="torrents"><tr><td><a class="title">Movie.2026.1080p.WEB-DL</a><a class="download" href="`+testMagnet+`">download</a><span class="size">1 GB</span></td></tr></table>`), nil
		}
		if r.URL.Path != "/rss" {
			t.Fatal(r.URL.Path)
		}
		return jsonResponse(r, `<rss><channel>`+subscriptionFeedItem("Movie.2026.1080p.WEB-DL", testMagnet, "", "")+`</channel></rss>`), nil
	})
	catalog := `{"indexer":[{"id":"native-tracker","name":"Builtin","domain":"https://tracker.test/","search":{"paths":[{"path":"torrents.php"}],"params":{"search":"{keyword}"}},"torrents":{"list":{"selector":"table.torrents > tr:has(a)"},"fields":{"title":{"selector":"a.title"},"download":{"selector":"a.download","attribute":"href"},"size":{"selector":".size"}}}}],"conf":{}}`
	catalogPath := filepath.Join(t.TempDir(), "sites.dat")
	if err := os.WriteFile(catalogPath, []byte(base64.StdEncoding.EncodeToString([]byte(catalog))), 0600); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	var err error
	f.handler, err = buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true, SiteCatalogPath: catalogPath}, f.transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.download.system.Set(t.Context(), "UserIndexerSites", `["native-tracker"]`); err != nil {
		t.Fatal(err)
	}
	limits := runner.download.service.siteLimits
	limits.now = func() time.Time { return clock }
	limits.wait = func(ctx context.Context, delay time.Duration) error { clock = clock.Add(delay); return ctx.Err() }
	search := nativeJSONRequest(f.handler, "POST", "/api/v1/search/resources", f.token, `{"keyword":"Movie","quick":true}`)
	if search.Code != 200 {
		t.Fatal(search.Code, search.Body.String())
	}
	r := performFormRequest(f.handler, "/api/v1/subscriptions/rss/run", f.token, url.Values{})
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"completed":1`) || len(visits) != 2 || visits[1].Sub(visits[0]) != time.Minute {
		t.Fatal(r.Code, r.Body.String(), visits)
	}
}

func TestSiteEditorRejectsIncompleteAndOverflowingLimits(t *testing.T) {
	base := siteUpsertRequest{Name: "Tracker", SiteURL: "https://tracker.test", Priority: 1}
	for _, test := range []siteUpsertRequest{{LimitInterval: "1"}, {LimitCount: "1"}, {LimitSeconds: "3153600001"}, {LimitSeconds: "-1"}} {
		input := base
		input.LimitInterval, input.LimitCount, input.LimitSeconds = test.LimitInterval, test.LimitCount, test.LimitSeconds
		if validateSiteInput(input, true) == "" {
			t.Fatal("bad editor limits accepted", test)
		}
	}
	base.LimitInterval, base.LimitCount, base.LimitSeconds = "1", "2", "3"
	if message := validateSiteInput(base, true); message != "" {
		t.Fatal(message)
	}
}
