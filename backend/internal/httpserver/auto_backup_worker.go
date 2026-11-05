package httpserver

import (
	"context"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/autobackup"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func startAutoBackupWorker(ctx context.Context, api *autoBackupAPI) func() {
	if api == nil || api.store == nil {
		return func() {}
	}
	api.mu.Lock()
	api.ctx = ctx
	api.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			api.mu.Lock()
			api.ctx = nil
			api.mu.Unlock()
			api.wait.Wait()
		}()
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		api.tick(ctx, time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				api.tick(ctx, now)
			}
		}
	}()
	return func() { <-done }
}

func (api *autoBackupAPI) acquire(ctx context.Context) (*organization.JobGuard, error) {
	if api.login.guards == nil || *api.login.guards == nil {
		return nil, autobackup.ErrScope
	}
	return (*api.login.guards).AcquireJob(ctx, "native-auto-backup")
}

func (api *autoBackupAPI) tick(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	if guard, err := api.acquire(ctx); err == nil {
		_ = api.recover(ctx)
		guard.Close()
	}
	state, err := api.store.State(ctx)
	if err != nil || !state.Installed || state.Control.Epoch == "" {
		return
	}
	c := state.Control
	once, _ := time.Parse(time.RFC3339Nano, c.Once)
	next, _ := time.Parse(time.RFC3339Nano, c.Next)
	due := func(at time.Time) bool { return !at.IsZero() && !at.After(now) }
	if !due(once) && !due(next) {
		return
	}
	s, err := api.readScope(ctx, c.Actor, false)
	if err != nil || s.state != state || !backupAuthorized(s) {
		return
	}
	kind, slot, updated := "once", c.Once, c
	if due(once) {
		updated.Once = ""
	} else {
		kind, slot = "cron", c.Next
		future, err := cookieCloudSchedule(s.settings.Cron, s.location, now)
		if err != nil || future.IsZero() {
			return
		}
		updated.Next = future.UTC().Format(time.RFC3339Nano)
	}
	id := sitelogin.Hash([]string{"auto-backup", c.Epoch, kind, slot})
	_, _, _ = api.launch(ctx, s, updated, id, kind)
}

func (api *autoBackupAPI) recover(ctx context.Context) error {
	pending, err := api.store.PendingPrunes(ctx)
	if err != nil {
		return err
	}
	for _, item := range pending {
		_ = api.store.ReconcilePrune(ctx, item)
	}
	items, err := api.store.Runs(ctx, 1)
	if err != nil || len(items) == 0 {
		return err
	}
	before := items[0]
	if before.State != "running" && before.State != "staged" {
		return nil
	}
	next := before
	next.State, next.Message = "unknown", "进程中断，请核对归档目录"
	if before.Stop {
		next.State, next.Message = "stopped", "备份已停止"
	}
	if before.State == "staged" {
		if f, e := autobackup.Open(ctx, before.Target, before.Archive); e == nil {
			f.Close()
			next.State, next.Message = "complete", "重启后已核验原归档"
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return api.store.Replace(ctx, before, next, nil, false)
}

func (api *autoBackupAPI) launch(ctx context.Context, s backupScope, next autobackup.Control, id, kind string) (autobackup.Run, <-chan struct{}, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.ctx == nil || api.ctx.Err() != nil {
		return autobackup.Run{}, nil, autobackup.ErrScope
	}
	guard, err := api.acquire(ctx)
	if err != nil {
		return autobackup.Run{}, nil, err
	}
	owned := true
	defer func() {
		if owned {
			guard.Close()
		}
	}()
	if run, e := api.store.Run(ctx, id); e == nil {
		done := make(chan struct{})
		close(done)
		return run, done, nil
	}
	if err := api.recover(ctx); err != nil {
		return autobackup.Run{}, nil, err
	}
	target, err := autobackup.Prepare(s.target)
	if err != nil {
		return autobackup.Run{}, nil, err
	}
	s.target = target
	run := autobackup.Run{ID: id, Kind: kind, Actor: s.actor, Scope: s.fingerprint(), Target: target}
	if kind == "cron" || kind == "once" {
		run.Epoch = s.state.Control.Epoch
	}
	run, err = api.store.Start(ctx, s.state, next, run)
	if err != nil {
		return autobackup.Run{}, nil, err
	}
	s.state.Control = next
	exec, cancel := context.WithTimeout(api.ctx, 30*time.Minute)
	api.cancel = cancel
	api.wait.Add(1)
	owned = false
	done := make(chan struct{})
	go func() {
		defer api.wait.Done()
		defer close(done)
		defer func() {
			cancel()
			api.mu.Lock()
			api.cancel = nil
			guard.Close()
			api.mu.Unlock()
		}()
		api.execute(exec, s, run)
	}()
	return run, done, nil
}

func (api *autoBackupAPI) valid(ctx context.Context, s backupScope, r autobackup.Run) error {
	current, err := api.readScope(ctx, r.Actor, r.Kind == "legacy")
	if err != nil || current.state != s.state || current.fingerprint() != r.Scope {
		return autobackup.ErrScope
	}
	return ctx.Err()
}

func (api *autoBackupAPI) fail(ctx context.Context, r autobackup.Run, cause error) {
	book, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	before, err := api.store.Run(book, r.ID)
	if err != nil || before.State != "running" && before.State != "staged" {
		return
	}
	next := before
	next.State, next.Message = "failed", "创建备份失败"
	if before.State == "staged" {
		if f, e := autobackup.Open(book, before.Target, before.Archive); e == nil {
			f.Close()
			next.State, next.Message = "complete", "已核验原归档"
		} else if !errors.Is(e, context.DeadlineExceeded) && !errors.Is(cause, autobackup.ErrScope) {
			next.State, next.Message = "unknown", "未能核实归档发布结果"
		}
	}
	if next.State != "complete" && (before.Stop || errors.Is(cause, autobackup.ErrScope) || errors.Is(cause, context.Canceled)) {
		next.State, next.Message = "stopped", "备份授权已停止或变化"
	}
	_ = api.store.Replace(book, before, next, nil, false)
}

func (api *autoBackupAPI) execute(ctx context.Context, s backupScope, r autobackup.Run) {
	if err := api.valid(ctx, s, r); err != nil {
		api.fail(ctx, r, err)
		return
	}
	stage, err := autobackup.Build(ctx, api.sources, s.target, r.ID, time.Now(), s.settings.Full)
	if err != nil {
		api.fail(ctx, r, err)
		return
	}
	defer stage.Close()
	staged := r
	staged.State, staged.Archive = "staged", stage.Archive
	if err := api.store.Replace(ctx, r, staged, &s.state, false); err != nil {
		api.fail(ctx, r, err)
		return
	}
	err = api.store.Finalize(ctx, staged, s.state, s.settings.Notify, func() error {
		authority, _, err := api.login.authority(r.Actor)
		if err != nil || authority != s.authority || ctx.Err() != nil {
			return autobackup.ErrScope
		}
		return stage.Publish()
	})
	if err != nil {
		api.fail(ctx, r, err)
		return
	}
	if keep := s.settings.Limit(); keep > 0 {
		api.prune(ctx, s, r, keep)
	}
}

func (api *autoBackupAPI) prune(ctx context.Context, s backupScope, r autobackup.Run, keep int) {
	items, err := api.store.Retention(ctx, s.target)
	if err != nil || len(items) <= keep || items[0].ID != r.ID {
		return
	}
	for _, retained := range items[:keep] {
		f, err := autobackup.Open(ctx, retained.Target, retained.Archive)
		if err != nil {
			return
		}
		f.Close()
	}
	for _, item := range items[keep:] {
		if api.valid(ctx, s, r) != nil {
			return
		}
		owner, err := api.store.Run(ctx, r.ID)
		if err != nil {
			return
		}
		_ = autobackup.Remove(ctx, item.Target, item.Archive, func(remove func() error) error {
			return api.store.Prune(ctx, s.state, owner, item, remove)
		})
	}
}
