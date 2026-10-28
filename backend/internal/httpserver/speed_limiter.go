package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type speedLimiterAPI struct {
	organizer *organizationAPI
	system    *systemconfig.Store
	queue     *speedlimit.Store
	transport http.RoundTripper
	workers   atomic.Int32
	accountMu sync.Mutex
	accounts  map[string]mediaRefreshAccountEntry
}

const speedLimiterGuard = "native-playback-speed-limiter"

var speedLimiterKeys = []string{"UserInstalledPlugins", speedlimit.ConfigKey, speedlimit.ControlKey}

func limiterConfigValues(raw string) (map[string]any, error) {
	v := map[string]any{}
	if raw == "" {
		return v, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if len(raw) > 64<<10 || !json.Valid([]byte(raw)) || d.Decode(&v) != nil || v == nil {
		return nil, speedlimit.ErrState
	}
	return v, nil
}
func (s pluginService) nativeSpeedLimiter(id string) bool {
	return s.speedLimiter != nil && strings.TrimSpace(id) == speedlimit.Plugin
}
func (api *speedLimiterAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.organizer == nil || api.system == nil {
		writeAPIError(w, 503, 503, "播放限速服务不可用")
		return "", false
	}
	if !api.organizer.jobsAuthorized(w, r) {
		return "", false
	}
	if api.queue == nil {
		writeAPIError(w, 503, 503, "播放限速回执不可用")
		return "", false
	}
	claims, err := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "需要当前登录")
		return "", false
	}
	if _, err := api.authority(claims.Username); err != nil {
		writeAPIError(w, 403, 403, "需要当前管理员")
		return "", false
	}
	return claims.Username, true
}
func (api *speedLimiterAPI) authority(actor string) (string, error) {
	return (&eventWebhookAPI{organizer: api.organizer}).authority(actor)
}
func (api *speedLimiterAPI) mediaSpec() (mediaRefreshConnection, error) {
	return (&mediaRefreshAPI{organizer: api.organizer, connection: mediaSyncAPI{transport: api.transport}}).spec()
}
func (api *speedLimiterAPI) validate(c speedlimit.Control) error {
	authority, err := api.authority(c.Actor)
	if err != nil || authority != c.Authority {
		return speedlimit.ErrState
	}
	media, err := api.mediaSpec()
	if err != nil || media.kind != c.Server || media.digest != c.MediaDigest || media.cloud && !c.PlexAccount {
		return speedlimit.ErrState
	}
	return nil
}
func (api *speedLimiterAPI) connectMedia(ctx context.Context, c speedlimit.Control) (*mediaserver.Client, error) {
	if err := api.validate(c); err != nil {
		return nil, err
	}
	spec, err := api.mediaSpec()
	if err != nil {
		return nil, err
	}
	if !spec.cloud {
		return spec.client, nil
	}
	api.accountMu.Lock()
	defer api.accountMu.Unlock()
	key := spec.digest + ":" + c.Epoch
	if entry, ok := api.accounts[key]; ok {
		return entry.client, entry.err
	}
	if len(api.accounts) >= 128 {
		return nil, speedlimit.ErrState
	}
	client, err := mediaserver.ResolvePlexAccount(ctx, spec.username, spec.password, spec.serverName, c.Epoch, c.PlexHTTP, api.transport)
	if api.accounts == nil {
		api.accounts = map[string]mediaRefreshAccountEntry{}
	}
	api.accounts[key] = mediaRefreshAccountEntry{client: client, err: err}
	return client, err
}
func (api *speedLimiterAPI) fingerprint(raw map[string]string, actor string) string {
	authority, err := api.authority(actor)
	if err != nil {
		authority = "unavailable"
	}
	media, err := api.mediaSpec()
	if err != nil {
		media.digest = "unavailable"
	}
	return bus.Hash([]any{raw, actor, authority, media.kind, media.digest})
}
func (api *speedLimiterAPI) detail(ctx context.Context, raw map[string]string, actor string) (pluginConfigDetail, error) {
	spec, err := speedlimit.DecodeSpec(raw[speedlimit.ConfigKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	values, err := limiterConfigValues(raw[speedlimit.ConfigKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	fields := []pluginConfigField{}
	for _, v := range []struct{ key, title, tip string }{
		{"upload_limit", "播放上传限速", "KB/s，0 为不限速。智能模式启用时由剩余上行带宽计算。"},
		{"download_limit", "播放下载限速", "KB/s，0 为不限速。"},
		{"upload_unlimit", "未播放上传限速", "KB/s，0 为不限速；全局设置会影响这个下载器内的所有任务。"},
		{"download_unlimit", "未播放下载限速", "KB/s，0 为不限速。"},
		{"bandwidth", "智能上传总带宽", "Mbps，沿用原 int(float(value)) 取整；0 使用固定上传值。"},
		{"allocation_ratio", "智能上传分配比例", "按所选下载器原顺序填写，例如 1:2；格式或数量不匹配沿用旧行为均分，审核显示实际权重。"},
		{"ipv4", "不限速 IPv4 来源", "逗号分隔地址或网段；空值沿用 0.0.0.0/0。"},
		{"ipv6", "不限速 IPv6 来源", "逗号分隔地址或网段；空值沿用 ::/0。两族均覆盖全部地址时不启用。"},
		{"interval", "定时检查间隔", "秒，留空 300；1 至 86400。回调只触发实时读取，Plex 保留三秒等待。"},
	} {
		configured := values[v.key] != nil && values[v.key] != ""
		fields = append(fields, pluginConfigField{Key: v.key, Title: v.title, Tooltip: v.tip, Type: "text", WriteOnly: true, Configured: configured, Options: []pluginConfigChoice{}})
	}
	choices, err := api.queue.Choices(ctx)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	options := []pluginConfigChoice{}
	known := map[string]bool{}
	for _, v := range choices {
		label := v.Name
		if !v.Enabled {
			label += "（未启用）"
		}
		options = append(options, pluginConfigChoice{Value: v.ID, Label: label})
		known[v.ID] = true
	}
	for _, id := range spec.Downloaders {
		if !known[id] {
			options = append(options, pluginConfigChoice{Value: id, Label: "原下载器 " + id + "（当前不可用）"})
		}
	}
	fields = append(fields, pluginConfigField{Key: "downloaders", Title: "限速下载器", Type: "multiselect", Options: options}, pluginConfigField{Key: "notify", Title: "限速状态通知", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "确认完成后按通知渠道的“自定义消息”开关发送。"})
	return pluginConfigDetail{ID: speedlimit.Plugin, Name: "下载器播放限速", Fields: fields, Values: map[string]any{"downloaders": spec.Downloaders, "notify": spec.Notify}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: api.fingerprint(raw, actor)}}, nil
}
func (s pluginService) speedLimiterSummary(ctx context.Context) (bool, bool) {
	if s.speedLimiter.queue == nil {
		return false, true
	}
	c, _, active, err := s.speedLimiter.queue.Scope(ctx)
	if err != nil {
		return false, false
	}
	return active && s.speedLimiter.validate(c) == nil, true
}
func speedLimiterError(w http.ResponseWriter, err error) {
	if errors.Is(err, speedlimit.ErrMissing) {
		writeAPIError(w, 404, 404, "未找到原限速审核或回执")
		return
	}
	writeAPIError(w, 409, 409, "原配置、会话或限速回执需要核对；没有重发设置，请先只读查回")
}
func (s pluginService) serveSpeedLimiterPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.speedLimiter
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	raw, err := s.system.ReadMany(r.Context(), speedLimiterKeys)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], speedlimit.Plugin)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	if action == "install" && installed {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
		return
	}
	if action != "install" && !installed {
		writeAPIError(w, 404, 404, "播放限速插件未安装")
		return
	}
	if action == "get" {
		detail, err := api.detail(r.Context(), raw, actor)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": detail})
		return
	}
	if action == "status" {
		active, known := s.speedLimiterSummary(r.Context())
		writeJSON(w, 200, map[string]any{"code": 0, "state": active, "stateKnown": known})
		return
	}
	if action == "page" {
		api.pluginPage(w, r)
		return
	}
	if action != "install" && action != "uninstall" && action != "save" && action != "legacy-save" {
		writeAPIError(w, 405, 405, "不支持此限速插件操作")
		return
	}
	values, err := limiterConfigValues(raw[speedlimit.ConfigKey])
	if err != nil && action != "uninstall" {
		speedLimiterError(w, err)
		return
	}
	if values == nil {
		values = map[string]any{}
	}
	fingerprint := api.fingerprint(raw, actor)
	if action == "save" {
		var in pluginConfigRequest
		if !decodeServiceRequest(w, r, &in, "无效限速设置") {
			return
		}
		if in.Fingerprint != fingerprint {
			speedLimiterError(w, speedlimit.ErrState)
			return
		}
		detail, err := api.detail(r.Context(), raw, actor)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		next, message := mergePluginConfig(values, detail.Fields, in)
		if message != "" {
			writeAPIError(w, 400, 400, "限速设置字段无效")
			return
		}
		values = next
	}
	if action == "legacy-save" {
		values, err = limiterConfigValues(r.PostForm.Get("config"))
		if err != nil || r.PostForm.Get("config") == "" {
			writeAPIError(w, 400, 400, "原限速配置无效")
			return
		}
	}
	encoded, _ := json.Marshal(values)
	if action != "uninstall" {
		if _, err := speedlimit.DecodeSpec(string(encoded)); err != nil {
			writeAPIError(w, 400, 400, "限速数值、地址范围、下载器或间隔无效")
			return
		}
	}
	epoch, err := refreshNonce()
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	authority, err := api.authority(actor)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	media, mediaErr := api.mediaSpec()
	if mediaErr != nil {
		media.kind, media.digest = "emby", bus.Hash("unavailable media")
	}
	c := speedlimit.Control{Epoch: epoch, Actor: actor, Authority: authority, ConfigDigest: speedlimit.RawConfigDigest(string(encoded)), MediaDigest: media.digest, Server: media.kind, Created: time.Now().UTC().Format(time.RFC3339Nano), Baselines: []speedlimit.Baseline{}}
	if c.ConfigDigest == "" {
		c.ConfigDigest = bus.Hash("preserved invalid configuration")
	}
	controlRaw, _ := json.Marshal(c)
	err = s.system.UpdateMany(r.Context(), speedLimiterKeys, func(current map[string]string) (map[string]string, error) {
		if bus.Hash(current) != bus.Hash(raw) || api.fingerprint(current, actor) != fingerprint {
			return nil, speedlimit.ErrState
		}
		ids, err := decodeInstalledPlugins(current["UserInstalledPlugins"])
		if err != nil {
			return nil, err
		}
		if action == "install" {
			ids = append(ids, speedlimit.Plugin)
		}
		if action == "uninstall" {
			kept := []string{}
			for _, id := range ids {
				if id != speedlimit.Plugin {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		all, _ := json.Marshal(ids)
		out := map[string]string{"UserInstalledPlugins": string(all), speedlimit.ControlKey: string(controlRaw)}
		if action != "uninstall" {
			out[speedlimit.ConfigKey] = string(encoded)
		}
		return out, nil
	})
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "设置已保存，自动限速保持停止；请在限速管理区审核并单独启用"})
}

type speedLimiterStatus struct {
	Installed     bool                 `json:"installed"`
	Active        bool                 `json:"active"`
	Current       bool                 `json:"current"`
	Server        string               `json:"server"`
	Cloud         bool                 `json:"cloud"`
	WorkerRunning bool                 `json:"workerRunning"`
	Fingerprint   string               `json:"fingerprint"`
	Basis         string               `json:"-"`
	Spec          speedlimit.Spec      `json:"spec"`
	Reason        string               `json:"reason"`
	Targets       []speedLimiterTarget `json:"targets"`
}
type speedLimiterTarget struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Weight int64  `json:"weight"`
}

func (api *speedLimiterAPI) status(ctx context.Context, actor string) (speedLimiterStatus, error) {
	v := speedLimiterStatus{WorkerRunning: api.workers.Load() > 0}
	raw, err := api.system.ReadMany(ctx, speedLimiterKeys)
	if err != nil {
		return v, err
	}
	v.Installed, err = indexerInstalled(raw["UserInstalledPlugins"], speedlimit.Plugin)
	if err != nil {
		return v, err
	}
	v.Spec, err = speedlimit.DecodeSpec(raw[speedlimit.ConfigKey])
	if err != nil {
		v.Reason = "原限速配置需要修正"
	}
	v.Basis = bus.Hash(raw)
	v.Fingerprint = api.fingerprint(raw, actor)
	v.Targets = []speedLimiterTarget{}
	targets, targetErr := api.queue.Targets(ctx, v.Spec)
	if targetErr == nil {
		for _, t := range targets {
			v.Targets = append(v.Targets, speedLimiterTarget{t.ID, t.Name, t.Kind, t.Weight})
		}
	} else {
		v.Reason = "原下载器配置不可用"
	}
	v.Fingerprint = bus.Hash([]any{v.Fingerprint, targets})
	media, mediaErr := api.mediaSpec()
	v.Server, v.Cloud = media.kind, media.cloud
	c, _, active, scopeErr := api.queue.Scope(ctx)
	v.Current = err == nil && mediaErr == nil && scopeErr == nil && targetErr == nil
	v.Active = v.Current && active && api.validate(c) == nil
	if mediaErr != nil {
		v.Reason = "原媒体服务器连接配置不可用"
	} else if scopeErr != nil || active && !v.Active {
		v.Reason = "原限速授权或下载器配置已变化"
	}
	return v, nil
}
func plainHTTP(host string) bool {
	if !strings.Contains(host, "://") {
		return true
	}
	u, err := url.Parse(host)
	return err == nil && u.Scheme == "http"
}
func (api *speedLimiterAPI) needsHTTP(ctx context.Context, media mediaRefreshConnection, targets []speedlimit.Target) (bool, error) {
	snapshot, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		return false, err
	}
	required := !media.cloud && plainHTTP(text(objectValue(snapshot[media.kind])["host"]))
	for _, t := range targets {
		raw, err := api.queue.TargetConfig(ctx, t)
		if err != nil {
			return false, err
		}
		var config map[string]any
		if json.Unmarshal([]byte(raw), &config) != nil {
			return false, speedlimit.ErrState
		}
		required = required || plainHTTP(text(config["host"]))
	}
	return required, nil
}
func limiterReady(spec speedlimit.Spec) bool {
	return len(spec.Downloaders) > 0 && (spec.Download > 0 || spec.Upload > 0 || spec.Bandwidth > 0) && !(strings.Contains(spec.IPv4, "0.0.0.0/0") && strings.Contains(spec.IPv6, "::/0"))
}
func (api *speedLimiterAPI) manage(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	base := "/api/v1/speed-limiter"
	if r.URL.Path == base && r.Method == "GET" {
		v, err := api.status(r.Context(), actor)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.URL.Path == base+"/review" && r.Method == "POST" {
		api.review(w, r, actor)
		return
	}
	if r.URL.Path == base+"/authorize" && r.Method == "POST" {
		api.activate(w, r, actor)
		return
	}
	if r.URL.Path == base+"/stop" && r.Method == "POST" {
		var in struct {
			Fingerprint string `json:"fingerprint"`
			Confirm     bool   `json:"confirm"`
		}
		if !decodeOrganizationRequest(w, r, &in, "无效停止审核") {
			return
		}
		v, err := api.status(r.Context(), actor)
		if err != nil || !in.Confirm || v.Fingerprint != in.Fingerprint {
			speedLimiterError(w, speedlimit.ErrState)
			return
		}
		authority, err := api.authority(actor)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		err = api.queue.Stop(r.Context(), v.Basis, func() error {
			current, err := api.authority(actor)
			if err != nil || current != authority {
				return speedlimit.ErrState
			}
			return nil
		})
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		v, err = api.status(r.Context(), actor)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.URL.Path == base+"/jobs" && r.Method == "GET" {
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil {
				speedLimiterError(w, speedlimit.ErrState)
				return
			}
			page = n
		}
		items, err := api.queue.Jobs(r.Context(), page)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": items})
		return
	}
	if r.URL.Path == base+"/triggers" && r.Method == "GET" {
		items, err := api.queue.Triggers(r.Context(), false)
		if err != nil {
			speedLimiterError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": items})
		return
	}
	if strings.HasPrefix(r.URL.Path, base+"/jobs/") {
		api.jobAction(w, r, actor)
		return
	}
	writeAPIError(w, 404, 404, "未知限速操作")
}
func (api *speedLimiterAPI) pluginPage(w http.ResponseWriter, r *http.Request) {
	jobs, err := api.queue.Jobs(r.Context(), 1)
	if err != nil {
		speedLimiterError(w, err)
		return
	}
	page := pluginPageData{Title: "下载器播放限速回执", ReadOnly: true, Sections: []string{"设置、启停、原编号查询、只读核对和明确忽略位于下方独立限速管理区。", "播放回调只触发读取原媒体服务器的当前会话，不直接信任回调中的播放人数或带宽。", "停用不恢复或撤回已经写入的限速；未知设置不重发，忽略会同时停止当前自动限速。"}, Tables: []pluginPageTable{{Columns: []string{"稳定编号", "来源", "原引用", "状态", "设置数"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}}}
	for _, j := range jobs {
		page.Tables[0].Rows = append(page.Tables[0].Rows, []string{j.ID, j.Source, j.Reference, j.State, strconv.Itoa(len(j.Operations))})
		page.Tables[0].RowActions = append(page.Tables[0].RowActions, nil)
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": page})
}
func (api *speedLimiterAPI) reservePlugin(next http.Handler) http.Handler {
	const base = "/api/v1/plugins/SpeedLimiter"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == base || strings.HasPrefix(p, base+"/") {
			allowed := p == base && (r.Method == "GET" || r.Method == "PUT" || r.Method == "DELETE") || p == base+"/install" && r.Method == "POST" || p == base+"/page" && r.Method == "GET"
			if !allowed {
				if _, ok := api.authorize(w, r); !ok {
					return
				}
				writeAPIError(w, 405, 405, "不支持此原生限速插件操作")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
