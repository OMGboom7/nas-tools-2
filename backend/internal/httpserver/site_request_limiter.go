package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type siteRequestPolicy struct {
	interval, gap time.Duration
	count         int64
}

func parseSiteRequestPolicy(note map[string]any) (siteRequestPolicy, error) {
	values := map[string]int64{}
	for _, key := range []string{"limit_interval", "limit_count", "limit_seconds"} {
		raw := strings.TrimSpace(text(note[key]))
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return siteRequestPolicy{}, errors.New("invalid site request limit")
		}
		values[key] = n
	}
	// Bound durations before multiplying into nanoseconds. Window units are
	// minutes in the saved editor; spacing units are seconds.
	if values["limit_interval"] > 52560000 || values["limit_seconds"] > 3153600000 || values["limit_count"] > 1000000000 {
		return siteRequestPolicy{}, errors.New("site request limit exceeded bounds")
	}
	if (values["limit_interval"] > 0) != (values["limit_count"] > 0) {
		return siteRequestPolicy{}, errors.New("site request window and count must be configured together")
	}
	return siteRequestPolicy{interval: time.Duration(values["limit_interval"]) * time.Minute, gap: time.Duration(values["limit_seconds"]) * time.Second, count: values["limit_count"]}, nil
}

type siteRequestState struct {
	last  time.Time
	count int64
}

// Shared by the handler's native site clients. Reservations are atomic; failed
// requests still count, but cancelled waiters do not reserve a future slot.
// Like the old Sites limiter, the counter resets after an idle interval.
type siteRequestLimiter struct {
	mu     sync.Mutex
	states map[int64]siteRequestState
	now    func() time.Time
	wait   func(context.Context, time.Duration) error
}

func (limiter *siteRequestLimiter) acquire(ctx context.Context, id int64, policy siteRequestPolicy) (time.Duration, error) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if policy.interval == 0 && policy.gap == 0 {
		return 0, nil
	}
	if id <= 0 {
		return 0, errors.New("site request identity is unavailable")
	}
	if limiter.states == nil {
		limiter.states = map[int64]siteRequestState{}
	}
	state, exists := limiter.states[id]
	if !exists && len(limiter.states) >= 10000 {
		return 0, errors.New("site request limiter capacity exceeded")
	}
	now := time.Now()
	if limiter.now != nil {
		now = limiter.now()
	}
	if !state.last.IsZero() && policy.interval > 0 && now.Sub(state.last) >= policy.interval {
		state.count = 0
	}
	delay := time.Duration(0)
	if !state.last.IsZero() {
		if remaining := state.last.Add(policy.gap).Sub(now); remaining > delay {
			delay = remaining
		}
		if policy.interval > 0 && state.count >= policy.count {
			if remaining := state.last.Add(policy.interval).Sub(now); remaining > delay {
				delay = remaining
			}
		}
	}
	if delay > 0 {
		return delay, nil
	}
	state.last = now
	// Keep admission history across policy edits instead of resetting and
	// allowing concurrent requests to bypass newly tightened limits.
	if state.count < 1000000000 {
		state.count++
	}
	limiter.states[id] = state
	return 0, nil
}

func waitSiteRequestDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (limiter *siteRequestLimiter) Wait(ctx context.Context, store *siteconfig.Store, id int64) error {
	if limiter == nil {
		return nil
	}
	if store == nil {
		return errors.New("site request configuration is unavailable")
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		site, err := store.Get(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("site request configuration is unavailable")
		}
		note := map[string]any{}
		if len(site.Note) > 64<<10 || site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
			return errors.New("invalid site request configuration")
		}
		policy, err := parseSiteRequestPolicy(note)
		if err != nil {
			return err
		}
		delay, err := limiter.acquire(ctx, id, policy)
		if err != nil || delay == 0 {
			return err
		}
		// Re-read saved settings at most one second after an edit. Never sleep
		// holding the lock or pre-reserve work which may be cancelled.
		if delay > time.Second {
			delay = time.Second
		}
		wait := waitSiteRequestDelay
		if limiter.wait != nil {
			wait = limiter.wait
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}
