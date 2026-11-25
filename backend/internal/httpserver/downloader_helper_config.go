package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/downloaderhelper"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

const helperID = downloaderhelper.Plugin
const helperConfigKey = "plugin." + helperID
const helperControlKey = downloaderhelper.ControlKey

var helperKeys = []string{"UserInstalledPlugins", helperConfigKey, helperControlKey}

type downloaderHelperAPI struct {
	transfer  *torrentTransferAPI
	downloads *downloadService
}
type helperMode struct{ Enable, Seed, Tag, Delete bool }
type helperSettings struct {
	Enable, Notify, Once, OnDownload, OnSource, UseName bool
	Cron, Exclude                                       string
}

func helperValues(raw string) (map[string]json.RawMessage, error) {
	v := map[string]json.RawMessage{}
	if raw != "" && (json.Unmarshal([]byte(raw), &v) != nil || v == nil) {
		return nil, downloaderhelper.ErrSettings
	}
	return v, nil
}
func helperDecode(v map[string]json.RawMessage, fields map[string]any) error {
	for k, dst := range fields {
		if b := v[k]; b != nil && json.Unmarshal(b, dst) != nil {
			return downloaderhelper.ErrSettings
		}
	}
	return nil
}
func (c *helperSettings) fields() map[string]any {
	return map[string]any{"enable": &c.Enable, "run_once": &c.Once, "enable_notify": &c.Notify, "listen_download_event": &c.OnDownload, "listen_source_file_event": &c.OnSource, "use_site_config": &c.UseName, "cron": &c.Cron, "exclude_tags": &c.Exclude}
}

type helperTarget struct {
	Item downloaderconfig.Downloader
	Mode helperMode
}
type helperScope struct {
	cfg     helperSettings
	targets []helperTarget
	sites   map[string]string
	exclude []string
	proof   string
}

func (api *downloaderHelperAPI) selection(ctx context.Context, actor string, v map[string]json.RawMessage) (helperScope, error) {
	s := helperScope{sites: map[string]string{}}
	auth, _, err := api.transfer.login.authority(actor)
	if err != nil {
		return s, err
	}
	if err := helperDecode(v, s.cfg.fields()); err != nil {
		return s, err
	}
	for _, tag := range strings.Split(s.cfg.Exclude, ",") {
		if x := strings.TrimSpace(tag); x != "" {
			s.exclude = append(s.exclude, x)
		}
	}
	ds, err := api.transfer.downloaders.List(ctx)
	if err != nil {
		return s, err
	}
	for _, d := range ds {
		if d.Enabled != 1 || d.Type != "transmission" && d.Type != "qbittorrent" {
			continue
		}
		p := "downloader." + strconv.FormatInt(d.ID, 10) + "."
		var m helperMode
		if err := helperDecode(v, map[string]any{p + "enable": &m.Enable, p + "enable_seeding": &m.Seed, p + "enable_tagging": &m.Tag, p + "enable_delete": &m.Delete}); err != nil {
			return s, err
		}
		if m.Enable && (m.Seed || m.Tag || m.Delete) {
			s.targets = append(s.targets, helperTarget{d, m})
		}
	}
	ss, err := api.transfer.login.sites.List(ctx)
	if err != nil {
		return s, err
	}
	for _, site := range ss {
		if key := downloaderhelper.Keyword(site.SignURL); key != "" {
			s.sites[key] = site.Name
		}
	}
	s.proof = sitelogin.Hash([]any{auth, v, s.targets, s.sites})
	return s, nil
}
func (api *downloaderHelperAPI) fields(ctx context.Context) ([]pluginConfigField, error) {
	fields := []pluginConfigField{}
	add := func(key, title, kind, section, help string) {
		fields = append(fields, pluginConfigField{Key: key, Title: title, Type: kind, Section: section, Tooltip: help, Options: []pluginConfigChoice{}})
	}
	add("enable", "启动插件", "switch", "", "启用定时任务和配置的事件监听。")
	add("run_once", "立即运行一次", "switch", "", "保存后三秒执行一次，独立于总开关。")
	add("enable_notify", "运行时通知", "switch", "", "")
	add("cron", "定时执行周期", "text", "", "五段 cron 表达式。")
	add("exclude_tags", "排除种子标签", "text", "", "多个标签用英文逗号分隔，按完整标签匹配。")
	add("use_site_config", "优先使用站点名称", "switch", "", "")
	add("listen_download_event", "监听下载添加事件", "switch", "", "对相应下载器执行站点打标。")
	add("listen_source_file_event", "监听源文件删除事件", "switch", "", "按下载器目录映射核对已经删除的源数据。")
	ds, err := api.transfer.downloaders.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.Enabled != 1 || d.Type != "transmission" && d.Type != "qbittorrent" {
			continue
		}
		p := "downloader." + strconv.FormatInt(d.ID, 10) + "."
		section := d.Name + " · " + strconv.FormatInt(d.ID, 10)
		add(p+"enable", "任务开关", "switch", section, "")
		add(p+"enable_seeding", "自动做种", "switch", section, "启动已经完成且暂停的任务。")
		add(p+"enable_tagging", "站点标签", "switch", section, "")
		add(p+"enable_delete", "自动删种", "switch", section, "下载器报告数据丢失时，移除任务及其剩余下载文件。")
	}
	return fields, nil
}
func (api *downloaderHelperAPI) detail(ctx context.Context, actor string, raw map[string]string) (pluginConfigDetail, error) {
	fields, err := api.fields(ctx)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	v, err := helperValues(raw[helperConfigKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	values := map[string]any{}
	for _, f := range fields {
		if f.Type == "switch" {
			var x bool
			err = helperDecode(v, map[string]any{f.Key: &x})
			values[f.Key] = x
		} else {
			var x string
			err = helperDecode(v, map[string]any{f.Key: &x})
			values[f.Key] = x
		}
		if err != nil {
			return pluginConfigDetail{}, err
		}
	}
	return pluginConfigDetail{ID: helperID, Name: "下载器助手", Fields: fields, Values: values, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: sitelogin.Hash([]any{actor, raw})}}, nil
}

type helperSchedule struct {
	Download, Source             bool
	Actor, Scope, Revision, Last string
	Next, Once, Since            time.Time
	Cursor                       int64
	Pending                      string
}

func helperState(raw string) (helperSchedule, error) {
	var st helperSchedule
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return st, err
		}
	}
	return st, nil
}
func (api *downloaderHelperAPI) configure(ctx context.Context, actor string, raw map[string]string, in map[string]json.RawMessage, clear []string, now time.Time) (map[string]string, error) {
	v, err := helperValues(raw[helperConfigKey])
	if err != nil || len(clear) > 0 {
		return nil, downloaderhelper.ErrSettings
	}
	fields, err := api.fields(ctx)
	if err != nil {
		return nil, err
	}
	for k, b := range in {
		if !slices.ContainsFunc(fields, func(f pluginConfigField) bool { return f.Key == k }) {
			return nil, downloaderhelper.ErrSettings
		}
		v[k] = b
	}
	var cfg helperSettings
	if err := helperDecode(v, cfg.fields()); err != nil {
		return nil, err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, err
	}
	next, err := cookieCloudSchedule(cfg.Cron, loc, now)
	if err != nil {
		return nil, err
	}
	if cfg.Once {
		v["run_once"] = json.RawMessage("false")
	}
	s, err := api.selection(ctx, actor, v)
	if err != nil {
		return nil, err
	}
	old, err := helperState(raw[helperControlKey])
	if err != nil {
		return nil, err
	}
	st := helperSchedule{Last: old.Last}
	if len(s.targets) > 0 {
		st.Actor, st.Scope = actor, s.proof
		st.Revision, err = refreshNonce()
		if err != nil {
			return nil, err
		}
		if cfg.Enable {
			st.Next = next.UTC()
			if cfg.OnDownload || cfg.OnSource {
				st.Since = now.UTC()
				st.Download, st.Source = cfg.OnDownload, cfg.OnSource
			}
		}
		if cfg.Once {
			st.Once = now.Add(3 * time.Second).UTC()
		}
	} else if cfg.Once {
		return nil, downloaderhelper.ErrSettings
	}
	control, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	return map[string]string{helperControlKey: string(control), helperConfigKey: string(b)}, err
}
func (api *downloaderHelperAPI) plugin() feedPluginDefinition {
	return feedPluginDefinition{helperID, "下载器助手", helperControlKey, helperKeys, api.detail, api.summary, api.configure}
}
func (api *downloaderHelperAPI) summary(ctx context.Context, raw map[string]string) (bool, bool) {
	st, err := helperState(raw[helperControlKey])
	if err != nil {
		return false, false
	}
	if st.Next.IsZero() && st.Once.IsZero() && st.Since.IsZero() && st.Pending == "" {
		return false, true
	}
	v, err := helperValues(raw[helperConfigKey])
	if err != nil {
		return false, false
	}
	s, err := api.selection(ctx, st.Actor, v)
	return err == nil && s.proof == st.Scope, err == nil
}
func (service pluginService) helperPage(w http.ResponseWriter, r *http.Request, legacy bool) {
	if !service.historyReader(w, r) {
		return
	}
	raw, err := service.system.ReadMany(r.Context(), helperKeys)
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	ok, err := indexerInstalled(raw["UserInstalledPlugins"], helperID)
	if err != nil || !ok {
		writeAPIError(w, 404, 404, "尚未安装下载器助手")
		return
	}
	st, err := helperState(raw[helperControlKey])
	if err != nil {
		writeAPIError(w, 409, 409, "任务记录无效")
		return
	}
	data := pluginPageData{Title: "下载器助手", ReadOnly: true, Tables: []pluginPageTable{}, Sections: []string{"每个下载器可分别配置做种、站点标签和缺失数据清理。完整匹配排除标签的种子会跳过。"}}
	if !st.Once.IsZero() {
		data.Sections = append(data.Sections, "单次运行时间 "+st.Once.Local().Format(time.DateTime))
	}
	if !st.Next.IsZero() {
		data.Sections = append(data.Sections, "下次定时运行 "+st.Next.Local().Format(time.DateTime))
	}
	if st.Last != "" {
		data.Sections = append(data.Sections, st.Last)
	}
	table := pluginPageTable{Columns: []string{"待核实任务", "下载器编号", "操作"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	err = service.system.Transaction(r.Context(), func(tx *sql.Tx) error {
		var exists bool
		if e := tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_DOWNLOADER_HELPER_ACTION')").Scan(&exists); e != nil || !exists {
			return e
		}
		rows, e := tx.QueryContext(r.Context(), "SELECT desired FROM GO_DOWNLOADER_HELPER_ACTION WHERE state='pending' ORDER BY rowid DESC LIMIT 100")
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var doc string
			var item helperItem
			if e := rows.Scan(&doc); e != nil {
				return e
			}
			if e := json.Unmarshal([]byte(doc), &item); e != nil {
				return e
			}
			table.Rows = append(table.Rows, []string{item.Task.Name, strconv.FormatInt(item.Downloader, 10), map[string]string{"seed": "做种", "tag": "站点标签", "remove": "删除"}[item.Action]})
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
	writeNativeRecordPage(w, data, legacy)
}
func (api *downloaderHelperAPI) importSettings(ctx context.Context, raw map[string]string, now time.Time) error {
	cfg, err := api.transfer.login.config.Snapshot()
	if err != nil {
		return err
	}
	changes, err := api.configure(ctx, text(objectValue(cfg["app"])["login_user"]), raw, nil, nil, now)
	if err != nil {
		return err
	}
	return api.transfer.system.UpdateMany(ctx, helperKeys, func(actual map[string]string) (map[string]string, error) {
		if !maps.Equal(actual, raw) {
			return nil, downloaderhelper.ErrSettings
		}
		return changes, nil
	})
}
