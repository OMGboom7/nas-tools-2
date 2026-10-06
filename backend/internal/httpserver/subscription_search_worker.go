package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const subscriptionQueueInterval = 5 * time.Minute

type subscriptionWorkerPlan struct {
	nextQueue, nextRecurring time.Time
	interval                 time.Duration
	nextFeed                 time.Time
	feedInterval             time.Duration
}

func subscriptionFeedInterval(value any) (time.Duration, error) {
	raw := strings.TrimSpace(text(value))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 3153600000 {
		return 0, errors.New("invalid subscription RSS interval")
	}
	n = math.RoundToEven(n)
	if n == 0 {
		return 0, nil
	}
	if n < 300 {
		n = 300
	}
	return time.Duration(n) * time.Second, nil
}

func (p *subscriptionWorkerPlan) reconcileFeed(now time.Time, interval time.Duration) bool {
	if p.feedInterval != interval {
		p.feedInterval, p.nextFeed = interval, time.Time{}
		if interval > 0 {
			p.nextFeed = now.Add(interval)
		}
	}
	if p.nextFeed.IsZero() || p.nextFeed.After(now) {
		return false
	}
	p.nextFeed = now.Add(interval - now.Sub(p.nextFeed)%interval)
	return true
}

// Equivalent to Python's round for ordinary numeric config values; minimum
// six hours applies only to an enabled, nonzero rounded interval. Invalid
// configuration disables recurring searches rather than inventing a period.
func subscriptionRecurringInterval(value any) (time.Duration, error) {
	raw := strings.TrimSpace(text(value))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 876000 {
		return 0, errors.New("invalid subscription search interval")
	}
	n = math.RoundToEven(n)
	if n == 0 {
		return 0, nil
	}
	if n < 6 {
		n = 6
	}
	return time.Duration(n) * time.Hour, nil
}

// No startup catch-up. Missed periods collapse to one batch while retaining
// interval phase; edits reset only the recurring schedule, not the D queue.
func (p *subscriptionWorkerPlan) reconcile(now time.Time, interval time.Duration) []string {
	if p.nextQueue.IsZero() {
		p.nextQueue = now.Add(subscriptionQueueInterval)
	}
	if p.interval != interval {
		p.interval, p.nextRecurring = interval, time.Time{}
		if interval > 0 {
			p.nextRecurring = now.Add(interval)
		}
	}
	states := []string{}
	if !p.nextQueue.After(now) {
		states = append(states, "D")
		p.nextQueue = now.Add(subscriptionQueueInterval - now.Sub(p.nextQueue)%subscriptionQueueInterval)
	}
	if !p.nextRecurring.IsZero() && !p.nextRecurring.After(now) {
		states = append(states, "R")
		p.nextRecurring = now.Add(interval - now.Sub(p.nextRecurring)%interval)
	}
	return states
}

type scheduledSubscription struct {
	kind, state string
	id          int64
}

func startSubscriptionSearchWorker(ctx context.Context, runner *subscriptionSearchRunner) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		runSubscriptionSearchWorker(ctx, runner, time.Now(), ticker.C)
	}()
	return func() { <-done }
}

func (api *subscriptionSearchRunner) recurringInterval() (time.Duration, string, error) {
	configuration, err := api.planner.runner.recognition.service.configStore.Snapshot()
	if err != nil {
		return 0, "", err
	}
	value := objectValue(configuration["pt"])["search_rss_interval"]
	interval, err := subscriptionRecurringInterval(value)
	return interval, text(value), err
}

func (api *subscriptionRSSAPI) interval() (time.Duration, string, error) {
	configuration, err := api.runner.planner.runner.recognition.service.configStore.Snapshot()
	if err != nil {
		return 0, "", err
	}
	value := objectValue(configuration["pt"])["pt_check_interval"]
	interval, err := subscriptionFeedInterval(value)
	return interval, text(value), err
}

func runSubscriptionSearchWorker(parent context.Context, runner *subscriptionSearchRunner, initial time.Time, ticks <-chan time.Time) {
	if runner == nil || runner.planner.runner == nil || !runner.planner.runner.pureGo || runner.planner.search == nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	var batches sync.WaitGroup
	defer func() { cancel(); batches.Wait() }()
	batchSlot := make(chan struct{}, 1)
	plan := subscriptionWorkerPlan{}
	lastInvalid := ""
	invalidReported := false
	lastInvalidFeed := ""
	invalidFeedReported := false
	poll := func(now time.Time) {
		if ctx.Err() != nil {
			return
		}
		interval, raw, err := runner.recurringInterval()
		if err != nil {
			if !invalidReported || raw != lastInvalid {
				slog.Warn("native subscription recurring search configuration is invalid or unavailable")
			}
			lastInvalid, invalidReported = raw, true
		} else {
			invalidReported = false
		}
		states := plan.reconcile(now, interval)
		feedInterval := time.Duration(0)
		if runner.feeds != nil {
			var raw string
			var err error
			feedInterval, raw, err = runner.feeds.interval()
			if err != nil {
				if !invalidFeedReported || raw != lastInvalidFeed {
					slog.Warn("native subscription RSS interval is invalid or unavailable")
				}
				lastInvalidFeed, invalidFeedReported = raw, true
			} else {
				invalidFeedReported = false
			}
		}
		feedDue := plan.reconcileFeed(now, feedInterval)
		if len(states) == 0 && !feedDue {
			return
		}
		select {
		case batchSlot <- struct{}{}:
		default:
			slog.Warn("native subscription search occurrence skipped: previous batch is running")
			return
		}
		batches.Add(1)
		go func() {
			defer batches.Done()
			defer func() { <-batchSlot }()
			if len(states) > 0 {
				runner.executeSearchBatch(ctx, states, interval)
			}
			if feedDue && ctx.Err() == nil {
				current, _, err := runner.feeds.interval()
				if err != nil || current == 0 || current != feedInterval {
					return
				}
				feedCtx, stop := context.WithTimeout(ctx, 5*time.Minute)
				defer stop()
				result, failure := runner.feeds.execute(feedCtx)
				if failure != nil {
					if ctx.Err() == nil {
						slog.Warn("native subscription RSS scan failed", "status", failure.status)
					}
				} else {
					slog.Info("native subscription RSS scan finished", "subscriptions", result.Subscriptions, "submitted", result.Submitted, "completed", result.Completed, "failed", result.Failed)
				}
			}
		}()
	}
	poll(initial)
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			poll(now)
		}
	}
}

func (api *subscriptionSearchRunner) scheduledSubscriptions(ctx context.Context, states []string) ([]scheduledSubscription, error) {
	return api.selectScheduledSubscriptions(ctx, states, false)
}

func (api *subscriptionSearchRunner) selectScheduledSubscriptions(ctx context.Context, states []string, includeFuzzy bool) ([]scheduledSubscription, error) {
	db, err := api.planner.runner.recognition.service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	selected := map[string]bool{}
	for _, state := range states {
		selected[state] = true
	}
	jobs := []scheduledSubscription{}
	for _, kind := range []string{"MOV", "TV"} {
		rows, err := readSubscriptionRows(ctx, db, subscriptionTable(kind))
		if err != nil {
			return nil, err
		}
		if len(rows) > 10000 {
			return nil, errors.New("subscription batch exceeded limit")
		}
		for _, row := range rows {
			state := text(row["STATE"])
			if !selected[state] {
				continue
			}
			item := normalizeNativeSubscriptionRow(row)
			if !includeFuzzy && truthy(item["fuzzy_match"]) {
				continue
			} // Same as the legacy search worker.
			id, err := strconv.ParseInt(text(row["ID"]), 10, 64)
			if err != nil || id <= 0 {
				return nil, errors.New("invalid subscription batch identity")
			}
			jobs = append(jobs, scheduledSubscription{kind: kind, state: state, id: id})
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].kind != jobs[j].kind {
			return jobs[i].kind < jobs[j].kind
		}
		return jobs[i].id < jobs[j].id
	})
	return jobs, nil
}

func (api *subscriptionSearchRunner) executeSearchBatch(ctx context.Context, states []string, interval time.Duration) {
	readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	jobs, err := api.scheduledSubscriptions(readCtx, states)
	stop()
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("native subscription search queue could not be read")
		}
		return
	}
	var executions sync.WaitGroup
	defer executions.Wait()
	capacity := make(chan struct{}, 4)
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			return
		case capacity <- struct{}{}:
		}
		if ctx.Err() != nil {
			<-capacity
			return
		}
		executions.Add(1)
		go func(job scheduledSubscription) {
			defer executions.Done()
			defer func() { <-capacity }()
			api.executeScheduledSearch(ctx, job, interval)
		}(job)
	}
}

func (api *subscriptionSearchRunner) executeScheduledSearch(ctx context.Context, job scheduledSubscription, interval time.Duration) {
	if ctx.Err() != nil {
		return
	}
	if job.state == "R" {
		current, _, err := api.recurringInterval()
		if err != nil || current == 0 || current != interval {
			return
		}
	}
	result, failure := api.executeForState(ctx, job.kind, job.id, job.state)
	if failure != nil {
		if ctx.Err() == nil {
			slog.Warn("native subscription scheduled search failed or requires verification", "type", job.kind, "subscription_id", job.id, "status", failure.status)
		}
		return
	}
	// No title, torrent URL, path, token or upstream error text in worker logs.
	slog.Info("native subscription scheduled search finished", "type", job.kind, "subscription_id", job.id, "submitted", result.Submitted, "remaining", len(result.Remaining), "completed", result.Completed)
}
