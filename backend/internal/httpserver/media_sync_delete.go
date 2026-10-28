package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type mediaSyncDeleteAPI struct {
	organizer *organizationAPI
	system    *systemconfig.Store
	queue     *mediasyncdel.Store
	transport http.RoundTripper
	workers   atomic.Int32
}

const mediaSyncDeleteGuard = "native-media-sync-delete"

var mediaSyncDeleteKeys = []string{"UserInstalledPlugins", mediasyncdel.ConfigKey, mediasyncdel.ControlKey}

func (s pluginService) nativeMediaSyncDelete(id string) bool {
	return s.mediaSyncDelete != nil && strings.TrimSpace(id) == mediasyncdel.Plugin
}
func (api *mediaSyncDeleteAPI) authority(actor string) (string, error) {
	return (&eventWebhookAPI{organizer: api.organizer}).authority(actor)
}
func (api *mediaSyncDeleteAPI) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.organizer == nil || api.system == nil {
		writeAPIError(w, 503, 503, "媒体同步删除不可用")
		return "", false
	}
	if !api.organizer.jobsAuthorized(w, r) {
		return "", false
	}
	if api.queue == nil {
		writeAPIError(w, 503, 503, "同步删除回执不可用")
		return "", false
	}
	c, err := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "需要当前登录")
		return "", false
	}
	if _, err := api.authority(c.Username); err != nil {
		writeAPIError(w, 403, 403, "需要当前管理员")
		return "", false
	}
	return c.Username, true
}
func (api *mediaSyncDeleteAPI) appDigest() (string, error) {
	v, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	return bus.Hash(v), nil
}
func (api *mediaSyncDeleteAPI) client() (*mediaserver.Client, error) {
	s, err := (&mediaRefreshAPI{organizer: api.organizer, connection: mediaSyncAPI{transport: api.transport}}).spec()
	if err != nil || s.kind != "emby" || s.cloud || s.client == nil {
		return nil, mediasyncdel.ErrState
	}
	return s.client, nil
}
func (api *mediaSyncDeleteAPI) validate(c mediasyncdel.Control) error {
	authority, err := api.authority(c.Actor)
	if err != nil || authority != c.Authority {
		return mediasyncdel.ErrState
	}
	digest, err := api.appDigest()
	if err != nil || digest != c.AppDigest {
		return mediasyncdel.ErrState
	}
	if _, err := api.client(); err != nil {
		return err
	}
	roots, err := api.organizer.roots(context.Background())
	if err != nil {
		return err
	}
	for _, selected := range c.Roots {
		found := false
		for _, root := range roots.Sources {
			if root.ID != selected.ID {
				continue
			}
			current, err := libraryRootSnapshot(root)
			if err != nil || current.Path != selected.Path || current.Identity != selected.Identity {
				return mediasyncdel.ErrState
			}
			found = true
		}
		if !found {
			return mediasyncdel.ErrState
		}
	}
	return nil
}
func (api *mediaSyncDeleteAPI) fingerprint(raw map[string]string, actor string) string {
	authority, err := api.authority(actor)
	if err != nil {
		authority = "unavailable"
	}
	app, err := api.appDigest()
	if err != nil {
		app = "unavailable"
	}
	return bus.Hash([]any{raw, actor, authority, app})
}
func (s pluginService) mediaSyncDeleteSummary(ctx context.Context) (bool, bool) {
	if s.mediaSyncDelete.queue == nil {
		return false, true
	}
	c, _, on, err := s.mediaSyncDelete.queue.Scope(ctx)
	if err != nil {
		return false, false
	}
	return on && s.mediaSyncDelete.validate(c) == nil, true
}
func (api *mediaSyncDeleteAPI) detail(raw map[string]string, actor string) (pluginConfigDetail, error) {
	spec, err := mediasyncdel.DecodeSpec(raw[mediasyncdel.ConfigKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	fields := []pluginConfigField{
		{Key: "enable", Title: "准备启用 Emby 同步删除", Type: "switch", Tooltip: "只保存原开关；还需在独立管理区审核并单独授权未来回调。", Options: []pluginConfigChoice{}},
		{Key: "del_source", Title: "同时删除原源文件", Type: "switch", Tooltip: "只处理历史登记的原源文件、关联 NFO 及无媒体目录，不再次删除 Emby 库目标；下载任务清理另行迁移授权。", Options: []pluginConfigChoice{}},
		{Key: "send_notify", Title: "完成通知", Type: "switch", Tooltip: "完整处理后按原“自定义消息”通知开关发送；通知不重做删除。", Options: []pluginConfigChoice{}},
		{Key: "exclude_path", Title: "排除的 Emby 媒体路径", Type: "text", WriteOnly: true, Configured: spec.Exclude != "", Tooltip: "英文逗号分隔原媒体路径；按完整路径组件排除，回调路径不直接用于删除。", Options: []pluginConfigChoice{}},
	}
	return pluginConfigDetail{ID: mediasyncdel.Plugin, Name: "Emby 同步删除", Fields: fields, Values: map[string]any{"enable": spec.Enabled, "del_source": spec.DeleteSource, "send_notify": spec.Notify}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: api.fingerprint(raw, actor)}}, nil
}
func mediaSyncDeleteError(w http.ResponseWriter, err error) {
	if errors.Is(err, mediasyncdel.ErrMissing) {
		writeAPIError(w, 404, 404, "未找到原同步删除审核或回执")
		return
	}
	writeAPIError(w, 409, 409, "原同步删除配置、历史或文件范围需要核对；没有自动重做，请只读查回原编号")
}
func (s pluginService) serveMediaSyncDeletePlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.mediaSyncDelete
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	raw, err := api.system.ReadMany(r.Context(), mediaSyncDeleteKeys)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], mediasyncdel.Plugin)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	if action == "install" && installed {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
		return
	}
	if action != "install" && !installed {
		writeAPIError(w, 404, 404, "同步删除插件未安装")
		return
	}
	if action == "get" {
		v, err := api.detail(raw, actor)
		if err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if action == "status" {
		on, known := s.mediaSyncDeleteSummary(r.Context())
		writeJSON(w, 200, map[string]any{"code": 0, "state": on, "stateKnown": known})
		return
	}
	if action == "page" {
		api.pluginPage(w, r)
		return
	}
	if action != "install" && action != "uninstall" && action != "save" && action != "legacy-save" {
		writeAPIError(w, 405, 405, "不支持此同步删除操作")
		return
	}
	values, err := mediasyncdel.Values(raw[mediasyncdel.ConfigKey])
	if err != nil && action != "uninstall" {
		mediaSyncDeleteError(w, err)
		return
	}
	if values == nil {
		values = map[string]any{}
	}
	fingerprint := api.fingerprint(raw, actor)
	if action == "save" {
		var in pluginConfigRequest
		if !decodeServiceRequest(w, r, &in, "无效同步删除设置") {
			return
		}
		if in.Fingerprint != fingerprint {
			mediaSyncDeleteError(w, mediasyncdel.ErrState)
			return
		}
		detail, err := api.detail(raw, actor)
		if err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		next, message := mergePluginConfig(values, detail.Fields, in)
		if message != "" {
			writeAPIError(w, 400, 400, "同步删除字段无效")
			return
		}
		values = next
	}
	if action == "legacy-save" {
		values, err = mediasyncdel.Values(r.PostForm.Get("config"))
		if err != nil || r.PostForm.Get("config") == "" {
			writeAPIError(w, 400, 400, "无效原同步删除配置")
			return
		}
	}
	encoded, _ := json.Marshal(values)
	if action != "uninstall" {
		if _, err := mediasyncdel.DecodeSpec(string(encoded)); err != nil {
			writeAPIError(w, 400, 400, "同步删除开关或排除路径无效")
			return
		}
	}
	epoch, err := refreshNonce()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	authority, err := api.authority(actor)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	app, err := api.appDigest()
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	digest := mediasyncdel.ConfigDigest(string(encoded))
	if digest == "" {
		digest = bus.Hash("retained invalid old config")
	}
	c := mediasyncdel.Control{Epoch: epoch, Actor: actor, Authority: authority, AppDigest: app, ConfigDigest: digest, Created: time.Now().UTC().Format(time.RFC3339Nano), Roots: []mediasyncdel.Root{}}
	control, _ := json.Marshal(c)
	err = api.system.UpdateMany(r.Context(), mediaSyncDeleteKeys, func(current map[string]string) (map[string]string, error) {
		if bus.Hash(current) != bus.Hash(raw) || api.fingerprint(current, actor) != fingerprint {
			return nil, mediasyncdel.ErrState
		}
		ids, err := decodeInstalledPlugins(current["UserInstalledPlugins"])
		if err != nil {
			return nil, err
		}
		if action == "install" {
			ids = append(ids, mediasyncdel.Plugin)
		}
		if action == "uninstall" {
			kept := []string{}
			for _, id := range ids {
				if id != mediasyncdel.Plugin {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		all, _ := json.Marshal(ids)
		out := map[string]string{"UserInstalledPlugins": string(all), mediasyncdel.ControlKey: string(control)}
		if action != "uninstall" {
			out[mediasyncdel.ConfigKey] = string(encoded)
		}
		return out, nil
	})
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "设置已保存，未来同步删除保持停止；请单独审核并授权"})
}

type mediaSyncDeleteStatus struct {
	Installed     bool               `json:"installed"`
	Active        bool               `json:"active"`
	Current       bool               `json:"current"`
	Ready         bool               `json:"ready"`
	WorkerRunning bool               `json:"workerRunning"`
	Fingerprint   string             `json:"fingerprint"`
	Reason        string             `json:"reason"`
	Spec          mediasyncdel.Spec  `json:"spec"`
	Roots         []organizationRoot `json:"roots"`
	Basis         string             `json:"-"`
}

func (api *mediaSyncDeleteAPI) status(ctx context.Context, actor string) (mediaSyncDeleteStatus, error) {
	v := mediaSyncDeleteStatus{Roots: []organizationRoot{}, WorkerRunning: api.workers.Load() > 0}
	raw, err := api.system.ReadMany(ctx, mediaSyncDeleteKeys)
	if err != nil {
		return v, err
	}
	v.Basis = bus.Hash(raw)
	v.Fingerprint = api.fingerprint(raw, actor)
	v.Installed, err = indexerInstalled(raw["UserInstalledPlugins"], mediasyncdel.Plugin)
	if err != nil {
		return v, err
	}
	v.Spec, err = mediasyncdel.DecodeSpec(raw[mediasyncdel.ConfigKey])
	if err != nil {
		v.Reason = "原配置需要修正"
		return v, err
	}
	roots, rootErr := api.organizer.roots(ctx)
	if rootErr == nil {
		v.Roots = roots.Sources
	}
	_, clientErr := api.client()
	c, _, on, scopeErr := api.queue.Scope(ctx)
	v.Current = scopeErr == nil && clientErr == nil && rootErr == nil
	v.Ready = v.Current && v.Installed && v.Spec.Enabled
	v.Active = v.Current && on && api.validate(c) == nil
	if !v.Current {
		v.Reason = "原媒体服务、源目录或同步删除范围不可用"
	} else if on && !v.Active {
		v.Reason = "原管理员、配置或源目录身份已变化"
	}
	return v, nil
}
func (api *mediaSyncDeleteAPI) manage(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	base := "/api/v1/media-sync-delete"
	if r.Method == "GET" && r.URL.Path == base {
		v, err := api.status(r.Context(), actor)
		if err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.Method == "POST" && r.URL.Path == base+"/review" {
		api.review(w, r, actor)
		return
	}
	if r.Method == "POST" && r.URL.Path == base+"/authorize" {
		api.activate(w, r, actor)
		return
	}
	if r.Method == "POST" && r.URL.Path == base+"/stop" {
		var in struct {
			Fingerprint string `json:"fingerprint"`
			Confirm     bool   `json:"confirm"`
		}
		if !decodeOrganizationRequest(w, r, &in, "无效停止确认") {
			return
		}
		v, err := api.status(r.Context(), actor)
		if err != nil || !in.Confirm || in.Fingerprint != v.Fingerprint {
			mediaSyncDeleteError(w, mediasyncdel.ErrState)
			return
		}
		authority, err := api.authority(actor)
		if err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		validate := func() error {
			current, err := api.authority(actor)
			if err != nil || current != authority {
				return mediasyncdel.ErrState
			}
			return nil
		}
		if err := api.queue.Stop(r.Context(), v.Basis, validate); err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		v, err = api.status(r.Context(), actor)
		if err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": v})
		return
	}
	if r.Method == "GET" && r.URL.Path == base+"/receipts" {
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				writeAPIError(w, 400, 400, "无效回执页码")
				return
			}
			page = n
		}
		rows, err := api.queue.List(r.Context(), page, false)
		if err != nil {
			mediaSyncDeleteError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": rows})
		return
	}
	if strings.HasPrefix(r.URL.Path, base+"/receipts/") {
		api.receiptAction(w, r, actor)
		return
	}
	writeAPIError(w, 404, 404, "未知同步删除操作")
}
func (api *mediaSyncDeleteAPI) pluginPage(w http.ResponseWriter, r *http.Request) {
	rows, err := api.queue.List(r.Context(), 1, false)
	if err != nil {
		mediaSyncDeleteError(w, err)
		return
	}
	page := pluginPageData{Title: "Emby 同步删除回执", ReadOnly: true, Sections: []string{"设置保存不会自动开始删除；在下方管理区单独授权未来回调。", "回调路径不直接删除，实际源范围来自完整原历史和当前已授权的源目录。", "未确认、中断和旧回执不自动重做；独立恢复需重新读取并确认原完整范围。"}, Tables: []pluginPageTable{{Columns: []string{"原编号", "媒体", "状态", "历史数量", "完整删除编号"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}}}
	for _, p := range rows {
		page.Tables[0].Rows = append(page.Tables[0].Rows, []string{p.ID, p.Event.Name, p.State, strconv.Itoa(p.Count), p.Parent})
		page.Tables[0].RowActions = append(page.Tables[0].RowActions, nil)
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": page})
}
func (api *mediaSyncDeleteAPI) reservePlugin(next http.Handler) http.Handler {
	const base = "/api/v1/plugins/MediaSyncDel"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == base || strings.HasPrefix(p, base+"/") {
			allowed := p == base && (r.Method == "GET" || r.Method == "PUT" || r.Method == "DELETE") || p == base+"/install" && r.Method == "POST" || p == base+"/page" && r.Method == "GET"
			if !allowed {
				if _, ok := api.authorize(w, r); !ok {
					return
				}
				writeAPIError(w, 405, 405, "不支持此原生同步删除插件操作")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
