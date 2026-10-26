package httpserver

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func (s pluginService) nativeMediaRefresh(id string) bool {
	return s.mediaRefresh != nil && strings.TrimSpace(id) == mediaserver.RefreshPluginID
}
func refreshDelay(v any) (int64, error) {
	if v == nil || v == "" {
		return 0, nil
	}
	var number float64
	switch n := v.(type) {
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, mediaserver.ErrRefreshState
		}
		number = f
	case float64:
		number = n
	case int:
		number = float64(n)
	case int64:
		number = float64(n)
	default:
		return 0, mediaserver.ErrRefreshState
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number > 2147483647 {
		return 0, mediaserver.ErrRefreshState
	}
	if number < 0 {
		return 0, nil
	}
	return int64(number), nil
}
func (api *mediaRefreshAPI) pluginDetail(raw map[string]string, values map[string]any, actor string) pluginConfigDetail {
	delay, err := refreshDelay(values["delay"])
	if err != nil {
		delay = 0
	}
	detail := pluginConfigDetail{ID: mediaserver.RefreshPluginID, Name: "刷新媒体库", Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: api.fingerprint(raw, actor)}, Values: map[string]any{
		"enable": truthy(values["enable"]), "delay": strconv.FormatInt(delay, 10), "go_confirm": false, "go_old_stopped": false, "go_scan": false,
	}, Fields: []pluginConfigField{
		{Key: "enable", Title: "接收入库完成及明确刷新请求", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "默认不接管旧授权。只处理本次授权之后的新原生入库事件，不补扫历史。"},
		{Key: "delay", Title: "自动入库事件的延迟秒数", Type: "text", Options: []pluginConfigChoice{}, Tooltip: "支持旧小数字符串，向零取整；负值按 0。手动明确刷新不延迟，待办时间持久保存。"},
		{Key: "go_confirm", Title: "确认对当前媒体服务器及未来新事件启用原生后台刷新", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "go_old_stopped", Title: "确认旧 Python 刷新插件及其他重复事件执行器已实际停止", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "go_scan", Title: "独立同意定向或回退整库扫描、服务器元数据变化和 I/O 开销", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "不强制替换全部元数据、不清空回收站、不删除媒体；服务器仍按其自身扫描设置工作。已接受的扫描不能由停用撤回。"},
	}}
	spec, err := api.spec()
	if err == nil && spec.cloud {
		detail.Values["go_plex_login"], detail.Values["go_plex_http"] = false, false
		detail.Fields = append(detail.Fields,
			pluginConfigField{Key: "go_plex_login", Title: "独立同意原 Plex 账户模式仅向 plex.tv 登录并发现所选服务器", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "Plex 已弃用密码登录，可能要求换用其新版登录方式；失败不自动重试或绕过验证。账户口令及令牌不进入事件和回执。"},
			pluginConfigField{Key: "go_plex_http", Title: "可选：允许发现的旧 Plex 服务器使用明文 HTTP 连接", Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "默认不选，只尝试 HTTPS。HTTP 会在该连接上传送服务器令牌，可能被网络观察者获取；仍不关闭证书验证。"})
	}
	return detail
}
func (s pluginService) mediaRefreshSummary(ctx context.Context) (bool, bool) {
	api := s.mediaRefresh
	state, err := api.read(ctx)
	if err != nil {
		return false, false
	}
	_, err = api.scope(ctx, state.control)
	return err == nil, true
}
func (s pluginService) serveMediaRefreshPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.mediaRefresh
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	raw, err := s.system.ReadMany(ctx, mediaRefreshKeys)
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	reviewed := api.fingerprint(raw, actor)
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], mediaserver.RefreshPluginID)
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	values, configErr := decodeMetadataConfig(raw[mediaserver.RefreshPluginKey])
	_, controlErr := mediaserver.DecodeRefreshControl(raw[mediaserver.RefreshControlKey])
	write := action == "install" || action == "uninstall" || action == "save" || action == "legacy-save"
	if !write {
		if configErr != nil || controlErr != nil {
			mediaRefreshError(w, mediaserver.ErrRefreshState)
			return
		}
		if action == "status" {
			running, known := s.mediaRefreshSummary(ctx)
			var state any
			if installed && known {
				state = running
			}
			writeJSON(w, 200, map[string]any{"code": 0, "state": state, "stateKnown": known})
			return
		}
		if !installed {
			writeAPIError(w, 404, 404, "plugin not installed")
			return
		}
		if action == "page" {
			api.pluginPage(w, r)
			return
		}
		if action != "get" {
			writeAPIError(w, 405, 405, "unsupported native media refresh action")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": api.pluginDetail(raw, values, actor)})
		return
	}
	if action == "install" && installed {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true})
		return
	}
	if action != "install" && !installed {
		if action == "uninstall" && r.URL.Path == "/api/v1/plugin/uninstall" {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true})
			return
		}
		writeAPIError(w, 409, 409, "plugin not installed")
		return
	}
	if configErr != nil && action != "uninstall" && action != "legacy-save" {
		mediaRefreshError(w, configErr)
		return
	}
	next := values
	if next == nil {
		next = map[string]any{}
	}
	explicit := false
	if action == "save" {
		var in pluginConfigRequest
		if !decodeServiceRequest(w, r, &in, "invalid media refresh plugin settings") {
			return
		}
		if in.Fingerprint != reviewed {
			mediaRefreshError(w, mediaserver.ErrRefreshState)
			return
		}
		var message string
		next, message = mergePluginConfig(values, api.pluginDetail(raw, values, actor).Fields, in)
		if message != "" {
			writeAPIError(w, 400, 400, "invalid media refresh fields")
			return
		}
		explicit = true
	} else if action == "legacy-save" {
		next, err = decodeMetadataConfig(r.PostForm.Get("config"))
		if err != nil || r.PostForm.Get("config") == "" {
			writeAPIError(w, 400, 400, "invalid media refresh configuration")
			return
		}
		explicit = r.PostForm.Get("fingerprint") == reviewed
		if next["go_confirm"] == true && !explicit {
			mediaRefreshError(w, mediaserver.ErrRefreshState)
			return
		}
	}
	active := false
	delay := int64(0)
	if action == "save" || action == "legacy-save" {
		enable, valid := next["enable"].(bool)
		if !valid {
			writeAPIError(w, 400, 400, "explicit boolean enable required")
			return
		}
		delay, err = refreshDelay(next["delay"])
		if err != nil {
			writeAPIError(w, 400, 400, "invalid delay in seconds")
			return
		}
		active = enable && explicit && next["go_confirm"] == true && next["go_old_stopped"] == true && next["go_scan"] == true
		if enable && action == "save" && !active {
			writeAPIError(w, 400, 400, "independent current-server, old-executor and scan-side-effect confirmations required")
			return
		}
		next["delay"] = strconv.FormatInt(delay, 10)
	}
	// One-shot approvals are not persistent plugin configuration or defaults.
	plexLogin, plexHTTP := next["go_plex_login"] == true, next["go_plex_http"] == true
	for _, key := range []string{"go_confirm", "go_old_stopped", "go_scan", "go_plex_login", "go_plex_http"} {
		delete(next, key)
	}
	epoch, err := refreshNonce()
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	control := mediaserver.RefreshControl{Epoch: epoch, Actor: actor, Delay: delay, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	if active {
		spec, err := api.spec()
		if err != nil {
			mediaRefreshError(w, err)
			return
		}
		if api.fingerprint(raw, actor) != reviewed {
			mediaRefreshError(w, mediaserver.ErrRefreshState)
			return
		}
		if spec.cloud && !plexLogin {
			writeAPIError(w, 400, 400, "separate Plex account credential transmission consent required")
			return
		}
		control.PlexAccount, control.PlexHTTP = spec.cloud && plexLogin, spec.cloud && plexHTTP
		client, err := api.resolve(ctx, spec, control)
		if err != nil {
			mediaRefreshError(w, err)
			return
		}
		serverID, err := client.RefreshIdentity(ctx)
		if err != nil {
			mediaRefreshError(w, err)
			return
		}
		authority, err := api.authority(actor)
		if err != nil {
			mediaRefreshError(w, err)
			return
		}
		control.Authorized, control.Server, control.ServerID, control.ServerDigest, control.AuthorityDigest, control.PluginDigest = true, spec.kind, serverID, spec.digest, authority, mediaserver.RefreshDigest(next)
	}
	if action == "uninstall" {
		control.Authorized = false
	}
	valueJSON, err := json.Marshal(next)
	if err != nil || len(valueJSON) > 64<<10 {
		mediaRefreshError(w, mediaserver.ErrRefreshState)
		return
	}
	controlJSON, err := json.Marshal(control)
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	err = s.system.UpdateMany(ctx, mediaRefreshKeys, func(current map[string]string) (map[string]string, error) {
		if mediaserver.RefreshDigest(current) != mediaserver.RefreshDigest(raw) || api.fingerprint(current, actor) != reviewed {
			return nil, mediaserver.ErrRefreshState
		}
		ids, err := decodeInstalledPlugins(current["UserInstalledPlugins"])
		if err != nil {
			return nil, err
		}
		if action == "install" {
			ids = append(ids, mediaserver.RefreshPluginID)
		}
		if action == "uninstall" {
			kept := []string{}
			for _, id := range ids {
				if id != mediaserver.RefreshPluginID {
					kept = append(kept, id)
				}
			}
			ids = kept
		}
		b, err := json.Marshal(ids)
		if err != nil {
			return nil, err
		}
		result := map[string]string{"UserInstalledPlugins": string(b), mediaserver.RefreshControlKey: string(controlJSON)}
		if action != "uninstall" {
			result[mediaserver.RefreshPluginKey] = string(valueJSON)
		}
		return result, nil
	})
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "active": active, "message": "刷新配置已保存；旧待办不会由新授权接管，已进入服务器的请求仍须核对"})
}
func (api *mediaRefreshAPI) pluginPage(w http.ResponseWriter, r *http.Request) {
	state, err := api.read(r.Context())
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	jobs, err := api.queue.List(r.Context(), false, time.Now())
	if err != nil {
		mediaRefreshError(w, err)
		return
	}
	_, scopeErr := api.scope(r.Context(), state.control)
	data := pluginPageData{Title: "媒体服务器刷新队列", ReadOnly: true, Sections: []string{
		"这里只读查看，不扫描媒体服务器。手动审核、请求停止及按较早稳定编号查找回执，请到服务页。",
		"旧启用状态不启动新执行器；授权只接收新的完整入库事件，变更配置或重新授权不会重放旧待办。",
		"accepted 只证明服务器接收请求，不证明扫描完成。planning/running/intent 或 uncertain 中断后均不会自动重发。",
		"当前授权有效：" + strconv.FormatBool(scopeErr == nil) + "；后台运行：" + strconv.FormatBool(api.workers.Load() > 0),
	}, Tables: []pluginPageTable{}}
	table := pluginPageTable{Columns: []string{"稳定编号", "来源／原任务", "状态", "最早时间", "逐项已接收／总数", "说明"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, j := range jobs {
		accepted := 0
		for _, a := range j.Actions {
			if a.State == "accepted" {
				accepted++
			}
		}
		table.Rows = append(table.Rows, []string{j.ID, j.Source + " / " + j.Reference, j.State, j.Due, strconv.Itoa(accepted) + " / " + strconv.Itoa(len(j.Actions)), j.Reason})
		table.RowActions = append(table.RowActions, nil)
	}
	data.Tables = append(data.Tables, table)
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
