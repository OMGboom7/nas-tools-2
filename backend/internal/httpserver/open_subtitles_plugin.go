package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/opensubtitles"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type openSubtitleService struct {
	Provider string `json:"provider"`
	Origin   string `json:"origin"`
	Remote   bool   `json:"remote"`
}
type openSubtitleAPI struct {
	subtitle  *subtitleAPI
	system    *systemconfig.Store
	transport http.RoundTripper
	browser   browserruntime.Renderer
	mu        sync.Mutex
	sessions  map[string]opensubtitles.Session
	active    map[string]context.CancelFunc
	lastLogin time.Time
}

var openSubtitleKeys = []string{"UserInstalledPlugins", opensubtitles.ConfigKey, opensubtitles.ControlKey}

func (s pluginService) nativeOpenSubtitles(id string) bool {
	return s.openSubtitles != nil && strings.TrimSpace(id) == opensubtitles.Plugin
}
func (api *openSubtitleAPI) authority(actor string) (string, error) {
	return api.subtitle.requestAuthority(actor)
}
func (api *openSubtitleAPI) fingerprint(raw map[string]string, actor string) string {
	authority, e := api.authority(actor)
	if e != nil {
		authority = "unavailable"
	}
	return organization.Digest([]any{raw, actor, authority})
}
func (api *openSubtitleAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.subtitle == nil || api.subtitle.organizer == nil || api.system == nil {
		writeAPIError(w, 503, 503, "native subtitle consumer unavailable")
		return "", false
	}
	auth := api.subtitle.organizer.auth
	if auth == nil || auth.service == nil {
		writeAPIError(w, 503, 503, "native administrator account unavailable")
		return "", false
	}
	claims, e := auth.service.VerifyToken(r.Header.Get("Authorization"))
	if e != nil {
		writeAPIError(w, 401, 401, "current login required")
		return "", false
	}
	if _, e = auth.service.FindUser(r.Context(), claims.Username); e != nil {
		writeAPIError(w, 401, 401, "current account unavailable")
		return "", false
	}
	if _, e = api.authority(claims.Username); e != nil {
		writeAPIError(w, 403, 403, "current root administrator required")
		return "", false
	}
	if write && !api.subtitle.organizer.pureGo {
		writeAPIError(w, 501, 501, "native OpenSubtitles operations require Go-only ownership")
		return "", false
	}
	return claims.Username, true
}
func (api *openSubtitleAPI) detail(raw map[string]string, actor string) pluginConfigDetail {
	c, _ := opensubtitles.Parse(raw[opensubtitles.ConfigKey])
	var control opensubtitles.Control
	_ = opensubtitles.Decode(raw[opensubtitles.ControlKey], &control)
	authority, _ := api.authority(actor)
	enabled := control.Authorized && control.Config == organization.Digest(raw[opensubtitles.ConfigKey]) && control.Actor == actor && control.Authority == authority
	fields := []pluginConfigField{
		{Key: "enable", Title: "原 OpenSubtitles 字幕开关", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "原开关保留；Go 后台还需要下面的独立授权。关闭会停止后续处理，已经开始的请求及文件仍须核对原回执。"},
		{Key: "provider", Title: "本次配置的字幕提供方式", Type: "select", Options: []pluginConfigChoice{{Value: opensubtitles.Legacy, Label: "原 opensubtitles.org 网页"}, {Value: opensubtitles.REST, Label: "官方 opensubtitles.com REST API"}}, Tooltip: "不会从 .org 自动改用 .com 的账户或地址。.org 需要验证时停止；REST 使用独立的应用密钥和 .com 账户，下载授权会消耗其配额。"},
		{Key: "api_key", Title: ".com 应用 API Key", Type: "text", WriteOnly: true, Configured: c.APIKey != "", Options: []pluginConfigChoice{}, Tooltip: "仅 REST 使用；留空保留。"},
		{Key: "username", Title: ".com 用户名", Type: "text", WriteOnly: true, Configured: c.Username != "", Options: []pluginConfigChoice{}},
		{Key: "password", Title: ".com 密码", Type: "text", WriteOnly: true, Configured: c.Password != "", Options: []pluginConfigChoice{}, Tooltip: "登录令牌只在本进程短期使用，不进入公开回执；拒绝登录后同一授权不继续尝试。"},
		{Key: "cookie", Title: "原 .org Cookie（可选）", Type: "text", WriteOnly: true, Configured: c.Cookie != "", Options: []pluginConfigChoice{}, Tooltip: "仅用于原 .org 网站及其合法 Cookie 范围；不会发送给 .com。验证页不会被当成字幕，也不会自动破解验证码。"},
		{Key: "user_agent", Title: "应用 User-Agent", Type: "text", Options: []pluginConfigChoice{}, Tooltip: "REST 应用标识须对应你的 API 消费者。原网页未另配时沿用应用的旧浏览器标识。"},
		{Key: "go_enable", Title: "启用 Go 处理授权后的新字幕请求", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "只处理以后完整入库或另行确认的单文件请求；不补领旧历史、旧 Python 待办或已停止的原回执。"},
		{Key: "go_confirm", Title: "允许向所选原提供方传送媒体名称、年份、IMDb 和季集", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "网页方式使用 www.opensubtitles.org／dl.opensubtitles.org；REST 使用 api.opensubtitles.com／vip-api.opensubtitles.com 及 www／dl 的字幕下载地址。视频内容不上传。"},
		{Key: "go_write", Title: "允许 Go 在已证明的原视频旁新增中文字幕，保留已有字幕", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "沿用原固定名及 .srt／.ass／.ssa。最多处理三个候选下载；失败、未知应答及进程重启不自动重发。"},
		{Key: "go_old_stopped", Title: "原 Python 字幕执行器及其他重复写入器已实际停止", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "go_remote", Title: "另外允许未来原入库范围的远程字幕与恢复副本写入", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "仅限原 Rclone／MinIO 账户和已批准命名空间；确认已独占该范围并接受重复完整读取及持久副本的流量、存储费用。远程没有通用原子发布。单文件仍须另外确认本次远程写入。"},
	}
	return pluginConfigDetail{ID: opensubtitles.Plugin, Name: opensubtitles.Plugin, Fields: fields, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: api.fingerprint(raw, actor)}, Values: map[string]any{"enable": c.Enabled, "provider": c.Provider, "user_agent": c.UserAgent, "go_enable": enabled, "go_confirm": false, "go_write": false, "go_old_stopped": false, "go_remote": false}}
}
func (s pluginService) openSubtitleSummary(ctx context.Context) (bool, bool) {
	api := s.openSubtitles
	if api == nil || api.subtitle.organizer.jobs == nil || !api.subtitle.organizer.pureGo {
		return false, false
	}
	_, scope, wanted, ready, e := api.subtitle.organizer.jobs.OpenSubtitleProvider(ctx)
	if e != nil || !wanted || !ready {
		return false, true
	}
	denied, err := api.subtitle.organizer.jobs.OpenSubtitleDenied(ctx, scope.Epoch)
	authority, e := api.authority(scope.Actor)
	return err == nil && !denied && e == nil && authority == scope.Authority, true
}
func (s pluginService) serveOpenSubtitlePlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.openSubtitles
	write := action == "save" || action == "legacy-save" || action == "install" || action == "uninstall"
	actor, ok := api.actor(w, r, write)
	if !ok {
		return
	}
	raw, e := s.system.ReadMany(r.Context(), openSubtitleKeys)
	if e != nil {
		openSubtitleError(w, e)
		return
	}
	installed, e := indexerInstalled(raw[openSubtitleKeys[0]], opensubtitles.Plugin)
	if e != nil {
		openSubtitleError(w, e)
		return
	}
	if !write {
		if action == "status" {
			enabled, _ := s.openSubtitleSummary(r.Context())
			writeJSON(w, 200, map[string]any{"code": 0, "state": enabled})
			return
		}
		if !installed {
			writeAPIError(w, 404, 404, "plugin is not installed")
			return
		}
		if action == "page" {
			api.page(w, r)
			return
		}
		if action != "get" {
			writeAPIError(w, 405, 405, "unsupported native subtitle action")
			return
		}
		if _, err := opensubtitles.Parse(raw[opensubtitles.ConfigKey]); err != nil {
			openSubtitleError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": api.detail(raw, actor)})
		return
	}
	if action == "install" && installed {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
		return
	}
	if action != "install" && !installed {
		writeAPIError(w, 409, 409, "plugin is not installed")
		return
	}
	var in pluginConfigRequest
	if action == "save" && !decodeServiceRequest(w, r, &in, "invalid subtitle settings") {
		return
	}
	if action == "save" && in.Fingerprint != api.fingerprint(raw, actor) {
		openSubtitleError(w, organization.ErrState)
		return
	}
	values := map[string]any{}
	if raw[opensubtitles.ConfigKey] != "" && opensubtitles.Decode(raw[opensubtitles.ConfigKey], &values) != nil && action != "uninstall" {
		openSubtitleError(w, opensubtitles.ErrConfig)
		return
	}
	if values == nil {
		values = map[string]any{}
	}
	next := values
	if action == "save" {
		merged, msg := mergePluginConfig(values, api.detail(raw, actor).Fields, in)
		if msg != "" {
			writeAPIError(w, 400, 400, "invalid native subtitle fields")
			return
		}
		next = merged
	} else if action == "legacy-save" {
		var provided map[string]any
		if opensubtitles.Decode(r.PostForm.Get("config"), &provided) != nil || provided == nil {
			writeAPIError(w, 400, 400, "invalid legacy subtitle configuration")
			return
		}
		next = map[string]any{}
		for key, value := range values {
			next[key] = value
		}
		for key, value := range provided {
			next[key] = value
		}
		if provided["go_enable"] == true && r.PostForm.Get("fingerprint") != api.fingerprint(raw, actor) {
			openSubtitleError(w, organization.ErrState)
			return
		}
	}
	requested := next["go_enable"] == true && next["enable"] == true && (action == "save" || action == "legacy-save")
	if requested && (next["go_confirm"] != true || next["go_write"] != true || next["go_old_stopped"] != true) {
		writeAPIError(w, 400, 400, "separate provider, subtitle write and stopped old executor consent required")
		return
	}
	remote := requested && next["go_remote"] == true
	for _, key := range []string{"go_enable", "go_confirm", "go_write", "go_old_stopped", "go_remote"} {
		delete(next, key)
	}
	configRaw := raw[opensubtitles.ConfigKey]
	if action == "save" || action == "legacy-save" {
		encoded, err := json.Marshal(next)
		if err != nil || len(encoded) > 64<<10 {
			openSubtitleError(w, opensubtitles.ErrConfig)
			return
		}
		configRaw = string(encoded)
		if _, err = opensubtitles.Parse(configRaw); err != nil {
			openSubtitleError(w, err)
			return
		}
	}
	authority, e := api.authority(actor)
	if e != nil {
		openSubtitleError(w, e)
		return
	}
	epoch, e := newIndexerClaim()
	if e != nil {
		openSubtitleError(w, e)
		return
	}
	grant := opensubtitles.Control{Version: 1, Epoch: epoch, Actor: actor, Authority: authority, Config: organization.Digest(configRaw), Authorized: requested, Remote: remote, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	encoded, _ := json.Marshal(grant)
	e = s.system.UpdateMany(r.Context(), openSubtitleKeys, func(current map[string]string) (map[string]string, error) {
		currentAuthority, e := api.authority(actor)
		if e != nil || currentAuthority != authority || organization.Digest(current) != organization.Digest(raw) {
			return nil, organization.ErrState
		}
		ids := []string{}
		if current["UserInstalledPlugins"] != "" && opensubtitles.Decode(current["UserInstalledPlugins"], &ids) != nil {
			return nil, opensubtitles.ErrConfig
		}
		if action == "install" {
			ids = append(ids, opensubtitles.Plugin)
		}
		if action == "uninstall" {
			keep := []string{}
			for _, id := range ids {
				if id != opensubtitles.Plugin {
					keep = append(keep, id)
				}
			}
			ids = keep
		}
		installedRaw, _ := json.Marshal(ids)
		update := map[string]string{opensubtitles.ControlKey: string(encoded)}
		if action == "install" || action == "uninstall" {
			update["UserInstalledPlugins"] = string(installedRaw)
		}
		if action == "save" || action == "legacy-save" {
			update[opensubtitles.ConfigKey] = configRaw
		}
		return update, nil
	})
	if e != nil {
		openSubtitleError(w, e)
		return
	}
	api.stopActive()
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "原配置与回执保留；仅本次独立授权后的新请求可执行，旧请求不重放"})
}
func openSubtitleError(w http.ResponseWriter, e error) {
	status := 503
	if errors.Is(e, organization.ErrState) || errors.Is(e, organization.ErrClaimed) || errors.Is(e, opensubtitles.ErrState) {
		status = 409
	}
	if errors.Is(e, organization.ErrPath) || errors.Is(e, opensubtitles.ErrConfig) {
		status = 400
	}
	writeAPIError(w, status, status, "原 OpenSubtitles 范围未核实；未扩大请求或自动重试，请只读查回原编号")
}
func (api *openSubtitleAPI) stopActive() {
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, cancel := range api.active {
		cancel()
	}
	api.sessions = map[string]opensubtitles.Session{}
}
func (api *openSubtitleAPI) manage(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, r.Method != "GET")
	if !ok {
		return
	}
	if !api.subtitle.organizer.pureGo {
		writeAPIError(w, 501, 501, "native subtitle receipts require Go-only ownership")
		return
	}
	store := api.subtitle.organizer.jobs
	if store == nil {
		writeAPIError(w, 503, 503, "native subtitle journal unavailable")
		return
	}
	base := "/api/v1/open-subtitles"
	var data any
	var e error
	switch {
	case r.URL.Path == base && r.Method == "GET":
		_, scope, wanted, ready, err := store.OpenSubtitleProvider(r.Context())
		if err != nil {
			openSubtitleError(w, err)
			return
		}
		authority, _ := api.authority(actor)
		ready = ready && scope.Actor == actor && scope.Authority == authority
		if ready {
			denied, err := store.OpenSubtitleDenied(r.Context(), scope.Epoch)
			if err != nil {
				openSubtitleError(w, err)
				return
			}
			ready = !denied
		}
		origin := ""
		provider := ""
		if ready {
			origin = scope.Origin
			provider = scope.Provider
		}
		data = map[string]any{"wanted": wanted, "ready": ready, "origin": origin, "provider": provider, "remote": ready && scope.Remote}
	case r.URL.Path == base+"/requests" && r.Method == "GET":
		page := 1
		if v := r.URL.Query().Get("page"); v != "" {
			page, e = strconv.Atoi(v)
		}
		if e == nil {
			data, e = store.OpenSubtitles(r.Context(), false, page)
		}
	case strings.HasPrefix(r.URL.Path, base+"/requests/"):
		tail := strings.TrimPrefix(r.URL.Path, base+"/requests/")
		id, action, has := strings.Cut(tail, "/")
		if !has && r.Method == "GET" {
			data, e = store.OpenSubtitle(r.Context(), id)
		} else if has && action == "stop" && r.Method == "POST" {
			var in struct {
				Confirm bool   `json:"confirm"`
				Reason  string `json:"reason"`
			}
			if !decodeSubtitleJSON(w, r, &in) {
				return
			}
			if !in.Confirm {
				writeAPIError(w, 400, 400, "确认仅停止这份尚未开始的原 OpenSubtitles 请求")
				return
			}
			closeGuards, err := api.subtitle.requestGuards(r.Context(), false)
			if err != nil {
				openSubtitleError(w, err)
				return
			}
			defer closeGuards()
			if _, err = api.authority(actor); err != nil {
				openSubtitleError(w, err)
				return
			}
			data, e = store.StopOpenSubtitle(r.Context(), id, actor, in.Reason, func() error { _, e := api.authority(actor); return e })
		} else {
			writeAPIError(w, 405, 405, "unsupported original subtitle receipt operation")
			return
		}
	default:
		writeAPIError(w, 405, 405, "unsupported original subtitle operation")
		return
	}
	if e != nil {
		openSubtitleError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
func (api *openSubtitleAPI) page(w http.ResponseWriter, r *http.Request) {
	if api.subtitle.organizer.jobs == nil {
		openSubtitleError(w, organization.ErrState)
		return
	}
	rows, e := api.subtitle.organizer.jobs.OpenSubtitles(r.Context(), false, 1)
	if e != nil {
		openSubtitleError(w, e)
		return
	}
	table := pluginPageTable{Columns: []string{"原编号", "媒体", "提供方式", "状态", "请求数", "说明", "更新时间"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, row := range rows {
		table.Rows = append(table.Rows, []string{row.ID, cleanPluginPageText(row.Name, 600), row.Provider, row.State, strconv.Itoa(row.Requests), row.Reason, row.Updated})
		table.RowActions = append(table.RowActions, nil)
	}
	data := pluginPageData{Title: "OpenSubtitles 原字幕回执", ReadOnly: true, Sections: []string{"这是独立的原字幕消费者；只有新增／保留文件的持久凭据才报告完成。请求数包括登录、检索和下载授权，不表示新增字幕数。", "应答不明、处理中断及进程重启均不重发。可在整理页按完整原编号查回较早回执，尚未开始项可单独注明原因后停止。", "不会自动迁移 .org 账户到 .com，不破解验证页面；REST 下载授权可能已消耗配额。"}, Tables: []pluginPageTable{table}}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
func (api *openSubtitleAPI) reservePlugin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "/api/v1/plugins/OpenSubtitles"
		if r.URL.Path == base || strings.HasPrefix(r.URL.Path, base+"/") {
			known := r.URL.Path == base && (r.Method == "GET" || r.Method == "PUT" || r.Method == "DELETE") || r.URL.Path == base+"/install" && r.Method == "POST" || r.URL.Path == base+"/page" && r.Method == "GET"
			if !known {
				writeAPIError(w, 405, 405, "unknown native OpenSubtitles route")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
