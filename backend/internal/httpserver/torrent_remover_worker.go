package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentremover"
)

func (api *torrentRemovalAPI) client(r torrentremover.Receipt, item downloaderconfig.Downloader, running bool, reviewed string) (completionOrganizationClient, error) {
	organizer := *api.organizer
	organizer.service.client = &http.Client{Transport: api.removalTransport(r, running, reviewed)}
	return organizer.clientFromDownloader(item)
}
func (api *torrentRemovalAPI) removalTransport(r torrentremover.Receipt, running bool, reviewed string) http.RoundTripper {
	return markTransport(func(req *http.Request) (*http.Response, error) {
		var check error
		if running {
			check = api.valid(req.Context(), r, true)
		} else {
			check = api.readValid(req.Context(), r, reviewed)
		}
		if err := check; err != nil {
			return nil, err
		}
		base := api.login.transport
		if base == nil {
			base = http.DefaultTransport
		}
		return base.RoundTrip(req)
	})
}
func (api *torrentRemovalAPI) tick(ctx context.Context) {
	guard, err := api.acquire(ctx)
	if err != nil {
		return
	}
	defer guard.Close()
	if api.store.Recover(ctx) != nil {
		return
	}
	receipts, err := api.store.List(ctx, "pending")
	if err != nil {
		return
	}
	for _, r := range receipts {
		if ctx.Err() != nil {
			return
		}
		if api.valid(ctx, r, false) != nil {
			_ = api.store.BlockPending(ctx, r.ID, "原管理员或配置范围已改变，未移除任务")
			continue
		}
		if api.store.Claim(ctx, r.ID) != nil {
			_ = api.store.BlockPending(ctx, r.ID, "原下载器或历史关系无法核实")
			continue
		}
		api.process(ctx, r)
	}
}
func (api *torrentRemovalAPI) process(parent context.Context, r torrentremover.Receipt) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	tasks := slices.Clone(r.Tasks)
	slices.SortStableFunc(tasks, func(a, b torrentremover.Task) int {
		if a.Parent == "" && b.Parent != "" {
			return -1
		}
		if a.Parent != "" && b.Parent == "" {
			return 1
		}
		return 0
	})
	for _, task := range tasks {
		if api.valid(ctx, r, true) != nil {
			break
		}
		api.removeOne(ctx, r, task)
	}
	book, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	_ = api.store.Finish(book, r.ID)
}
func (api *torrentRemovalAPI) removeOne(ctx context.Context, r torrentremover.Receipt, t torrentremover.Task) {
	block := func(reason string) { _ = api.store.Resolve(ctx, r.ID, t.ID, "pending", "blocked", reason) }
	if t.Parent != "" {
		current, err := api.store.Get(ctx, r.ID)
		if err != nil {
			return
		}
		found := false
		for _, p := range current.Tasks {
			if p.ID == t.Parent && p.State == "completed" {
				found = true
			}
		}
		if !found {
			block("原父任务尚未核实移除，不处理辅种")
			return
		}
	}
	id, err := strconv.ParseInt(t.Downloader, 10, 64)
	if err != nil {
		block("原下载器编号无效")
		return
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, torrentLabelGuard(id, t.Task))
	if err != nil {
		block("原任务有其他操作，请核对")
		return
	}
	defer guard.Close()
	item, err := api.organizer.service.downloaders.Get(ctx, id)
	if err != nil {
		block("原下载器不存在")
		return
	}
	c, err := api.client(r, item, true, "")
	if err != nil {
		block("原下载器配置不可用")
		return
	}
	expected, date, err := torrentremover.ExpectedTask(r.Definition, t.Candidate)
	if err != nil {
		block("原保存目录或创建记录缺失，未发送移除")
		return
	}
	incarnation := torrentremover.Hash([]string{"aria2", t.Task})
	added := int64(0)
	if item.Type != "aria2" {
		at, err := removalHistoryTime(date)
		if err != nil {
			block("原任务创建时间无法核对")
			return
		}
		added, err = api.removalAddedAt(ctx, r, item, t.Task)
		if err != nil || added > at.Unix() {
			block("当前任务晚于原历史，未移除重新添加的任务")
			return
		}
		incarnation = torrentremover.Hash([]any{item.Type, t.Task, added})
	}
	before, err := c.read(ctx, t.Task)

	if err != nil {
		block("原任务文件证据不可读，未发送移除")
		return
	}
	if before.Directory != expected {
		block("当前保存目录与原历史不符")
		return
	}
	if item.Type == "aria2" && !before.Complete {
		block("Aria2 只允许移除已完成结果")
		return
	}
	matched := false
	for _, file := range before.Files {
		if torrentremover.FileMatch(r.Definition.Event, file.Path) {
			matched = true
			break
		}
	}
	if !matched {
		_ = api.store.Resolve(ctx, r.ID, t.ID, "pending", "ignored", "原任务清单不含本次已删除源文件")
		return
	}
	fresh, err := c.read(ctx, t.Task)
	if err != nil || torrentremover.Hash(fresh) != torrentremover.Hash(before) {
		block("任务文件清单已变化")
		return
	}
	// Policy rejection occurs before intent, and is rechecked by the protected
	// client immediately before removal while holding the seeding guard.
	service := *api.organizer
	service.service.client = &http.Client{Transport: api.removalTransport(r, true, "")}
	if service.seedingService().checkSeedingAction(ctx, item, t.Task, "remove") != nil {
		block("原做种或 PT 义务未通过，保留任务")
		return
	}
	if item.Type != "aria2" {
		current, err := api.removalAddedAt(ctx, r, item, t.Task)
		if err != nil || current != added {
			block("原任务实例已变化")
			return
		}
	}
	proof := torrentremover.Hash([]any{r.ID, t.ID, removalDownloaderDigest(item), before, incarnation})
	if err := api.store.Intent(ctx, r.ID, t.ID, proof, incarnation); err != nil {
		if errors.Is(err, torrentremover.ErrAttempted) {
			block("同一任务实例已有移除意图，请核对原记录")
		}
		return
	}
	if c.remove(ctx, t.Task) != nil {
		_ = api.recordUnknown(ctx, r, t)
		return
	}
	present, err := c.has(ctx, t.Task)
	if err != nil || present {
		_ = api.recordUnknown(ctx, r, t)
		return
	}
	_ = api.store.Resolve(ctx, r.ID, t.ID, "intent", "completed", "已核实原任务不存在；移除接口保留全部文件")
}
func (api *torrentRemovalAPI) recordUnknown(ctx context.Context, r torrentremover.Receipt, t torrentremover.Task) error {
	book, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return api.store.Resolve(book, r.ID, t.ID, "intent", "unknown", "移除结果未确认，保留原意图且不自动重发")
}
func (api *torrentRemovalAPI) reconcile(ctx context.Context, r torrentremover.Receipt, reviewed string) error {
	guard, err := api.acquire(ctx)
	if err != nil {
		return err
	}
	defer guard.Close()
	if api.readValid(ctx, r, reviewed) != nil {
		return torrentremover.ErrState
	}
	if r.State != "unknown" && r.State != "stopped" {
		return torrentremover.ErrState
	}
	for _, task := range r.Tasks {
		if task.State != "unknown" && task.State != "intent" {
			continue
		}
		id, err := strconv.ParseInt(task.Downloader, 10, 64)
		if err != nil {
			return err
		}
		item, err := api.organizer.service.downloaders.Get(ctx, id)
		if err != nil {
			return err
		}
		c, err := api.client(r, item, false, reviewed)
		if err != nil {
			return err
		}
		present, err := c.has(ctx, task.Task)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		if err := api.store.Resolve(ctx, r.ID, task.ID, task.State, "completed", "仅通过只读查询核实原任务已不存在"); err != nil {
			return err
		}
	}
	// This only records facts and conditionally updates the frozen relationship
	// rows. It never changes a receipt back into remotely executable work.
	return api.store.FinalizeReconciled(ctx, r.ID)
}

func removalHistoryTime(raw string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return at, nil
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return time.Time{}, err
	}
	at, err := time.ParseInLocation("2006-01-02 15:04:05", raw, loc)
	if err != nil {
		return time.Time{}, torrentremover.ErrState
	}
	return at, nil
}
func (api *torrentRemovalAPI) removalAddedAt(ctx context.Context, r torrentremover.Receipt, item downloaderconfig.Downloader, hash string) (int64, error) {
	var cfg map[string]any
	if json.Unmarshal([]byte(item.Config), &cfg) != nil {
		return 0, torrentremover.ErrState
	}
	transport := api.removalTransport(r, true, "")
	if item.Type == "qbittorrent" {
		c, err := qbittorrent.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), transport)
		if err != nil {
			return 0, err
		}
		return c.TaskAddedAt(ctx, hash)
	}
	c, err := transmission.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), transport)
	if err != nil {
		return 0, err
	}
	return c.TaskAddedAt(ctx, hash)
}
func (api *torrentRemovalAPI) readValid(ctx context.Context, r torrentremover.Receipt, reviewed string) error {
	if !api.login.pureGo || ctx.Err() != nil {
		return torrentremover.ErrState
	}
	scope, err := api.scope(ctx, r.Actor)
	if err != nil || scope.fingerprint() != reviewed {
		return torrentremover.ErrState
	}
	for _, target := range r.Definition.Control.Targets {
		found := false
		for _, item := range scope.targets {
			if strconv.FormatInt(item.ID, 10) == target.ID && removalDownloaderDigest(item) == target.Digest {
				found = true
				break
			}
		}
		if !found {
			return torrentremover.ErrState
		}
	}
	return nil
}
