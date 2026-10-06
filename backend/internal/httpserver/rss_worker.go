package httpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Static containers need IANA zones without host zoneinfo.

	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
)

type rssScheduledTask struct {
	raw      string
	schedule rssSchedule
	next     time.Time
}

type rssWorkerPlan struct {
	location *time.Location
	tasks    map[int64]rssScheduledTask
}

func rssTaskEnabled(task rsstaskconfig.Task) bool {
	return task.State == "Y" || task.State == "1"
}

// reconcile consumes at most one occurrence per task. No startup catch-up or
// outage backlog can repeatedly enqueue external downloader side effects.
func (p *rssWorkerPlan) reconcile(now time.Time, tasks []rsstaskconfig.Task) []int64 {
	current := make(map[int64]rssScheduledTask, len(tasks))
	due := []int64{}
	for _, task := range tasks {
		raw := strings.TrimSpace(task.Interval)
		if !rssTaskEnabled(task) || raw == "" {
			continue
		}
		entry, exists := p.tasks[task.ID]
		// Unsupported task types stay visible as one sanitized warning, not
		// as a successfully executed or silently substituted download task.
		key := task.Uses + ":" + raw
		if !exists || entry.raw != key {
			entry = rssScheduledTask{raw: key}
			if task.Uses != "D" {
				slog.Warn("native RSS scheduled task type is not migrated", "task_id", task.ID)
			} else {
				schedule, err := parseRSSSchedule(raw, p.location)
				if err == nil {
					entry.schedule, entry.next = schedule, schedule.next(now)
				}
				if err != nil || entry.next.IsZero() {
					slog.Warn("native RSS schedule is invalid or unsupported", "task_id", task.ID)
				}
			}
		}
		if !entry.next.IsZero() && !entry.next.After(now) {
			due = append(due, task.ID)
			if entry.schedule.interval > 0 {
				// Keep the initial interval phase after a slow or skipped run.
				elapsed := now.Sub(entry.next)
				entry.next = now.Add(entry.schedule.interval - elapsed%entry.schedule.interval)
			} else {
				entry.next = entry.schedule.next(now)
			}
		}
		current[task.ID] = entry
	}
	p.tasks = current
	return due
}

func startRSSWorker(ctx context.Context, runner *rssRunAPI, location *time.Location) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		runRSSWorker(ctx, runner, location, time.Now(), ticker.C)
	}()
	return func() { <-done }
}

// The injected clock stream lets tests exercise production dispatch and
// cancellation without real minute-long sleeps or network listeners.
func runRSSWorker(ctx context.Context, runner *rssRunAPI, location *time.Location, initial time.Time, ticks <-chan time.Time) {
	var executions sync.WaitGroup
	defer executions.Wait()
	capacity := make(chan struct{}, 4)
	plan := rssWorkerPlan{location: location}
	poll := func(now time.Time) {
		if ctx.Err() != nil {
			return
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		tasks, err := runner.preview.tasks.List(readCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("native RSS schedule configuration could not be read")
			}
			return
		}
		for _, id := range plan.reconcile(now, tasks) {
			select {
			case capacity <- struct{}{}:
			default:
				slog.Warn("native RSS scheduled occurrence skipped: worker is busy", "task_id", id)
				continue
			}
			raw := plan.tasks[id].raw
			executions.Add(1)
			go func(id int64, raw string) {
				defer executions.Done()
				defer func() { <-capacity }()
				runner.executeScheduled(ctx, id, raw)
			}(id, raw)
		}
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

func (api *rssRunAPI) executeScheduled(ctx context.Context, id int64, raw string) {
	// Recheck immediately before execution: disable/delete/edit takes effect
	// even if the task was due in the last snapshot.
	task, err := api.preview.tasks.Get(ctx, id)
	if err != nil || ctx.Err() != nil || !rssTaskEnabled(task) || task.Uses != "D" || task.Uses+":"+strings.TrimSpace(task.Interval) != raw {
		return
	}
	result, failure := api.execute(ctx, id)
	if failure != nil {
		if ctx.Err() == nil {
			// IDs and status only: no feed URL, credentials or raw upstream errors.
			slog.Warn("native RSS scheduled execution failed", "task_id", id, "status", failure.status)
		}
		return
	}
	slog.Info("native RSS scheduled execution finished", "task_id", id, "total", result.Total, "downloaded", result.Downloaded, "skipped", result.Skipped, "uncertain", result.Uncertain)
}

func rssWorkerLocation() (*time.Location, error) {
	zone := strings.TrimSpace(os.Getenv("TZ"))
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	return time.LoadLocation(zone)
}
