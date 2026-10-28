package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (api *mediaSyncDeleteAPI) notificationImage(ctx context.Context, e mediasyncdel.Event, notify bool) string {
	if !notify {
		return ""
	}
	fallback := "https://emby.media/notificationicon.png"
	var selected string
	if e.Type == "Episode" {
		snapshot, err := api.organizer.service.configStore.Snapshot()
		if err != nil {
			return fallback
		}
		app := objectValue(snapshot["app"])
		key := text(app["rmt_tmdbkey"])
		if key == "" {
			return fallback
		}
		host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text(app["tmdb_domain"])), "https://"), "http://"), "/")
		if host == "" {
			host = "api.themoviedb.org"
		}
		if strings.ContainsAny(host, "/?#@\\") || strings.Contains(host, "..") {
			return fallback
		}
		endpoint := url.URL{Scheme: "https", Host: host, Path: "/3/tv/" + e.TMDB + "/season/" + strconv.Itoa(e.Season) + "/episode/" + strconv.Itoa(e.Episode) + "/images", RawQuery: url.Values{"api_key": {key}}.Encode()}
		r, err := http.NewRequestWithContext(ctx, "GET", endpoint.String(), nil)
		if err != nil {
			return fallback
		}
		client := &http.Client{Transport: api.transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		response, err := client.Do(r)
		if err != nil {
			return fallback
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return fallback
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil || len(raw) > 1<<20 {
			return fallback
		}
		var data struct {
			Stills []struct {
				Path string `json:"file_path"`
			} `json:"stills"`
		}
		if json.Unmarshal(raw, &data) != nil || len(data.Stills) > 1000 {
			return fallback
		}
		for _, s := range data.Stills {
			if strings.HasPrefix(s.Path, "/") {
				selected = s.Path
				break
			}
		}
	} else {
		kind := "tv"
		if e.Type == "Movie" {
			kind = "movie"
		}
		details, err := fetchNativeTMDBDetails(ctx, api.organizer.service.configStore, api.transport, kind, e.TMDB)
		if err != nil {
			return fallback
		}
		selected = text(details.Attributes["backdrop_path"])
	}
	image := "https://image.tmdb.org/t/p/original" + selected
	if !strings.HasPrefix(selected, "/") || !mediasyncdel.SafeImage(image) {
		return fallback
	}
	return image
}
func (api *mediaSyncDeleteAPI) process(ctx context.Context, p mediasyncdel.Receipt) error {
	c := p.Definition.Control
	if err := api.validate(c); err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原管理员、Emby 配置或源目录身份已变化")
	}
	if err := api.queue.CheckAutomatic(ctx, p.ID); err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原同步删除范围已停止或变化")
	}
	client, err := api.client()
	if err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原 Emby 连接不可用")
	}
	identity, err := client.RefreshIdentity(ctx)
	if err != nil || identity != c.MediaID {
		return api.queue.ResolvePending(ctx, p, "blocked", "原 Emby 服务身份未核实，没有执行删除")
	}
	if err := api.queue.CheckAutomatic(ctx, p.ID); err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原同步删除授权在服务读取过程中已停止")
	}
	ids, err := api.queue.CandidateIDs(ctx, p.Event)
	if err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "完整原历史匹配未核实，没有处理任何子集")
	}
	if len(ids) == 0 {
		return api.queue.ResolvePending(ctx, p, "ignored", "没有匹配原类型、季集和文件名的历史，不处理文件或通知")
	}
	input := mediaRemovalInput{HistoryIDs: []string{}, Paths: []mediaRemovalPath{}, IncludeNFO: p.Policy.DeleteSource, CleanupDirectories: p.Policy.DeleteSource, RequestID: mediasyncdel.RequestID(p.ID), Decision: "原 Emby 删除回调 " + p.Callback, ConfirmRead: true, MediaSyncDelete: &organization.MediaSyncDeleteRemoval{Receipt: p.ID}}
	for _, id := range ids {
		input.HistoryIDs = append(input.HistoryIDs, strconv.FormatInt(id, 10))
	}
	if p.Policy.DeleteSource {
		input.Flag = "del_source"
	}
	// Resolve history locations before reading any file contents. Only the
	// independently approved source roots may supply physical deletion units.
	records, _, err := api.organizer.jobs.MediaRemovalHistories(ctx, ids)
	if err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原完整历史已变化")
	}
	paths, err := api.organizer.mediaRemovalSelections(ctx, input, records)
	if err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原源文件位置无法按已批准范围核实")
	}
	allowed := map[string]bool{}
	for _, root := range c.Roots {
		allowed[root.ID] = true
	}
	for _, selected := range paths {
		if !allowed[selected.RootID] || selected.Driver != "" || selected.Role != "source" {
			return api.queue.ResolvePending(ctx, p, "blocked", "原源文件不在单独批准的本机源目录内")
		}
	}
	input.MediaSyncDelete.Image = api.notificationImage(ctx, p.Event, p.Policy.Notify)
	if err := api.queue.CheckAutomatic(ctx, p.ID); err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原同步删除授权在准备源范围前已停止")
	}
	if err := api.validate(c); err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原授权在读取过程中变化")
	}
	v, closeAll, err := api.organizer.prepareMediaRemoval(ctx, input, c.Actor)
	if err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "完整源范围、PT 约束或关联执行器未核实；未处理子集")
	}
	defer closeAll()
	for _, unit := range v.Definition.Units {
		if !allowed[unit.Definition.Scope.Root.ID] || unit.Definition.Scope.Remote != nil {
			return api.queue.ResolvePending(ctx, p, "blocked", "展开目录超出原批准的源范围")
		}
	}
	if err := api.validate(c); err != nil {
		return api.queue.ResolvePending(ctx, p, "blocked", "原授权在建立完整计划前变化")
	}
	parent, err := api.organizer.jobs.CreateMediaRemoval(ctx, v.Definition, v.Fingerprint)
	if err != nil {
		current, readErr := api.queue.Receipt(ctx, p.ID)
		if readErr == nil && current.Parent == "" && current.State == "pending" {
			return api.queue.ResolvePending(ctx, p, "blocked", "原完整计划未提交，未执行删除")
		}
		return err // A committed original plan is retained, never re-created.
	}
	saved, err := api.organizer.jobs.MediaRemoval(ctx, parent)
	if err != nil {
		return err
	}
	clients, closeClients, err := api.organizer.mediaRemovalClients(ctx, saved)
	if err != nil {
		return err
	}
	defer closeClients()
	validate := func(scope organization.RecoveryScope) error {
		if err := api.validate(c); err != nil {
			return err
		}
		if err := api.queue.CheckAutomatic(ctx, p.ID); err != nil {
			return err
		}
		return api.organizer.recoveryScopeCurrent(ctx, scope)
	}
	reviewed, err := api.organizer.jobs.ReviewMediaRemoval(ctx, saved, clients)
	if err != nil {
		return err
	}
	guard := func(call context.Context, tx *sql.Tx) error {
		if err := api.validate(c); err != nil {
			return err
		}
		original, err := mediasyncdel.ReadReceipt(call, tx, p.ID)
		if err != nil {
			return err
		}
		if original.State == "completed" {
			current, _, on, err := mediasyncdel.Scope(call, tx)
			if err != nil || !on || mediasyncdel.Hash(current) != mediasyncdel.Hash(c) {
				return mediasyncdel.ErrState
			}
			return nil
		}
		return mediasyncdel.CheckAutomatic(call, tx, p.ID, true)
	}
	if !reviewed.CanContinue {
		return organization.ErrState
	}
	return api.organizer.jobs.ExecuteMediaRemoval(ctx, reviewed, clients, reviewed.Fingerprint, validate, guard)
}
func (api *mediaSyncDeleteAPI) tick(ctx context.Context) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.queue == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, mediaSyncDeleteGuard)
	if err != nil {
		return err
	}
	defer guard.Close()
	pending, err := api.queue.List(ctx, 1, true)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := api.process(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
func startMediaSyncDeleteWorker(ctx context.Context, api *mediaSyncDeleteAPI) func() {
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
					slog.Warn("media callback deletion interrupted; original scope and receipts retained without automatic retry")
				}
			}
		}
	}()
	return func() { <-done }
}
