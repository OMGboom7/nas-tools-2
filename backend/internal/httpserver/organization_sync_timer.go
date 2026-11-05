package httpserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func syncTimerCron(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	var cfg map[string]json.RawMessage
	if len(raw) > 128<<10 || json.Unmarshal([]byte(raw), &cfg) != nil || cfg == nil {
		return "", organization.ErrState
	}
	var value string
	if field, ok := cfg["cron"]; ok && json.Unmarshal(field, &value) != nil {
		return "", organization.ErrState
	}
	return strings.TrimSpace(value), nil
}

func nextSyncTimer(timer organization.MonitorTimer, now time.Time) (time.Time, error) {
	location, err := rssWorkerLocation()
	fields := strings.Fields(timer.Cron)
	if err != nil || location.String() != timer.Timezone || len(fields) != 5 {
		return time.Time{}, organization.ErrState
	}
	// APScheduler 3.10.2 parses named month and weekday ranges without a
	// step. Its prefix match ignores a trailing numeric /step on those terms.
	for _, index := range []int{3, 4} {
		terms := strings.Split(strings.ToLower(fields[index]), ",")
		for i, term := range terms {
			base, step, found := strings.Cut(term, "/")
			if found && len(base) > 0 && base[0] >= 'a' && base[0] <= 'z' && rssDigits(step) {
				terms[i] = base
			}
		}
		fields[index] = strings.Join(terms, ",")
	}
	s, err := parseRSSSchedule(strings.Join(fields, " "), location)
	if err != nil {
		return time.Time{}, organization.ErrState
	}
	next := s.next(now)
	if next.IsZero() {
		return time.Time{}, organization.ErrState
	}
	return next, nil
}

func (api organizationAPI) reviewSyncTimer(ctx context.Context) (*organization.MonitorTimer, *recognitionFailure) {
	settings, err := api.jobs.SyncTimerSettings(ctx)
	if err != nil {
		return nil, &recognitionFailure{503, "无法读取 SyncTimer 设置"}
	}
	cron, err := syncTimerCron(settings.Raw)
	if err != nil || !settings.Installed || cron == "" {
		return nil, &recognitionFailure{409, "请先安装定时目录同步插件并保存有效周期"}
	}
	location, err := rssWorkerLocation()
	if err != nil {
		return nil, &recognitionFailure{422, "无法读取同步周期时区"}
	}
	timer := &organization.MonitorTimer{Cron: cron, Timezone: location.String(), SettingsDigest: organization.Digest(settings.Raw)}
	if _, err := nextSyncTimer(*timer, time.Now()); err != nil {
		return nil, &recognitionFailure{422, "请输入有效的五段 cron 周期"}
	}
	return timer, nil
}

type syncTimerStatus struct {
	Installed bool                        `json:"installed"`
	Cron      string                      `json:"cron"`
	Active    bool                        `json:"active"`
	Next      string                      `json:"next,omitempty"`
	Runs      []organization.SyncTimerRun `json:"runs"`
}

func (api organizationAPI) syncTimerStatus(ctx context.Context, control organization.MonitorControl) syncTimerStatus {
	status := syncTimerStatus{Runs: []organization.SyncTimerRun{}}
	settings, err := api.jobs.SyncTimerSettings(ctx)
	if err != nil {
		return status
	}
	status.Installed = settings.Installed
	status.Cron, _ = syncTimerCron(settings.Raw)
	if runs, err := api.jobs.SyncTimerRuns(ctx, ""); err == nil {
		status.Runs = runs
	}
	if control.Lease.Timer == nil || !control.Enabled() || api.jobs.VerifyMonitorControl(ctx, control.Lease.Generation) != nil {
		return status
	}
	if _, err := nextSyncTimer(*control.Lease.Timer, time.Now()); err != nil {
		return status
	}
	clock, err := api.jobs.SyncTimerClock(ctx, control.Lease.Generation)
	if err == nil {
		status.Active = true
		status.Next = clock.Next
	}
	return status
}

func (api organizationAPI) syncTimerTick(ctx context.Context, control organization.MonitorControl, now time.Time) error {
	lease := control.Lease
	ctx = organization.AutomaticRemoteContext(ctx, "sync-monitor", lease.Generation)
	if err := api.jobs.VerifyMonitorControl(ctx, lease.Generation); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return api.jobs.PauseSyncMonitorGeneration(ctx, lease.Generation, "SyncTimer 安装或配置已变化，需重新授权")
	}
	next, err := nextSyncTimer(*lease.Timer, now)
	if err != nil {
		return api.jobs.PauseSyncMonitorGeneration(ctx, lease.Generation, "SyncTimer 周期或时区已变化，需重新授权")
	}
	clock, err := api.jobs.SyncTimerClock(ctx, lease.Generation)
	if err != nil {
		return err
	}
	if clock.Active == "" {
		due, err := time.Parse(time.RFC3339Nano, clock.Next)
		if err != nil {
			return organization.ErrState
		}
		if due.After(now) {
			return nil
		}
		scanStart := time.Now()
		snapshots, failure := api.monitorSnapshots(ctx, lease)
		if failure != nil {
			return api.jobs.PauseSyncMonitorGeneration(ctx, lease.Generation, failure.message)
		}
		observed := now.Add(time.Since(scanStart))
		if !next.After(observed) {
			next, err = nextSyncTimer(*lease.Timer, observed)
			if err != nil {
				return err
			}
		}
		return api.jobs.ClaimSyncTimer(ctx, lease, clock.Next, next, observed, snapshots)
	}
	run, err := api.jobs.SyncTimerRun(ctx, lease.Generation, clock.Active)
	if err != nil || run.State != "observing" {
		return organization.ErrState
	}
	started, err := time.Parse(time.RFC3339Nano, run.Started)
	if err != nil {
		return organization.ErrState
	}
	if now.Sub(started) < 30*time.Second {
		return nil
	}
	fresh, failure := api.monitorSnapshots(ctx, lease)
	if failure != nil {
		return api.jobs.PauseSyncMonitorGeneration(ctx, lease.Generation, failure.message)
	}
	original := map[string]bool{}
	for _, key := range run.Keys {
		original[key] = true
	}
	stable := map[string]bool{}
	observed := []organization.MonitorSnapshot{}
	for _, snapshot := range fresh {
		key := organization.MonitorEventKey(lease.Generation, snapshot)
		if original[key] {
			stable[key] = true
			observed = append(observed, snapshot)
		}
	}
	if err := api.jobs.ObserveSyncMonitor(ctx, lease, observed, now); err != nil {
		return err
	}
	err = api.processMonitorEvents(ctx, lease, now, stable)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state := "scanned"
	if err != nil {
		state = "needs_review"
	}
	finish := api.jobs.FinishSyncTimer(ctx, lease.Generation, run.ID, state)
	if err != nil {
		return err
	}
	return finish
}
