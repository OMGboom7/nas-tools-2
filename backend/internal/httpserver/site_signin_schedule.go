package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

type signinSchedule struct {
	cron       *rssSchedule
	interval   time.Duration
	start, end int
	window     bool
	location   *time.Location
}

func parseSigninSchedule(raw string, location *time.Location) (signinSchedule, error) {
	s := signinSchedule{location: location}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return s, nil
	}
	if len(strings.Fields(raw)) == 5 {
		cron, e := parseRSSSchedule(raw, location)
		if e != nil {
			return s, e
		}
		s.cron = &cron
		return s, nil
	}
	if strings.Contains(raw, "-") {
		a, b, e := sitesignin.Window(raw)
		if e != nil {
			return s, e
		}
		s.start, s.end, s.window = a, b, true
		return s, nil
	}
	if strings.Contains(raw, ":") {
		parts := strings.Split(raw, ":")
		if len(parts) != 2 {
			return s, sitesignin.ErrState
		}
		h, e := strconv.Atoi(parts[0])
		if e != nil || h < 0 || h > 23 {
			return s, sitesignin.ErrState
		}
		m, e := strconv.Atoi(parts[1])
		if e != nil || m < 0 || m > 59 {
			return s, sitesignin.ErrState
		}
		s.start, s.end, s.window = h*60+m, h*60+m, true
		return s, nil
	}
	hours, e := strconv.ParseFloat(raw, 64)
	if e != nil || math.IsNaN(hours) || math.IsInf(hours, 0) || hours <= 0 || hours >= float64(math.MaxInt64)/float64(time.Hour) {
		return s, sitesignin.ErrState
	}
	s.interval = time.Duration(hours * float64(time.Hour))
	if s.interval <= 0 {
		return s, sitesignin.ErrState
	}
	return s, nil
}
func (s signinSchedule) next(after time.Time) (time.Time, error) {
	if s.cron != nil {
		next := s.cron.next(after)
		if next.IsZero() {
			return next, sitesignin.ErrState
		}
		return next, nil
	}
	if s.interval > 0 {
		return after.Add(s.interval), nil
	}
	if !s.window {
		return time.Time{}, nil
	}
	local := after.In(s.location)
	for day := 0; day < 370; day++ {
		date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, s.location).AddDate(0, 0, day)
		start := s.start
		if day == 0 {
			minute := local.Hour()*60 + local.Minute()
			if minute >= start {
				start = minute + 1
			}
		}
		if start > s.end {
			continue
		}
		offset, e := signinRandom(s.end - start + 1)
		if e != nil {
			return time.Time{}, e
		}
		minute := start + offset
		second := 0
		if s.start != s.end {
			second, e = signinRandom(59)
			if e != nil {
				return time.Time{}, e
			}
			second++
		}
		candidate := time.Date(date.Year(), date.Month(), date.Day(), minute/60, minute%60, second, 0, s.location)
		if candidate.Hour() != minute/60 || candidate.Minute() != minute%60 || !candidate.After(after) {
			continue
		}
		return candidate, nil
	}
	return time.Time{}, sitesignin.ErrState
}
func signinRandom(size int) (int, error) {
	if size <= 0 {
		return 0, sitesignin.ErrState
	}
	raw := sitelogin.Random()
	if raw == "" {
		return 0, sitesignin.ErrState
	}
	n, e := strconv.ParseUint(raw[:8], 16, 32)
	return int(n % uint64(size)), e
}
func signinMissedNext(raw string, location *time.Location, after time.Time) (time.Time, error) {
	a, b, e := sitesignin.Window(raw)
	if e != nil {
		return time.Time{}, e
	}
	local := after.In(location)
	for day := 0; day < 370; day++ {
		date := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, location).AddDate(0, 0, day)
		start := a
		if day == 0 && local.Hour()*60+local.Minute() >= a {
			start = (local.Hour() + 1) * 60
		}
		if start > b {
			continue
		}
		end := (start/60+1)*60 - 1
		if end > b {
			end = b
		}
		offset, e := signinRandom(end - start + 1)
		if e != nil {
			return time.Time{}, e
		}
		second, e := signinRandom(59)
		if e != nil {
			return time.Time{}, e
		}
		minute := start + offset
		candidate := time.Date(date.Year(), date.Month(), date.Day(), minute/60, minute%60, second+1, 0, location)
		if candidate.Hour() == minute/60 && candidate.Minute() == minute%60 && candidate.After(after) {
			return candidate, nil
		}
	}
	return time.Time{}, sitesignin.ErrState
}
func signinNext(settings sitesignin.Settings, location *time.Location, now time.Time) (string, string, error) {
	cron, e := parseSigninSchedule(settings.Cron, location)
	if e != nil {
		return "", "", e
	}
	run, missed := "", ""
	if settings.Once {
		run = now.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	} else if settings.Enabled {
		next, e := cron.next(now)
		if e != nil {
			return "", "", e
		}
		if !next.IsZero() {
			run = next.UTC().Format(time.RFC3339Nano)
		}
	}
	if settings.Enabled && settings.Missed {
		next, e := signinMissedNext(settings.MissedWindow, location, now)
		if e != nil {
			return "", "", e
		}
		missed = next.UTC().Format(time.RFC3339Nano)
	}
	return run, missed, nil
}
func (api *siteSigninAPI) selected(ctx context.Context, scope sitesignin.Scope, missed bool) ([]sitesignin.Source, error) {
	location, e := time.LoadLocation(scope.Location)
	if e != nil {
		return nil, e
	}
	daily, e := api.store.Daily(ctx, time.Now().In(location).Format("2006-01-02"))
	if e != nil {
		return nil, e
	}
	if scope.Settings.Clean {
		for id, r := range daily {
			if r.Created < scope.Created && !sitesignin.Active(r.State) && r.Outcome.State != "unknown" && r.Outcome.State != "stopped" {
				delete(daily, id)
			}
		}
	}
	sources := scope.Sources
	if missed {
		wanted := map[string]bool{}
		for _, id := range scope.Settings.Sites {
			wanted[id] = true
		}
		sources = []sitesignin.Source{}
		for _, source := range scope.Sources {
			if wanted[strconv.FormatInt(source.SiteID, 10)] {
				sources = append(sources, source)
			}
		}
	}
	return scope.Settings.Select(sources, daily, missed), nil
}
func (api *siteSigninAPI) tick(ctx context.Context, now time.Time) error {
	guard, e := api.acquire(ctx)
	if e != nil {
		return e
	}
	defer guard.Close()
	if e = api.store.Recover(ctx); e != nil {
		return e
	}
	control, e := api.store.Control(ctx)
	if e != nil || !control.Enabled {
		return e
	}
	if e = api.base(control.Scope, "", control.Scope.PluginDigest)(ctx); e != nil {
		return api.store.StopControl(ctx, control.Scope.ID, "原配置或管理员执行权限已变化，需要重新核对")
	}
	for _, source := range control.Scope.Sources {
		site, e := api.login.sites.Get(ctx, source.SiteID)
		if e != nil || sitelogin.Hash(site) != source.SiteDigest {
			return api.store.StopControl(ctx, control.Scope.ID, "原站点记录已变化，需要重新核对")
		}
	}
	signals, e := api.store.Signals(ctx, true)
	if e != nil {
		return e
	}
	for _, signal := range signals {
		if sitelogin.Hash(signal.Scope) != sitelogin.Hash(control.Scope) {
			if e = api.store.ResolveSignal(ctx, signal, "blocked", "原事件的接管范围已停止或变化", ""); e != nil {
				return e
			}
			continue
		}
		batchID := sitelogin.Hash([]string{"native-signin-signal", signal.ID})
		sources, e := api.selected(ctx, control.Scope, false)
		if e != nil {
			return e
		}
		if _, e = api.launch(ctx, sitesignin.Batch{ID: batchID, Trigger: "signal:" + signal.ID, Scope: control.Scope}, sources, "", guard, true); e != nil {
			return e
		}
		if e = api.store.ResolveSignal(ctx, signal, "handled", "原事件已交给唯一签到执行器", batchID); e != nil {
			return e
		}
	}
	for _, kind := range []string{"cron", "missed"} {
		due := control.NextRun
		if kind == "missed" {
			due = control.NextMissed
		}
		if due == "" {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, due)
		if e != nil {
			return e
		}
		if at.After(now) {
			continue
		}
		batchID := sitelogin.Hash([]string{"native-signin-schedule", control.Scope.ID, kind, due})
		sources, e := api.selected(ctx, control.Scope, kind == "missed")
		if e != nil {
			return e
		}
		if _, e = api.launch(ctx, sitesignin.Batch{ID: batchID, Trigger: kind + ":" + due, Scope: control.Scope}, sources, "", guard, true); e != nil {
			return e
		}
		next := control
		location, e := time.LoadLocation(control.Scope.Location)
		if e != nil {
			return e
		}
		completed := time.Now()
		if kind == "missed" {
			at, e = signinMissedNext(control.Scope.Settings.MissedWindow, location, completed)
			if e != nil {
				return e
			}
			next.NextMissed = at.UTC().Format(time.RFC3339Nano)
		} else {
			next.NextRun = ""
			if control.Scope.Settings.Enabled {
				cron, e := parseSigninSchedule(control.Scope.Settings.Cron, location)
				if e != nil {
					return e
				}
				at, e = cron.next(completed)
				if e != nil {
					return e
				}
				if !at.IsZero() {
					next.NextRun = at.UTC().Format(time.RFC3339Nano)
				}
			}
		}
		if e = api.store.Advance(ctx, control, next); e != nil {
			return e
		}
		control = next
	}
	if control.NextRun == "" && control.NextMissed == "" && !control.Scope.Options.Remote {
		return api.store.StopControl(ctx, control.Scope.ID, "一次签到已完成，未保留周期执行")
	}
	return api.pollTelegram(ctx, control, now)
}
func (api *siteSigninAPI) activate(ctx context.Context, p sitesignin.Review) error {
	if p.Mode != "activate" {
		return sitesignin.ErrState
	}
	guard, e := api.acquire(ctx)
	if e != nil {
		return e
	}
	defer guard.Close()
	previous, e := api.store.Control(ctx)
	if e != nil || sitelogin.Hash(previous) != p.PreviousControlDigest {
		return sitesignin.ErrState
	}
	raw, _, _, _, e := api.pluginSnapshot(ctx)
	if e != nil || sitelogin.Hash(raw) != p.BeforePluginDigest {
		return sitesignin.ErrScope
	}
	after, e := sitesignin.MergeSettings(raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("false"), "clean": json.RawMessage("false")})
	if e != nil || sitelogin.Hash(after) != p.Scope.PluginDigest {
		return sitesignin.ErrScope
	}
	location, e := time.LoadLocation(p.Scope.Location)
	if e != nil {
		return e
	}
	run, missed, e := signinNext(p.Scope.Settings, location, time.Now())
	if e != nil {
		return e
	}
	if run == "" && missed == "" && !p.Scope.Options.Remote {
		return fmt.Errorf("no sign-in consumer is configured")
	}
	next := sitesignin.Control{Scope: p.Scope, Enabled: true, NextRun: run, NextMissed: missed}
	return api.store.Activate(ctx, previous, next, raw, after, api.base(p.Scope, "", p.BeforePluginDigest))
}
