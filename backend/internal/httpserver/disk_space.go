package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

const diskID = "DiskSpaceSaver"
const diskConfigKey = "plugin." + diskID
const diskControlKey = "GoDiskSpaceSaver"

var diskKeys = []string{"UserInstalledPlugins", diskConfigKey, diskControlKey}

type diskSpaceAPI struct {
	login      *siteLoginAPI
	jobs       *organization.JobLocks
	store      *systemconfig.Store
	configFile string
}
type diskOptions struct {
	Extensions, Minimum, Paths, Legacy string
	Once, Dry, Fast                    bool
}

func diskValues(raw string) (map[string]json.RawMessage, diskOptions, error) {
	v, err := helperValues(raw)
	var c diskOptions
	if err != nil {
		return nil, c, err
	}
	err = helperDecode(v, map[string]any{"ext_list": &c.Extensions, "path_list": &c.Paths, "run_now": &c.Once, "dry_run": &c.Dry, "fast": &c.Fast, "result_path": &c.Legacy})
	if err != nil {
		return nil, c, err
	}
	if b := v["file_size"]; b != nil {
		if json.Unmarshal(b, &c.Minimum) != nil {
			var n int64
			if json.Unmarshal(b, &n) != nil {
				return nil, c, organization.ErrState
			}
			c.Minimum = strconv.FormatInt(n, 10)
		}
	}
	return v, c, nil
}
func diskFields() []pluginConfigField {
	fields := []pluginConfigField{
		{Key: "path_list", Title: "磁盘目录", Type: "textarea", Tooltip: "每行一个绝对目录，每个目录内分别查重。"},
		{Key: "ext_list", Title: "文件后缀", Type: "text", Placeholder: ".mkv,.mp4"},
		{Key: "file_size", Title: "最小文件大小（MB）", Type: "text", Tooltip: "只检查达到该大小的文件。"},
		{Key: "dry_run", Title: "仅查重", Type: "switch", Tooltip: "显示候选文件并更新校验缓存，保留原文件。"},
		{Key: "fast", Title: "快速模式", Type: "switch", Tooltip: "先比较文件首段、中段和末段；替换前仍核对完整内容。"},
		{Key: "run_now", Title: "立即运行一次", Type: "switch", Tooltip: "保存后执行。关闭仅查重时，同一磁盘的重复文件将共享同一个硬链接目标。"},
	}
	for i := range fields {
		fields[i].Options = []pluginConfigChoice{}
	}
	return fields
}

type diskRun struct {
	ID, Actor, Scope, Status, Last string
	Roots                          []organization.DiskRoot
	Unavailable                    []string
}

func readDiskRun(raw string) (diskRun, error) {
	var s diskRun
	if raw == "" {
		return s, nil
	}
	err := json.Unmarshal([]byte(raw), &s)
	return s, err
}
func diskFilters(c diskOptions) ([]string, int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(c.Minimum), 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/(1<<20) {
		return nil, 0, organization.ErrState
	}
	var exts []string
	for _, part := range strings.Split(c.Extensions, ",") {
		ext := strings.ToLower(strings.TrimSpace(part))
		if ext == "" {
			continue
		}
		if len(ext) < 2 || !strings.HasPrefix(ext, ".") || strings.ContainsAny(ext, "/\\\n\r\x00") {
			return nil, 0, organization.ErrPath
		}
		if !slices.Contains(exts, ext) {
			exts = append(exts, ext)
		}
	}
	if len(exts) == 0 {
		return nil, 0, organization.ErrState
	}
	return exts, n * (1 << 20), nil
}
func (api *diskSpaceAPI) plugin() feedPluginDefinition {
	return feedPluginDefinition{diskID, "磁盘空间释放", diskControlKey, diskKeys, api.detail, func(_ context.Context, raw map[string]string) (bool, bool) {
		s, e := readDiskRun(raw[diskControlKey])
		return s.Status == "queued", e == nil
	}, api.configure}
}
func (api *diskSpaceAPI) detail(_ context.Context, actor string, raw map[string]string) (pluginConfigDetail, error) {
	_, c, err := diskValues(raw[diskConfigKey])
	return pluginConfigDetail{ID: diskID, Name: "磁盘空间释放", Fields: diskFields(), Values: map[string]any{"path_list": c.Paths, "ext_list": c.Extensions, "file_size": c.Minimum, "dry_run": c.Dry, "fast": c.Fast, "run_now": c.Once}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: sitelogin.Hash([]any{actor, raw})}}, err
}
func (api *diskSpaceAPI) configure(_ context.Context, actor string, raw map[string]string, in map[string]json.RawMessage, clear []string, _ time.Time) (map[string]string, error) {
	v, _, err := diskValues(raw[diskConfigKey])
	if err != nil || len(clear) > 0 {
		return nil, organization.ErrState
	}
	fields := diskFields()
	for k, b := range in {
		if !slices.ContainsFunc(fields, func(f pluginConfigField) bool { return f.Key == k }) {
			return nil, organization.ErrState
		}
		v[k] = b
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	_, c, err := diskValues(string(b))
	if err != nil {
		return nil, err
	}
	s, err := readDiskRun(raw[diskControlKey])
	if err != nil || s.Status == "queued" {
		return nil, organization.ErrBusy
	}
	if c.Once {
		if _, _, err = diskFilters(c); err != nil {
			return nil, err
		}
		auth, _, err := api.login.authority(actor)
		if err != nil {
			return nil, err
		}
		id, err := refreshNonce()
		if err != nil {
			return nil, err
		}
		s = diskRun{ID: id, Actor: actor, Status: "queued"}
		if err := freezeOneShotRoots(&s, c.Paths); err != nil {
			return nil, err
		}
		v["run_now"] = json.RawMessage("false")
		b, err = json.Marshal(v)
		if err != nil {
			return nil, err
		}
		s.Scope = sitelogin.Hash([]any{auth, string(b), s.ID, s.Roots, s.Unavailable})
	}
	st, err := json.Marshal(s)
	return map[string]string{diskControlKey: string(st), diskConfigKey: string(b)}, err
}
func (api *diskSpaceAPI) page(w http.ResponseWriter, r *http.Request, service pluginService, legacy bool) {
	if !service.historyReader(w, r) {
		return
	}
	ctx := r.Context()
	raw, err := api.store.ReadMany(ctx, diskKeys)
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	ok, err := indexerInstalled(raw["UserInstalledPlugins"], diskID)
	if err != nil || !ok {
		writeAPIError(w, 404, 404, "尚未安装磁盘空间释放")
		return
	}
	s, err := readDiskRun(raw[diskControlKey])
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	data := pluginPageData{Title: "磁盘空间释放", ReadOnly: true, Tables: []pluginPageTable{}, Sections: []string{}}
	if s.Status == "queued" {
		data.Sections = append(data.Sections, "本次查重任务正在等待或执行。")
	}
	if s.Last != "" {
		data.Sections = append(data.Sections, s.Last)
	}
	if s.ID != "" {
		data.Sections = append(data.Sections, "任务编号 "+s.ID)
		first := 0
		if cursor := r.FormValue("after"); cursor != "" {
			id, offset, ok := strings.Cut(cursor, ".")
			n, e := strconv.Atoi(offset)
			if !ok || id != s.ID || e != nil || n < 0 || strconv.Itoa(n) != offset {
				writeAPIError(w, 400, 400, "查重任务或分页位置已变化，请重新打开结果")
				return
			}
			first = n
		}
		table := pluginPageTable{Columns: []string{"保留文件", "重复文件", "处理结果"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
		err = api.store.Transaction(ctx, func(tx *sql.Tx) error {
			if err := diskTables(ctx, tx); err != nil {
				return err
			}
			rows, e := tx.QueryContext(ctx, "SELECT seq,pair,state,stage FROM GO_DISK_SPACE_PAIRS WHERE job=? AND seq>=? ORDER BY seq LIMIT 101", s.ID, first)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var v, st, stage string
				var p organization.DiskPair
				var seq int
				if err := rows.Scan(&seq, &v, &st, &stage); err != nil {
					return err
				}
				if len(table.Rows) == 100 {
					data.NextCursor = s.ID + "." + strconv.Itoa(seq)
					break
				}
				if err := json.Unmarshal([]byte(v), &p); err != nil {
					return err
				}
				note := map[string]string{"candidate": "仅查重", "pending": "待处理", "linked": "已替换", "different": "完整内容有差异", "failed": "文件已变化或无法替换"}[st]
				if st == "failed" && stage != "" {
					note += "，请核对暂存目录 " + stage
				}
				table.Rows = append(table.Rows, []string{filepath.Join(p.Root.Path, p.Keep.Path), filepath.Join(p.Root.Path, p.Duplicate.Path), note})
			}
			return rows.Err()
		})
		if err != nil {
			subscriptionHistoryError(w, err)
			return
		}
		if len(table.Rows) > 0 {
			data.Tables = append(data.Tables, table)
		}
	}
	if len(data.Sections) == 0 {
		data.Sections = append(data.Sections, "暂无查重任务。")
	}
	writeNativeRecordPage(w, data, legacy)
}

func diskTables(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_DISK_SPACE_RUNS(id TEXT PRIMARY KEY,files INTEGER NOT NULL,linked INTEGER NOT NULL,failed INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS GO_DISK_SPACE_PAIRS(job TEXT NOT NULL,seq INTEGER NOT NULL,pair TEXT NOT NULL,state TEXT NOT NULL,hold TEXT NOT NULL DEFAULT '',stage TEXT NOT NULL DEFAULT '',PRIMARY KEY(job,seq));
 CREATE INDEX IF NOT EXISTS GO_DISK_SPACE_NEXT ON GO_DISK_SPACE_PAIRS(job,state,seq)`)
	return err
}

func sameDiskState(a, b map[string]string) bool {
	ok, err := indexerInstalled(a["UserInstalledPlugins"], diskID)
	return err == nil && ok && a[diskConfigKey] == b[diskConfigKey] && a[diskControlKey] == b[diskControlKey]
}
func (api *diskSpaceAPI) tick(ctx context.Context, _ time.Time) error {
	raw, err := api.store.ReadMany(ctx, diskKeys)
	if err != nil {
		return err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], diskID)
	if err != nil || !installed {
		return err
	}
	s, err := readDiskRun(raw[diskControlKey])
	if err != nil {
		return err
	}
	_, cfg, err := diskValues(raw[diskConfigKey])
	if err != nil {
		return err
	}
	if s.Status != "queued" && !cfg.Once {
		return nil
	}
	guard, err := api.jobs.AcquireJob(ctx, diskID)
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	if s.Status != "queued" {
		settings, err := api.login.config.Snapshot()
		if err != nil {
			return err
		}
		return api.store.UpdateMany(ctx, diskKeys, func(live map[string]string) (map[string]string, error) {
			if !sameDiskState(live, raw) {
				return nil, organization.ErrState
			}
			return api.configure(ctx, text(objectValue(settings["app"])["login_user"]), raw, nil, nil, time.Now())
		})
	}
	current := func() error {
		live, err := api.store.ReadMany(ctx, diskKeys)
		if err != nil {
			return err
		}
		auth, _, err := api.login.authority(s.Actor)
		if err != nil {
			return err
		}
		if !sameDiskState(live, raw) || sitelogin.Hash([]any{auth, raw[diskConfigKey], s.ID, s.Roots, s.Unavailable}) != s.Scope {
			return organization.ErrState
		}
		return ctx.Err()
	}
	finish := func(last string) error {
		s.Last, s.Status = last, "finished"
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return api.store.UpdateMany(ctx, diskKeys, func(live map[string]string) (map[string]string, error) {
			if !sameDiskState(live, raw) {
				return nil, organization.ErrState
			}
			return map[string]string{diskControlKey: string(b)}, nil
		})
	}
	if err := current(); err != nil {
		if errors.Is(err, organization.ErrState) || errors.Is(err, sitelogin.ErrScope) {
			return finish("配置或管理员已变化，任务已停止。")
		}
		return err
	}
	last, err := api.execute(ctx, s, cfg, current)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errDiskCache) {
			return finish("无法读取或保存查重缓存，请核对插件目录和旧缓存文件。")
		}
		return err
	}
	return finish(last)
}

func freezeOneShotRoots(s *diskRun, paths string) error {
	seen := map[string]bool{}
	for _, line := range strings.Split(paths, "\n") {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return organization.ErrPath
		}
		root, e := organization.FreezeDiskRoot(path)
		if e != nil {
			s.Unavailable = append(s.Unavailable, path)
			continue
		}
		if !seen[root.Identity] {
			s.Roots = append(s.Roots, root)
			seen[root.Identity] = true
		}
	}
	if len(s.Roots)+len(s.Unavailable) == 0 {
		return organization.ErrPath
	}
	return nil
}
