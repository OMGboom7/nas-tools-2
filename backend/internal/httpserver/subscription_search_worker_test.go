package httpserver

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestSubscriptionRecurringIntervalCompatibilityAndBounds(t *testing.T) {
	for _, test := range []struct {
		value   any
		hours   int
		invalid bool
	}{
		{nil, 0, false}, {"", 0, false}, {0, 0, false}, {"0", 0, false}, {"0.5", 0, false},
		{"1", 6, false}, {1.5, 6, false}, {6.5, 6, false}, {7.5, 8, false}, {"24", 24, false},
		{"bad", 0, true}, {-1, 0, true}, {true, 0, true}, {math.Inf(1), 0, true}, {math.NaN(), 0, true}, {876001, 0, true},
	} {
		got, err := subscriptionRecurringInterval(test.value)
		if (err != nil) != test.invalid || got != time.Duration(test.hours)*time.Hour {
			t.Errorf("value=%v interval=%v err=%v", test.value, got, err)
		}
	}
}

func TestSubscriptionWorkerPlanNoCatchupPhaseAndHotReload(t *testing.T) {
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	p := subscriptionWorkerPlan{}
	if states := p.reconcile(base, 6*time.Hour); len(states) != 0 {
		t.Fatal("startup catchup", states)
	}
	if states := p.reconcile(base.Add(4*time.Minute), 6*time.Hour); len(states) != 0 {
		t.Fatal(states)
	}
	if states := p.reconcile(base.Add(5*time.Minute), 6*time.Hour); !reflect.DeepEqual(states, []string{"D"}) {
		t.Fatal(states)
	}
	if states := p.reconcile(base.Add(19*time.Hour+time.Minute), 6*time.Hour); !reflect.DeepEqual(states, []string{"D", "R"}) {
		t.Fatal(states)
	}
	if !p.nextRecurring.Equal(base.Add(24*time.Hour)) || !p.nextQueue.Equal(base.Add(19*time.Hour+5*time.Minute)) {
		t.Fatal("phase drift", p)
	}
	if states := p.reconcile(base.Add(19*time.Hour+2*time.Minute), 12*time.Hour); len(states) != 0 {
		t.Fatal("config edit triggered execution", states)
	}
	if !p.nextRecurring.Equal(base.Add(31*time.Hour + 2*time.Minute)) {
		t.Fatal(p.nextRecurring)
	}
	p.reconcile(base.Add(19*time.Hour+3*time.Minute), 0)
	if !p.nextRecurring.IsZero() {
		t.Fatal("disabled recurring schedule remained", p)
	}
	if states := p.reconcile(base.Add(19*time.Hour+5*time.Minute), 0); !reflect.DeepEqual(states, []string{"D"}) {
		t.Fatal("disabling recurring disabled D queue", states)
	}
}

func waitSubscriptionWorker(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription worker did not cancel/join")
	}
}

func TestSubscriptionWorkerDispatchSharesManualGateAndPersists(t *testing.T) {
	for _, state := range []string{"D", "R"} {
		t.Run(state, func(t *testing.T) {
			f := newSubscriptionRunFixture(t)
			if _, err := f.db.Exec(`DELETE FROM RSS_TVS; UPDATE RSS_MOVIES SET STATE=? WHERE ID=1`, state); err != nil {
				t.Fatal(err)
			}
			if err := config.NewStore(f.path).Update(map[string]any{"pt.search_rss_interval": 6}); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "indexer.local" && r.URL.Path != "/api/v1/indexerstats" {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return f.transport.RoundTrip(r)
			})
			var runner *rssRunAPI
			handler, err := buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, transport, &runner)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			ticks, done := make(chan time.Time), make(chan struct{})
			base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
			go func() { defer close(done); runSubscriptionSearchWorker(ctx, runner.search, base, ticks) }()
			due := subscriptionQueueInterval
			if state == "R" {
				due = 6 * time.Hour
			}
			ticks <- base.Add(due)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("scheduled subscription did not start")
			}
			manual := performFormRequest(handler, "/api/v1/subscriptions/MOV/1/refresh", f.token, url.Values{})
			if manual.Code != 409 {
				t.Fatal("manual overlapped scheduled execution", manual.Code, manual.Body.String())
			}
			close(release)
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
					t.Fatal("scheduled subscription did not persist")
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			waitSubscriptionWorker(t, done)
			if f.adds != 1 || f.queries != 1 {
				t.Fatal(f.adds, f.queries)
			}
		})
	}
}

func TestSubscriptionWorkerShutdownAndClosedClockCancelIndexer(t *testing.T) {
	for _, closeClock := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "closed-clock"}[closeClock], func(t *testing.T) {
			f := newSubscriptionRunFixture(t)
			if _, err := f.db.Exec(`DELETE FROM RSS_TVS`); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "indexer.local" && r.URL.Path != "/api/v1/indexerstats" {
					close(started)
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return f.transport.RoundTrip(r)
			})
			var runner *rssRunAPI
			if _, err := buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, transport, &runner); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ticks, done := make(chan time.Time), make(chan struct{})
			base := time.Now()
			go func() { defer close(done); runSubscriptionSearchWorker(ctx, runner.search, base, ticks) }()
			ticks <- base.Add(subscriptionQueueInterval)
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("indexer did not start")
			}
			if closeClock {
				close(ticks)
			} else {
				cancel()
			}
			waitSubscriptionWorker(t, done)
			if f.adds != 0 {
				t.Fatal("cancelled search submitted a download", f.adds)
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_RSS_DOWNLOAD_CLAIMS`).Scan(&n); err != nil || n != 0 {
				t.Fatal(n, err)
			}
		})
	}
}

func TestSubscriptionScheduledSearchRechecksStateConfigAndPending(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET STATE='R' WHERE ID=1;DELETE FROM RSS_TVS`); err != nil {
		t.Fatal(err)
	}
	api := f.runner.search
	api.executeScheduledSearch(t.Context(), scheduledSubscription{kind: "MOV", id: 1, state: "D"}, 0)
	for _, value := range []any{0, 12, "invalid"} {
		if err := config.NewStore(f.path).Update(map[string]any{"pt.search_rss_interval": value}); err != nil {
			t.Fatal(err)
		}
		api.executeScheduledSearch(t.Context(), scheduledSubscription{kind: "MOV", id: 1, state: "R"}, 6*time.Hour)
	}
	if f.adds != 0 || f.queries != 0 {
		t.Fatal("stale scheduled search fetched resources", f.adds, f.queries)
	}
	if _, err := f.db.Exec(`UPDATE RSS_MOVIES SET STATE='D' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	f.addError = true
	if status, _ := f.run("MOV", "1"); status != 502 {
		t.Fatal(status)
	}
	f.addError = false
	before := f.queries
	api.executeSearchBatch(t.Context(), []string{"D"}, 0)
	if f.adds != 1 || f.queries != before {
		t.Fatal("uncertain subscription retried indexer/submission", f.adds, f.queries)
	}
	if _, err := f.db.Exec(`DELETE FROM RSS_MOVIES WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	api.executeScheduledSearch(t.Context(), scheduledSubscription{kind: "MOV", id: 1, state: "D"}, 0)
	if f.queries != before {
		t.Fatal("deleted subscription fetched resources")
	}
}

func TestSubscriptionWorkerCapacityDoesNotDropLaterJobs(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	if _, err := f.db.Exec(`DELETE FROM RSS_TVS; INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES) SELECT ID+2,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES FROM RSS_MOVIES WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{4, 5, 6, 7, 8} {
		if _, err := f.db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES) SELECT ?,NAME,YEAR,TMDBID,STATE,FUZZY_MATCH,OVER_EDITION,FILTER_RULE,SEARCH_SITES FROM RSS_MOVIES WHERE ID=1`, id); err != nil {
			t.Fatal(err)
		}
	}
	started, release := make(chan struct{}, 10), make(chan struct{})
	var active, peak, calls atomic.Int64
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "indexer.local" && r.URL.Path != "/api/v1/indexerstats" {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			calls.Add(1)
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return jsonResponse(r, `[]`), nil
		}
		return f.transport.RoundTrip(r)
	})
	var runner *rssRunAPI
	if _, err := buildHandler(config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, transport, &runner); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); runner.search.executeSearchBatch(ctx, []string{"D"}, 0) }()
	for range 4 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("four workers did not start")
		}
	}
	if active.Load() != 4 {
		t.Fatal("unexpected concurrency", active.Load())
	}
	close(release)
	waitSubscriptionWorker(t, done)
	if calls.Load() != 7 || peak.Load() > 4 {
		t.Fatal("capacity dropped or over-dispatched later jobs", calls.Load(), peak.Load())
	}
}

func TestSubscriptionProductionWorkerLifecycleAndEligibility(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	if _, err := f.db.Exec(`INSERT INTO RSS_MOVIES(ID,NAME,STATE,FUZZY_MATCH) VALUES (3,'Fuzzy','D',1),(4,'Running','S',0),(5,'Finished','F',0)`); err != nil {
		t.Fatal(err)
	}
	jobs, err := f.runner.search.scheduledSubscriptions(t.Context(), []string{"D", "R"})
	if err != nil || len(jobs) != 2 || jobs[0].id != 1 || jobs[1].id != 2 {
		t.Fatal(jobs, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, wait, err := newRuntimeHandler(ctx, config.Config{ApplicationConfigPath: f.path, DisableLegacy: true}, f.transport)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	waitSubscriptionWorker(t, done)
	if f.adds != 0 || f.queries != 0 {
		t.Fatal("production startup replayed downloads", f.adds, f.queries)
	}
}

func TestSubscriptionWriteTransactionsReserveWriterBeforeSnapshot(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	service := f.runner.recognition.service
	first, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	tx, err := first.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), `UPDATE RSS_MOVIES SET NAME='Committed writer' WHERE ID=1`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		db, err := service.openNativeSubscriptionWriteDatabase()
		if err != nil {
			done <- err
			return
		}
		defer db.Close()
		second, err := db.BeginTx(ctx, nil)
		if err != nil {
			done <- err
			return
		}
		defer second.Rollback()
		var name string
		if err := second.QueryRowContext(ctx, `SELECT NAME FROM RSS_MOVIES WHERE ID=1`).Scan(&name); err != nil {
			done <- err
			return
		}
		if name != "Committed writer" {
			done <- errSubscriptionSubmissionConflict
			return
		}
		done <- nil
	}()
	<-started
	select {
	case err := <-done:
		t.Fatal("write transaction read before reserving writer", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("writer did not resume", ctx.Err())
	}
}
