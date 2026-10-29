package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/movielike"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func (api *movieLikeAPI) automatic(ctx context.Context, p movielike.Receipt) error {
	c, _, on, err := api.queue.Scope(ctx)
	if err != nil || !on || movielike.Hash(c) != movielike.Hash(p.Definition.Control) {
		return movielike.ErrState
	}
	return api.validate(c)
}
func (api *movieLikeAPI) process(ctx context.Context, p movielike.Receipt) error {
	block := func(reason string) error { return api.queue.Resolve(ctx, p, "blocked", reason) }
	if err := api.automatic(ctx, p); err != nil {
		return block("原管理员、配置或电影库授权已停止或变化")
	}
	c := p.Definition.Control
	client, err := api.client()
	if err != nil {
		return block("原 Emby 连接不可用")
	}
	identity, err := client.RefreshIdentity(ctx)
	if err != nil || identity != c.MediaID {
		return block("原 Emby 身份未核实，没有读取电影内容或移动")
	}
	if err := api.automatic(ctx, p); err != nil {
		return block("原授权在身份读取过程中已经停止")
	}
	mapped, err := p.Definition.Spec.Map(p.Event.Path)
	if err != nil {
		return block("原路径映射无效或未映射为本机路径")
	}
	var selected movielike.Root
	relative := ""
	best := 0
	for _, root := range c.Roots {
		for _, base := range []string{root.Path, root.ConfiguredPath} {
			if base != "" && strings.HasPrefix(mapped, base+"/") && len(base) > best {
				selected = root
				relative = strings.TrimPrefix(mapped, base+"/")
				best = len(base)
			}
		}
	}
	if selected.ID == "" {
		return block("原映射路径不在单独批准的电影库内")
	}
	scope := organization.RecoveryScope{Root: organization.LibraryRoot{ID: selected.ID, Path: selected.Path, Identity: selected.Identity, Kind: "MOV"}, ConfigDigest: c.AppDigest}
	source, err := organization.FavoriteMovieDirectory(scope, relative)
	if err != nil {
		return block("原回调没有指向批准分类内的完整电影目录")
	}
	category := path.Dir(source)
	if category == p.Definition.Spec.Directory {
		return api.queue.Resolve(ctx, p, "ignored", "电影已在原精选分类，不再次移动")
	}
	allowed := false
	for _, name := range c.Categories {
		if name == category {
			allowed = true
		}
	}
	if !allowed {
		return api.queue.Resolve(ctx, p, "ignored", "原目录不属于已保存的电影二级分类，不移动")
	}
	target := path.Join(p.Definition.Spec.Directory, path.Base(source))
	aliases := []string{}
	roots, err := api.movieRoots(ctx)
	if err != nil {
		return block("当前原电影库范围不可用")
	}
	for _, root := range roots {
		current, err := libraryRootSnapshot(root)
		if err == nil && current.Path == selected.Path && current.Identity == selected.Identity {
			aliases = append(aliases, root.Path)
		}
	}
	if err := api.automatic(ctx, p); err != nil {
		return block("原授权在完整目录读取前已停止")
	}
	v, err := api.organizer.jobs.PrepareFavoriteMove(ctx, p, scope, source, target, aliases...)
	if err != nil {
		return block("完整电影目录、目标防覆盖或关联范围未核实，未移动任何子集")
	}
	closeGuards, err := api.organizer.recoveryGuards(ctx, organization.RecoveryDefinition{Owners: v.Definition.Owners})
	if err != nil {
		return block("原关联写入器正在执行，未抢占或移动目录")
	}
	defer closeGuards()
	// The coordinator checks its own SQLite authority through the actual
	// transaction. This callback only reads external configuration/identities,
	// avoiding a second checkout of the store's single database connection.
	validate := func() error { return api.validate(c) }
	if err := api.automatic(ctx, p); err != nil {
		return block("原授权在冻结整个目录前已停止")
	}
	if err := api.organizer.jobs.SaveFavoriteMove(ctx, v, validate); err != nil {
		actual, readErr := api.queue.Receipt(ctx, p.ID)
		if readErr == nil && actual.State == "pending" && len(actual.Work) == 0 {
			return block("完整移动计划没有提交，未执行文件操作")
		}
		return err
	}
	review, err := api.organizer.jobs.ReviewFavoriteMove(ctx, p.ID)
	if err != nil {
		return err
	}
	if !review.CanContinue {
		return organization.ErrState
	}
	return api.organizer.jobs.ExecuteFavoriteMove(ctx, review, c.Actor, true, validate)
}
func (api *movieLikeAPI) tick(ctx context.Context) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, movieLikeGuard)
	if err != nil {
		return err
	}
	defer guard.Close()
	rows, err := api.queue.List(ctx, 1, true)
	if err != nil {
		return err
	}
	for _, p := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := api.process(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
func startMovieLikeWorker(ctx context.Context, api *movieLikeAPI) func() {
	if api == nil || api.organizer == nil || !api.organizer.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	api.workers.Add(1)
	go func() {
		defer close(done)
		defer api.workers.Add(-1)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				call, cancel := context.WithTimeout(ctx, organization.ExecutionTimeout)
				err := api.tick(call)
				cancel()
				if err != nil && !errors.Is(err, organization.ErrBusy) && !errors.Is(err, context.Canceled) {
					slog.Warn("favorite movie move interrupted; original receipts and objects retained without automatic retry")
				}
			}
		}
	}()
	return func() { <-done }
}
