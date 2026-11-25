package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/autosub"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

const autoSubID = "AutoSub"
const autoSubConfig = "plugin." + autoSubID
const autoSubControl = "GoAutoSub"

var autoSubKeys = []string{"UserInstalledPlugins", autoSubConfig, autoSubControl}

type autoSubAPI struct {
	login     *siteLoginAPI
	jobs      *organization.Store
	store     *systemconfig.Store
	transport http.RoundTripper
}
type autoSubSettings struct {
	autosub.Options
	Paths, Minimum string
	Once, Notify   bool
}

func (c *autoSubSettings) fields() map[string]any {
	return map[string]any{
		"path_list": &c.Paths, "file_size": &c.Minimum,
		"whisper_main": &c.Whisper.Binary, "whisper_model": &c.Whisper.Model,
		"additional_args": &c.Whisper.Args, "asr_engine": &c.Engine,
		"faster_whisper_model": &c.Faster.Model, "faster_whisper_model_path": &c.Faster.Directory,
		"translate_only": &c.ExtractOnly, "translate_zh": &c.Translate,
		"run_now": &c.Once, "send_notify": &c.Notify,
	}
}
func autoSubValues(v map[string]json.RawMessage) (autoSubSettings, error) {
	c := autoSubSettings{Options: autosub.Options{Engine: "whisper.cpp", Whisper: autosub.Whisper{Args: "-t 4 -p 1"}, Faster: autosub.FasterWhisper{Model: "base"}}}
	fields := c.fields()
	if b := v["file_size"]; len(b) > 0 && b[0] != '"' && string(b) != "null" {
		var n int64
		if err := json.Unmarshal(b, &n); err != nil {
			return c, err
		}
		c.Minimum = strconv.FormatInt(n, 10)
		delete(fields, "file_size")
	}
	err := helperDecode(v, fields)
	return c, err
}
func autoSubFields() []pluginConfigField {
	models := []pluginConfigChoice{}
	for _, name := range autosub.FasterModels() {
		models = append(models, pluginConfigChoice{name, name})
	}
	fs := []pluginConfigField{
		{Key: "path_list", Title: "媒体目录", Type: "textarea", Tooltip: "每行一个绝对目录。生成的字幕保存到媒体文件旁。"},
		{Key: "file_size", Title: "最小文件大小（字节）", Type: "text", Tooltip: "沿用旧任务实际按字节筛选的数值。"},
		{Key: "translate_zh", Title: "翻译为中文", Type: "switch", Tooltip: "将字幕文本发送到已配置的 OpenAI 翻译服务，生成中外文双语 SRT。"},
		{Key: "translate_only", Title: "仅使用已有字幕", Type: "switch", Tooltip: "读取外挂字幕或提取内嵌文本字幕，跳过语音识别。"},
		{Key: "asr_engine", Title: "语音识别引擎", Type: "select", Options: []pluginConfigChoice{{"whisper.cpp", "whisper.cpp"}, {"faster-whisper", "faster-whisper（CTranslate2）"}}},
		{Key: "whisper_main", Title: "whisper.cpp 程序路径", Type: "text", Tooltip: "使用服务端 whisper-cli 或 NASTOOL_WHISPER_CLI 指定的程序。"},
		{Key: "whisper_model", Title: "whisper.cpp 模型文件", Type: "text"},
		{Key: "additional_args", Title: "whisper.cpp 识别参数", Type: "text"},
		{Key: "faster_whisper_model", Title: "faster-whisper 模型名称", Type: "select", Options: models},
		{Key: "faster_whisper_model_path", Title: "faster-whisper 模型目录", Type: "text", Tooltip: "支持原缓存和解压后的 CTranslate2 模型。首次缺少模型时从 Hugging Face 下载并校验。语音在本机识别。"},
		{Key: "send_notify", Title: "发送处理通知", Type: "switch"},
		{Key: "run_now", Title: "保存后运行一次", Type: "switch"},
	}
	for i := range fs {
		if fs[i].Options == nil {
			fs[i].Options = []pluginConfigChoice{}
		}
	}
	return fs
}
func (a *autoSubAPI) plugin() feedPluginDefinition {
	return feedPluginDefinition{autoSubID, "AI 字幕自动生成", autoSubControl, autoSubKeys, a.detail, func(_ context.Context, raw map[string]string) (bool, bool) {
		s, e := readDiskRun(raw[autoSubControl])
		return s.Status == "queued", e == nil
	}, a.configure}
}
func (a *autoSubAPI) detail(_ context.Context, actor string, raw map[string]string) (pluginConfigDetail, error) {
	v, err := helperValues(raw[autoSubConfig])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	c, err := autoSubValues(v)
	return pluginConfigDetail{ID: autoSubID, Name: "AI 字幕自动生成", Fields: autoSubFields(), Values: c.fields(), Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: sitelogin.Hash([]any{actor, raw})}}, err
}
func (a *autoSubAPI) scope(s diskRun, raw string) (string, map[string]any, error) {
	auth, config, err := a.login.authority(s.Actor)
	if err != nil {
		return "", nil, err
	}
	return sitelogin.Hash([]any{auth, raw, s.ID, s.Roots, s.Unavailable, config["openai"], objectValue(config["app"])["proxies"]}), config, nil
}
func (a *autoSubAPI) configure(_ context.Context, actor string, raw map[string]string, in map[string]json.RawMessage, clear []string, _ time.Time) (map[string]string, error) {
	v, err := helperValues(raw[autoSubConfig])
	if err != nil || len(clear) > 0 {
		return nil, organization.ErrState
	}
	var opt autoSubSettings
	known := opt.fields()
	for k, b := range in {
		if _, ok := known[k]; !ok {
			return nil, organization.ErrState
		}
		v[k] = b
	}
	c, err := autoSubValues(v)
	if err != nil {
		return nil, err
	}
	s, err := readDiskRun(raw[autoSubControl])
	if err != nil || s.Status == "queued" {
		return nil, organization.ErrBusy
	}
	if c.Once {
		if n, e := strconv.ParseInt(c.Minimum, 10, 64); e != nil || n < 0 {
			return nil, organization.ErrState
		}
		if !slices.Contains([]string{"whisper.cpp", "faster-whisper"}, c.Engine) {
			return nil, organization.ErrState
		}
		id, err := refreshNonce()
		if err != nil {
			return nil, err
		}
		s = diskRun{ID: id, Actor: actor, Status: "queued"}
		if err = freezeOneShotRoots(&s, c.Paths); err != nil {
			return nil, err
		}
		v["run_now"] = json.RawMessage("false")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if c.Once {
		s.Scope, _, err = a.scope(s, string(b))
		if err != nil {
			return nil, err
		}
	}
	state, err := json.Marshal(s)
	return map[string]string{autoSubControl: string(state), autoSubConfig: string(b)}, err
}
func (a *autoSubAPI) tick(ctx context.Context, _ time.Time) error {
	raw, err := a.store.ReadMany(ctx, autoSubKeys)
	if err != nil {
		return err
	}
	ok, err := indexerInstalled(raw["UserInstalledPlugins"], autoSubID)
	if err != nil || !ok {
		return err
	}
	s, err := readDiskRun(raw[autoSubControl])
	if err != nil {
		return err
	}
	v, err := helperValues(raw[autoSubConfig])
	if err != nil {
		return err
	}
	cfg, err := autoSubValues(v)
	if err != nil {
		return err
	}
	if s.Status != "queued" && !cfg.Once {
		return nil
	}
	lock, err := a.jobs.AcquireJob(ctx, autoSubID)
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	update := func(changes map[string]string) error {
		return a.store.UpdateMany(ctx, autoSubKeys, func(live map[string]string) (map[string]string, error) {
			if !maps.Equal(live, raw) {
				return nil, organization.ErrState
			}
			return changes, nil
		})
	}
	if s.Status != "queued" {
		settings, e := a.login.config.Snapshot()
		if e != nil {
			return e
		}
		changes, e := a.configure(ctx, text(objectValue(settings["app"])["login_user"]), raw, nil, nil, time.Now())
		if e != nil {
			return e
		}
		return update(changes)
	}
	current := func() error {
		live, e := a.store.ReadMany(ctx, autoSubKeys)
		if e != nil {
			return e
		}
		scope, _, e := a.scope(s, raw[autoSubConfig])
		if e != nil {
			return e
		}
		if !maps.Equal(live, raw) || scope != s.Scope {
			return organization.ErrState
		}
		return ctx.Err()
	}
	last := "配置或管理员已变化，任务已停止。"
	if err = current(); err == nil {
		scope, configuration, e := a.scope(s, raw[autoSubConfig])
		if e != nil {
			return e
		}
		if scope != s.Scope {
			return organization.ErrState
		}
		last, err = a.execute(ctx, s, cfg, configuration, current)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, organization.ErrState) && !errors.Is(err, sitelogin.ErrScope) {
			return err
		}
		last = "配置或执行范围已变化，任务已停止。"
	}
	s.Status, s.Last = "finished", last
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	return update(map[string]string{autoSubControl: string(b)})
}

func (api *autoSubAPI) page(w http.ResponseWriter, r *http.Request, service pluginService, legacy bool) {
	if !service.historyReader(w, r) {
		return
	}
	ctx := r.Context()
	raw, err := api.store.ReadMany(ctx, autoSubKeys)
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	ok, err := indexerInstalled(raw["UserInstalledPlugins"], autoSubID)
	if err != nil || !ok {
		writeAPIError(w, 404, 404, "尚未安装自动字幕")
		return
	}
	s, err := readDiskRun(raw[autoSubControl])
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	data := pluginPageData{Title: "自动字幕", ReadOnly: true, Tables: []pluginPageTable{}, Sections: []string{}}
	if s.Status == "queued" {
		data.Sections = append(data.Sections, "本次字幕任务正在等待或执行。")
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
				writeAPIError(w, 400, 400, "字幕任务或分页位置已变化，请重新打开结果")
				return
			}
			first = n
		}
		table := pluginPageTable{Columns: []string{"媒体文件", "处理结果"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
		err = api.store.Transaction(ctx, func(tx *sql.Tx) error {
			if err := autoSubTables(ctx, tx); err != nil {
				return err
			}
			rows, e := tx.QueryContext(ctx, "SELECT seq,path,note FROM GO_AUTOSUB_FILES WHERE job=? AND seq>=? ORDER BY seq LIMIT 101", s.ID, first)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var path, note string
				var seq int
				if err := rows.Scan(&seq, &path, &note); err != nil {
					return err
				}
				if len(table.Rows) == 100 {
					data.NextCursor = s.ID + "." + strconv.Itoa(seq)
					break
				}
				table.Rows = append(table.Rows, []string{path, note})
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
		data.Sections = append(data.Sections, "暂无字幕任务。")
	}
	writeNativeRecordPage(w, data, legacy)
}
