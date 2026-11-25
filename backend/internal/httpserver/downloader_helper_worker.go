package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloaderhelper"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type helperClient interface {
	HelperTasks(context.Context, string) ([]downloaderhelper.Task, error)
	SetHelperLabels(context.Context, downloaderhelper.Task, string, string) error
	TransferTask(context.Context, string) (torrenttransfer.Task, error)
	Address() string
}
type helperTrigger struct {
	Kind, Path, Filename, Driver string
	Downloader, Row              int64
	At                           time.Time
}
type helperItem struct {
	Downloader                  int64
	Task                        downloaderhelper.Task
	Action, Want, Drop, Payload string
}
type helperRun struct {
	api     *downloaderHelperAPI
	raw     map[string]string
	st      helperSchedule
	s       helperScope
	trigger helperTrigger
	id      string
}

func startDownloaderHelper(ctx context.Context, api *downloaderHelperAPI) func() {
	if api == nil || api.transfer.organizer.jobs == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-tick.C:
				if err := api.tick(ctx, now); err != nil && ctx.Err() == nil {
					slog.Warn("下载器助手更新任务失败")
				}
			}
		}
	}()
	return func() { <-done }
}
func (api *downloaderHelperAPI) next(ctx context.Context, st helperSchedule, cfg helperSettings, now time.Time) (helperTrigger, error) {
	if !st.Once.IsZero() && !st.Once.After(now) {
		return helperTrigger{Kind: "once", At: st.Once}, nil
	}
	if !st.Next.IsZero() && !st.Next.After(now) {
		return helperTrigger{Kind: "cron", At: st.Next}, nil
	}
	var e helperTrigger
	if st.Since.IsZero() || !cfg.Enable {
		return e, nil
	}
	err := api.transfer.system.Transaction(ctx, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_DOWNLOADER_HELPER_EVENTS')").Scan(&exists); err != nil || !exists {
			return err
		}
		var created, data string
		err := tx.QueryRowContext(ctx, "SELECT rowid,kind,created,data FROM GO_DOWNLOADER_HELPER_EVENTS WHERE revision=? AND rowid>? ORDER BY rowid LIMIT 1", st.Revision, st.Cursor).Scan(&e.Row, &e.Kind, &created, &data)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		e.At, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return err
		}
		if e.At.Before(st.Since) {
			e.Kind = "skip"
			return nil
		}
		if e.At.Add(3 * time.Second).After(now) {
			e = helperTrigger{}
			return nil
		}
		var v struct {
			ID                     json.Number `json:"downloader_id"`
			Path, Filename, Driver string
		}
		if json.Unmarshal([]byte(data), &v) != nil {
			e.Kind = "skip"
			return nil
		}
		e.Path, e.Filename, e.Driver = v.Path, v.Filename, v.Driver
		if v.ID != "" {
			e.Downloader, err = v.ID.Int64()
			if err != nil || e.Downloader <= 0 {
				e.Kind = "skip"
			}
		}
		return nil
	})
	return e, err
}

func (api *downloaderHelperAPI) tick(ctx context.Context, now time.Time) error {
	raw, err := api.transfer.system.ReadMany(ctx, helperKeys)
	if err != nil {
		return err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], helperID)
	if err != nil || !installed {
		return err
	}
	if raw[helperControlKey] == "" {
		return api.importSettings(ctx, raw, now)
	}
	st, err := helperState(raw[helperControlKey])
	if err != nil || st.Actor == "" {
		return err
	}
	v, err := helperValues(raw[helperConfigKey])
	if err != nil {
		return err
	}
	var cfg helperSettings
	if err = helperDecode(v, cfg.fields()); err != nil {
		return err
	}
	var e helperTrigger
	if st.Pending != "" {
		if err = json.Unmarshal([]byte(st.Pending), &e); err != nil {
			return err
		}
	} else {
		e, err = api.next(ctx, st, cfg, now)
		if err != nil || e.Kind == "" {
			return err
		}
	}
	g, err := api.transfer.organizer.jobs.AcquireJob(ctx, "downloader-helper")
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer g.Close()
	s, err := api.selection(ctx, st.Actor, v)
	if err != nil {
		return err
	}
	save := func(next helperSchedule) error {
		return api.transfer.system.UpdateMany(ctx, helperKeys, func(live map[string]string) (map[string]string, error) {
			if !maps.Equal(live, raw) {
				return nil, downloaderhelper.ErrSettings
			}
			b, err := json.Marshal(next)
			return map[string]string{helperControlKey: string(b)}, err
		})
	}
	if s.proof != st.Scope {
		return save(helperSchedule{Last: "下载器或站点配置已变化，请重新保存任务设置。"})
	}
	if st.Pending == "" {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		st.Pending = string(b)
		if err = save(st); err != nil {
			return err
		}
		state, _ := json.Marshal(st)
		raw[helperControlKey] = string(state)
	}
	run := helperRun{api: api, raw: raw, st: st, s: s, trigger: e, id: bus.Hash([]any{st.Revision, e})}
	return run.execute(ctx, now)
}
func (r *helperRun) current(ctx context.Context) error {
	live, err := r.api.transfer.system.ReadMany(ctx, helperKeys)
	if err != nil || !maps.Equal(live, r.raw) {
		return downloaderhelper.ErrSettings
	}
	v, err := helperValues(live[helperConfigKey])
	if err != nil {
		return err
	}
	s, err := r.api.selection(ctx, r.st.Actor, v)
	if err != nil || s.proof != r.s.proof {
		return downloaderhelper.ErrSettings
	}
	return ctx.Err()
}
func (r *helperRun) client(d downloaderconfig.Downloader) (helperClient, error) {
	cc, err := r.api.transfer.organizer.clientFromDownloader(d)
	if err != nil {
		return nil, err
	}
	c, ok := cc.transfer.(helperClient)
	if !ok {
		return nil, downloaderhelper.ErrSettings
	}
	return c, nil
}

// A deletion event concerns the top-level data entry from the actual file
// list, which can differ from the editable display name. Require the mapped
// directory still mounted and the entire entry absent before remote deletion.
func helperSourceGone(ctx context.Context, c helperClient, d downloaderconfig.Downloader, t downloaderhelper.Task, e helperTrigger) bool {
	if e.Kind != "sourcefile.deleted" || e.Driver != "" && e.Driver != "local" || e.Filename == "" || filepath.Base(e.Filename) != e.Filename || !downloadcompletion.Relative(e.Filename) || !downloadcompletion.Absolute(e.Path) || t.Added >= e.At.Unix() {
		return false
	}
	location, err := downloadcompletion.Resolve(d.DownloadDir, t.Directory, d.MatchPath == 1)
	if err != nil {
		return false
	}
	files, err := c.TransferTask(ctx, t.Hash)
	if err != nil || files.AddedAt != t.Added || files.Directory != t.Directory || len(files.Files) == 0 {
		return false
	}
	top, _, _ := strings.Cut(files.Files[0].Path, "/")
	if top == "" || top == "." || top == ".." || strings.ContainsAny(top, "\\\x00") {
		return false
	}
	for _, f := range files.Files {
		part, _, _ := strings.Cut(f.Path, "/")
		if part != top {
			return false
		}
	}
	root := filepath.Join(location.Directory, top)
	source := filepath.Join(e.Path, e.Filename)
	if source != root && !strings.HasPrefix(source, root+"/") {
		return false
	}
	real, err := filepath.EvalSymlinks(location.Directory)
	if err != nil || real != location.Directory {
		return false
	}
	dir, err := os.Stat(real)
	if err != nil || !dir.IsDir() {
		return false
	}
	_, err = os.Lstat(root)
	return os.IsNotExist(err)
}
func (r *helperRun) actions(ctx context.Context, target helperTarget, c helperClient, t downloaderhelper.Task) []helperItem {
	if downloaderhelper.Excluded(t.Labels, r.s.exclude) {
		return nil
	}
	m, e := target.Mode, r.trigger
	row := helperItem{Downloader: target.Item.ID, Task: t}
	if e.Kind == "sourcefile.deleted" {
		if m.Delete && helperSourceGone(ctx, c, target.Item, t, e) {
			row.Payload = r.removalProof(ctx, target.Item, c, t)
			row.Action = "remove"
			return []helperItem{row}
		}
		return nil
	}
	if e.Kind != "download.add" && t.Missing && m.Delete {
		row.Payload = r.removalProof(ctx, target.Item, c, t)
		row.Action = "remove"
		return []helperItem{row}
	}
	var rows []helperItem
	if e.Kind != "download.add" && m.Seed && t.CanSeed {
		row.Action = "seed"
		rows = append(rows, row)
	}
	if m.Tag {
		row.Want, row.Drop = downloaderhelper.TagPair(t.Site, r.s.sites[t.Site], r.s.cfg.UseName)
		if row.Want != "" && !slices.Equal(t.Labels, downloaderhelper.LabelsAfter(t.Labels, row.Want, row.Drop)) {
			row.Action = "tag"
			rows = append(rows, row)
		}
	}
	return rows
}

type helperBatch struct {
	Items           []helperItem
	Scanned, Failed int
	ByDownloader    map[int64]int
	Unavailable     map[int64]bool
}

func (r *helperRun) batch(ctx context.Context) (helperBatch, error) {
	var b helperBatch
	store := r.api.transfer.system
	var saved string
	err := store.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_DOWNLOADER_HELPER_BATCH(slot INTEGER PRIMARY KEY CHECK(slot=1),id TEXT NOT NULL,manifest TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS GO_DOWNLOADER_HELPER_ACTION(id TEXT PRIMARY KEY,object TEXT NOT NULL,action TEXT NOT NULL,state TEXT NOT NULL,desired TEXT NOT NULL,downloader INTEGER NOT NULL);
		CREATE UNIQUE INDEX IF NOT EXISTS GO_DOWNLOADER_HELPER_PENDING ON GO_DOWNLOADER_HELPER_ACTION(object,action) WHERE state='pending'`)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, "SELECT manifest FROM GO_DOWNLOADER_HELPER_BATCH WHERE slot=1 AND id=?", r.id).Scan(&saved)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return b, err
	}
	if saved != "" {
		err = json.Unmarshal([]byte(saved), &b)
		return b, err
	}
	b.ByDownloader = map[int64]int{}
	b.Unavailable = map[int64]bool{}
	for _, target := range r.s.targets {
		if r.trigger.Kind == "skip" || r.trigger.Downloader > 0 && target.Item.ID != r.trigger.Downloader || r.trigger.Kind == "download.add" && !target.Mode.Tag || r.trigger.Kind == "sourcefile.deleted" && !target.Mode.Delete {
			continue
		}
		if err := r.current(ctx); err != nil {
			return b, err
		}
		c, err := r.client(target.Item)
		if err != nil {
			b.Failed++
			b.Unavailable[target.Item.ID] = true
			continue
		}
		tasks, err := c.HelperTasks(ctx, "")
		if err != nil {
			b.Failed++
			b.Unavailable[target.Item.ID] = true
			continue
		}
		if err := r.reconcile(ctx, c, target, tasks); err != nil {
			return b, err
		}
		b.Scanned += len(tasks)
		b.ByDownloader[target.Item.ID] = len(tasks)
		for _, t := range tasks {
			b.Items = append(b.Items, r.actions(ctx, target, c, t)...)
		}
	}
	if err := r.current(ctx); err != nil {
		return b, err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return b, err
	}
	err = store.Transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO GO_DOWNLOADER_HELPER_BATCH VALUES(1,?,?) ON CONFLICT(slot) DO UPDATE SET id=excluded.id,manifest=excluded.manifest`, r.id, string(data))
		return err
	})
	return b, err
}

func (r *helperRun) execute(ctx context.Context, now time.Time) error {
	if err := r.current(ctx); err != nil {
		return err
	}
	b, err := r.batch(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{"failed": b.Failed}
	perTarget := map[int64]map[string]int{}
	for i, item := range b.Items {
		if err := r.current(ctx); err != nil {
			return err
		}
		state, err := r.apply(ctx, item, i)
		if err != nil {
			return err
		}
		counts[state]++
		if perTarget[item.Downloader] == nil {
			perTarget[item.Downloader] = map[string]int{}
		}
		perTarget[item.Downloader][state]++
	}
	if err := r.current(ctx); err != nil {
		return err
	}
	st := r.st
	switch r.trigger.Kind {
	case "once":
		st.Once = time.Time{}
	case "cron":
		loc, err := rssWorkerLocation()
		if err != nil {
			return err
		}
		st.Next, err = cookieCloudSchedule(r.s.cfg.Cron, loc, now)
		if err != nil {
			return err
		}
	default:
		st.Cursor = r.trigger.Row
	}
	st.Pending = ""
	st.Last = fmt.Sprintf("已扫描 %d 个任务，做种 %d 个，打标 %d 个，删除 %d 个，跳过 %d 项，失败 %d 项，待核实 %d 项。", b.Scanned, counts["seed"], counts["tag"], counts["remove"], counts["skipped"], counts["failed"], counts["pending"])
	if len(b.ByDownloader)+len(b.Unavailable) > 1 {
		for _, target := range r.s.targets {
			n, scanned := b.ByDownloader[target.Item.ID]
			if b.Unavailable[target.Item.ID] {
				st.Last += "\n" + target.Item.Name + "：无法读取任务。"
				continue
			}
			if scanned {
				c := perTarget[target.Item.ID]
				st.Last += fmt.Sprintf("\n%s：已扫描 %d 个任务，做种 %d 个，打标 %d 个，删除 %d 个。", target.Item.Name, n, c["seed"], c["tag"], c["remove"])
			}
		}
	}
	return r.api.transfer.system.Transaction(ctx, func(tx *sql.Tx) error {
		err := systemconfig.UpdateManyTx(ctx, tx, helperKeys, func(actual map[string]string) (map[string]string, error) {
			if !maps.Equal(actual, r.raw) {
				return nil, downloaderhelper.ErrSettings
			}
			data, err := json.Marshal(st)
			return map[string]string{helperControlKey: string(data)}, err
		})
		if err != nil {
			return err
		}
		if r.s.cfg.Notify && counts["seed"]+counts["tag"]+counts["remove"] > 0 {
			if err := bus.EmitData(ctx, tx, "plugin_message", r.id, r.id, "下载器助手任务执行结果", st.Last, map[string]string{"plugin": helperID}); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM GO_DOWNLOADER_HELPER_BATCH WHERE id=?", r.id)
		return err
	})
}
