package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/cookiecloud"
)

func (s pluginService) nativeCookieCloud(id string) bool {
	return strings.TrimSpace(id) == cookiecloud.Plugin && s.cookieCloud != nil && s.cookieCloud.store != nil
}

func (api *cookieCloudAPI) summary(ctx context.Context) (bool, bool) {
	control, err := api.store.Control(ctx)
	if err != nil {
		return false, false
	}
	if control.Epoch == "" {
		return false, true
	}
	scope, err := api.scope(ctx, control.Actor)
	return err == nil && api.login.pureGo && controlCurrent(control, scope), err == nil
}

func cookieCloudDetail(scope cookieCloudScope) pluginConfigDetail {
	cfg := scope.settings
	fields := []pluginConfigField{
		{Key: "server", Title: "CookieCloud 服务器", Type: "text", Tooltip: "执行时向此服务器发送保存的标识和解密密码"},
		{Key: "key", Title: "用户标识", Type: "text", WriteOnly: true, Configured: cfg.Key != ""},
		{Key: "password", Title: "解密密码", Type: "text", WriteOnly: true, Configured: cfg.Password != ""},
		{Key: "cron", Title: "执行周期", Type: "text", Placeholder: "0 8 * * *"},
		{Key: "synchronousMode", Title: "过滤方式", Type: "select", Options: []pluginConfigChoice{
			{Value: "all_mode", Label: "全部"}, {Value: "black_mode", Label: "黑名单"}, {Value: "white_mode", Label: "白名单"},
		}},
		{Key: "black_list", Title: "黑名单", Type: "textarea"},
		{Key: "white_list", Title: "白名单", Type: "textarea"},
		{Key: "notify", Title: "同步完成时发送插件通知", Type: "switch"},
		{Key: "onlyonce", Title: "接管后运行一次", Type: "switch"},
	}
	for i := range fields {
		if fields[i].Options == nil {
			fields[i].Options = []pluginConfigChoice{}
		}
	}
	return pluginConfigDetail{ID: cookiecloud.Plugin, Name: "CookieCloud 同步", Fields: fields, Values: map[string]any{
		"server": cfg.Server, "cron": cfg.Cron, "synchronousMode": cfg.Mode, "black_list": cfg.BlackList, "white_list": cfg.WhiteList,
		"notify": cfg.Notify, "onlyonce": cfg.OnlyOnce,
	}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: scope.fingerprint(scope.raw)}}
}

func (s pluginService) serveCookieCloudPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.cookieCloud
	actor, ok := api.actor(w, r, action != "get" && action != "page" && action != "legacy-page" && action != "status")
	if !ok {
		return
	}
	ctx := r.Context()
	if action == "install" || action == "uninstall" {
		if err := api.mutateControl(action == "uninstall", func() error {
			return api.store.SetInstalled(ctx, action == "install")
		}); err != nil {
			cookieCloudError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "CookieCloud 插件状态已保存"})
		return
	}
	scope, err := api.scope(ctx, actor)
	if err != nil {
		cookieCloudError(w, err)
		return
	}
	if !scope.installed {
		writeAPIError(w, 404, 404, "CookieCloud 插件未安装")
		return
	}
	switch action {
	case "get":
		signinJSON(w, cookieCloudDetail(scope))
	case "status":
		running, known := api.summary(ctx)
		writeJSON(w, 200, map[string]any{"code": 0, "state": running, "stateKnown": known})
	case "page", "legacy-page":
		runs, err := api.store.Runs(ctx)
		if err != nil {
			cookieCloudError(w, err)
			return
		}
		rows := [][]string{}
		for _, run := range runs {
			rows = append(rows, []string{run.ID, run.Updated, run.State, run.Message, strconv.Itoa(run.UpdatedSites), strconv.Itoa(run.Added)})
		}
		page := pluginPageData{Title: "CookieCloud 同步记录", Sections: []string{}, ReadOnly: true,
			Tables: []pluginPageTable{{Columns: []string{"原记录编号", "时间", "状态", "结果", "更新", "新增"}, Rows: rows, RowActions: make([]*pluginPageRowAction, len(rows))}},
		}
		signinJSON(w, page)
	case "save", "legacy-save":
		var input struct {
			Fingerprint string                     `json:"fingerprint"`
			Values      map[string]json.RawMessage `json:"values"`
			ClearConfig []string                   `json:"clearConfig"`
		}
		if action == "legacy-save" {
			raw := r.FormValue("config")
			if raw == "" || json.Unmarshal([]byte(raw), &input.Values) != nil {
				cookieCloudError(w, cookiecloud.ErrConfig)
				return
			}
			input.Fingerprint = r.FormValue("fingerprint")
			if input.Fingerprint == "" {
				input.Fingerprint = scope.fingerprint(scope.raw)
			}
		} else if !siteLoginJSON(w, r, &input) {
			return
		}
		if input.Fingerprint != scope.fingerprint(scope.raw) {
			cookieCloudError(w, cookiecloud.ErrScope)
			return
		}
		if input.Values == nil {
			input.Values = map[string]json.RawMessage{}
		}
		for _, key := range input.ClearConfig {
			if key != "key" && key != "password" {
				cookieCloudError(w, cookiecloud.ErrConfig)
				return
			}
			input.Values[key] = json.RawMessage(`""`)
		}
		if err = api.save(ctx, scope, input.Values); err != nil {
			cookieCloudError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "CookieCloud 设置已保存，请单独确认执行"})
	default:
		writeAPIError(w, 404, 404, "未找到 CookieCloud 插件操作")
	}
}
