package httpserver

import (
	"context"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/cookiecloud"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func startCookieCloudWorker(ctx context.Context, api *cookieCloudAPI) func() {
	if api == nil || api.store == nil {
		return func() {}
	}
	api.mu.Lock()
	api.lifecycle = ctx
	api.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			api.mu.Lock()
			api.lifecycle = nil
			api.mu.Unlock()
			api.wait.Wait()
		}()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		api.tick(ctx, time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				api.tick(ctx, now)
			}
		}
	}()
	return func() { <-done }
}

func (api *cookieCloudAPI) acquire(ctx context.Context) (*organization.JobGuard, error) {
	if api.login.guards == nil || *api.login.guards == nil {
		return nil, cookiecloud.ErrScope
	}
	return (*api.login.guards).AcquireJob(ctx, "native-cookie-cloud")
}

func (api *cookieCloudAPI) tick(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	if guard, err := api.acquire(ctx); err == nil {
		_ = api.store.Recover(ctx)
		guard.Close()
	}
	control, err := api.store.Control(ctx)
	if err != nil {
		return
	}
	once, _ := time.Parse(time.RFC3339Nano, control.NextOnce)
	cron, _ := time.Parse(time.RFC3339Nano, control.NextRun)
	due := func(at time.Time) bool { return !at.IsZero() && !at.After(now) }
	if !due(once) && !due(cron) {
		return
	}
	scope, err := api.scope(ctx, control.Actor)
	if err != nil || !controlCurrent(control, scope) {
		return
	}
	next := control
	kind, slot := "once", control.NextOnce
	if due(once) {
		next.NextOnce = ""
	} else {
		kind, slot = "cron", control.NextRun
		at, err := cookieCloudSchedule(scope.settings.Cron, scope.location, now)
		if err != nil || at.IsZero() {
			return
		}
		next.NextRun = at.UTC().Format(time.RFC3339Nano)
	}
	run := cookiecloud.Run{ID: sitelogin.Hash([]string{"cookie-cloud", control.Epoch, kind, slot}), Kind: kind, Actor: scope.actor, Scope: control.Scope, Epoch: control.Epoch}
	_, _ = api.launch(ctx, scope, run, control, next)
}

func (api *cookieCloudAPI) launch(ctx context.Context, scope cookieCloudScope, run cookiecloud.Run, old, next cookiecloud.Control) (cookiecloud.Run, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.lifecycle == nil || api.lifecycle.Err() != nil {
		return cookiecloud.Run{}, cookiecloud.ErrScope
	}
	guard, err := api.acquire(ctx)
	if err != nil {
		return cookiecloud.Run{}, err
	}
	started := false
	defer func() {
		if !started {
			guard.Close()
		}
	}()
	if original, err := api.store.Run(ctx, run.ID); err == nil && original.Actor == run.Actor {
		return original, nil
	}
	if err = api.store.Recover(ctx); err != nil {
		return cookiecloud.Run{}, err
	}
	run, err = api.store.Start(ctx, run, scope.raw, old, next)
	if err != nil {
		return cookiecloud.Run{}, err
	}
	execution, cancel := context.WithTimeout(api.lifecycle, 8*time.Minute)
	api.cancel = cancel
	api.wait.Add(1)
	started = true
	go func() {
		defer api.wait.Done()
		defer func() {
			cancel()
			api.mu.Lock()
			api.cancel = nil
			guard.Close()
			api.mu.Unlock()
		}()
		api.execute(execution, scope, run)
	}()
	return run, nil
}

func (api *cookieCloudAPI) runningScope(ctx context.Context, expected cookieCloudScope, run cookiecloud.Run) error {
	current, err := api.scope(ctx, run.Actor)
	if err != nil || !current.installed || current.fingerprint(current.raw) != run.Scope || current.raw != expected.raw {
		return cookiecloud.ErrScope
	}
	job, err := api.store.Run(ctx, run.ID)
	if err != nil || job.State != "running" || job.Stop {
		return cookiecloud.ErrScope
	}
	if run.Epoch != "" {
		control, err := api.store.Control(ctx)
		if err != nil || control.Scope != run.Scope || control.Epoch != run.Epoch {
			return cookiecloud.ErrScope
		}
	}
	return ctx.Err()
}

func (api *cookieCloudAPI) execute(ctx context.Context, scope cookieCloudScope, run cookiecloud.Run) {
	var original []siteconfig.Site
	changes := []cookiecloud.Candidate{}
	finish := func(state, message string) {
		book, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		run.State, run.Message = state, message
		if state != "complete" {
			changes = nil
		}
		notify := scope.settings.Notify && state == "complete" && run.Kind != "test"
		if err := api.store.Complete(book, run, scope.raw, original, changes, notify); err != nil {
			run.State, run.Message = "failed", "原站点或设置已变化，同步未发布"
			_ = api.store.Complete(book, run, scope.raw, nil, nil, false)
		}
	}
	if api.runningScope(ctx, scope, run) != nil {
		finish("stopped", "原设置已变化")
		return
	}
	var err error
	if run.Kind != "test" {
		original, err = api.login.sites.List(ctx)
		if err != nil || len(original) > 3000 {
			finish("failed", "原站点范围不可读取")
			return
		}
	}
	cookies, err := cookiecloud.Fetch(ctx, scope.settings, api.login.transport)
	if err != nil {
		if ctx.Err() != nil {
			finish("unknown", "连接已中断，请查看原记录")
		} else {
			finish("failed", cookiecloud.ErrCloud.Error())
		}
		return
	}
	if run.Kind != "test" {
		for _, candidate := range cookiecloud.Candidates(original, scope.catalog, cookies, scope.settings, time.Now()) {
			if err = api.runningScope(ctx, scope, run); err != nil {
				finish("stopped", "原设置已变化")
				return
			}
			site := candidate.Original
			if site.ID != 0 {
				if site.Cookie == candidate.Cookie {
					run.Kept++
					continue
				}
				checkSite := func(ctx context.Context) error {
					current, err := api.login.sites.Get(ctx, site.ID)
					if err != nil || current != site {
						return cookiecloud.ErrScope
					}
					return api.runningScope(ctx, scope, run)
				}
				admit := func(ctx context.Context) error {
					if err := checkSite(ctx); err != nil {
						return err
					}
					if err := api.login.limits.Wait(ctx, api.login.sites, site.ID); err != nil {
						return err
					}
					return checkSite(ctx)
				}
				if err = checkSite(ctx); err != nil {
					run.Skipped++
					continue
				}
				ok, reason, unsupported := checkSiteConnectionAdmitted(ctx, site, scope.app, api.login.transport, admit, api.login.browser)
				if ok {
					run.Kept++
					continue
				}
				if unsupported || reason == "站点限流等待被取消或配置无效" || reason == "站点配置无效" || reason == "未配置有效站点地址" {
					run.Skipped++
					continue
				}
			}
			changes = append(changes, candidate)
		}
	}
	if api.runningScope(ctx, scope, run) != nil {
		finish("stopped", "原设置已变化")
		return
	}
	finish("complete", "测试连通性成功")
}
