package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestRSSWorkerDispatchPersistsAndSharesManualGate(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var feeds, adds atomic.Int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Hostname() {
		case "feed.local":
			feeds.Add(1)
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return jsonResponse(r, rssRunFeed("Movie.2026.1080p", 22, "movie")), nil
		case "client.local":
			if strings.HasSuffix(r.URL.Path, "/auth/login") {
				return jsonResponse(r, "Ok."), nil
			}
			adds.Add(1)
			return jsonResponse(r, "Ok."), nil
		default:
			t.Errorf("unexpected external/Python request")
			return nil, context.Canceled
		}
	})
	_, _, db, path := rssRunFixture(t, transport)
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET STATE='Y', INTERVAL='1' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	handler, err := buildHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport, &runner)
	if err != nil {
		t.Fatal(err)
	}
	token := loginForTest(t, handler, "admin", "password")
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); runRSSWorker(ctx, runner, time.UTC, base, ticks) }()
	ticks <- base.Add(time.Minute)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled task did not start")
	}
	manual := performFormRequest(handler, "/api/v1/rss/run", token, url.Values{"id": {"7"}})
	if manual.Code != 409 {
		t.Fatal("manual execution overlapped scheduled execution", manual.Code)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	var count string
	for {
		if err := db.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled submission did not persist")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The actual execution's WaitGroup is joined by worker shutdown.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop/join")
	}
	runner.executeScheduled(context.Background(), 7, "D:1")
	if err := db.QueryRow(`SELECT PROCESS_COUNT FROM CONFIG_USER_RSS WHERE ID=7`).Scan(&count); err != nil || count != "1" || adds.Load() != 1 {
		t.Fatal(count, adds.Load(), err)
	}
	runner.executeScheduled(context.Background(), 7, "D:1")
	if adds.Load() != 1 {
		t.Fatal("scheduled duplicate submitted", adds.Load())
	}
	before := feeds.Load()
	for _, statement := range []string{`UPDATE CONFIG_USER_RSS SET STATE='N' WHERE ID=7`, `UPDATE CONFIG_USER_RSS SET STATE='Y',INTERVAL='2' WHERE ID=7`, `DELETE FROM CONFIG_USER_RSS WHERE ID=7`} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		runner.executeScheduled(context.Background(), 7, "D:1")
	}
	if feeds.Load() != before {
		t.Fatal("disabled/changed/deleted task fetched a feed")
	}
}

func TestRSSWorkerShutdownCancelsActiveFeed(t *testing.T) {
	started := make(chan struct{})
	var adds atomic.Int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "feed.local" {
			adds.Add(1)
			return nil, context.Canceled
		}
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	_, _, db, path := rssRunFixture(t, transport)
	if _, err := db.Exec(`UPDATE CONFIG_USER_RSS SET STATE='Y', INTERVAL='1' WHERE ID=7`); err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	if _, err := buildHandler(config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport, &runner); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	base := time.Now()
	go func() { defer close(done); runRSSWorker(ctx, runner, time.UTC, base, ticks) }()
	ticks <- base.Add(time.Minute)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled feed did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel active feed")
	}
	var claims int
	if err := db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&claims); err != nil || claims != 0 || adds.Load() != 0 {
		t.Fatal("cancelled feed produced submission/claim", claims, adds.Load(), err)
	}
}

func TestRSSRuntimeWorkersOnlyStartInPureGoMode(t *testing.T) {
	t.Setenv("TZ", "invalid/private/timezone")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("no outgoing calls expected")
		return nil, context.Canceled
	})
	_, _, _, path := rssRunFixture(t, transport)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, _, err := newRuntimeHandler(ctx, config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("invalid timezone not rejected safely", err)
	}
	_, wait, err := newRuntimeHandler(ctx, config.Config{ApplicationConfigPath: path, LegacyBackendURL: "http://legacy.local"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	wait() // transition mode has no worker, even before cancellation
	t.Setenv("TZ", "Asia/Shanghai")
	_, wait, err = newRuntimeHandler(ctx, config.Config{ApplicationConfigPath: path, DisableLegacy: true}, transport)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	wait()
}
