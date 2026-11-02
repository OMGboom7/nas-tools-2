package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func (s pluginService) nativeSignin(id string) bool {
	return id == sitesignin.Plugin && s.siteSignin != nil && s.siteSignin.store != nil
}
func (s pluginService) signinSummary(ctx context.Context) (bool, bool) {
	c, e := s.siteSignin.store.Control(ctx)
	if e != nil {
		return false, false
	}
	return c.Enabled && s.siteSignin.base(c.Scope, "", c.Scope.PluginDigest)(ctx) == nil, true
}
func (api *siteSigninAPI) detail(ctx context.Context, actor string) (pluginConfigDetail, error) {
	raw, _, settings, _, e := api.pluginSnapshot(ctx)
	if e != nil {
		return pluginConfigDetail{}, e
	}
	basis, e := api.fingerprint(ctx, actor)
	if e != nil {
		return pluginConfigDetail{}, e
	}
	fields := []pluginConfigField{}
	for _, v := range []struct{ key, title string }{{"enabled", "准备启用周期签到"}, {"notify", "签到汇总通知"}, {"onlyonce", "接管后运行一次"}, {"clean", "接管后重新核对今日已确认的站点"}, {"missed_detection", "漏签检测"}} {
		fields = append(fields, pluginConfigField{Key: v.key, Title: v.title, Type: "switch", Options: []pluginConfigChoice{}, Tooltip: "保存不会直接访问站点，请在签到管理中核对后执行。未知动作不会自动重做。"})
	}
	for _, v := range []struct{ key, title, tip string }{{"cron", "签到时间或周期", "保留原格式：五段 cron、08:00、08:00-09:00，或小时数（例如 23.5）。"}, {"retry_keyword", "已确认失败的重试关键词", "只对可重试失败和延后任务生效；不明结果、停止和中断任务不自动重做。"}, {"queue_cnt", "原配置并发数", "保留原值；Go 同时处理最多 32 个站点，其余站点排队执行。"}, {"auto_cf", "CF 优选失败阈值", "保留原配置；CF 优选执行器尚未迁移，触发时保留依赖状态，不调用 Python。"}, {"missed_schedule", "漏签检测时间段", "例如 09:00-23:59；按原时区每小时核对未完成站点。"}} {
		fields = append(fields, pluginConfigField{Key: v.key, Title: v.title, Type: "text", WriteOnly: true, Configured: configuredPluginValue(objectSigninRaw(raw)[v.key]), Options: []pluginConfigChoice{}, Tooltip: v.tip})
	}
	all, e := api.login.sites.List(ctx)
	if e != nil {
		return pluginConfigDetail{}, e
	}
	options := []pluginConfigChoice{}
	seen := map[string]bool{}
	for _, site := range all {
		id := strconv.FormatInt(site.ID, 10)
		seen[id] = true
		options = append(options, pluginConfigChoice{id, site.Name})
	}
	for _, id := range append(append([]string{}, settings.Sites...), settings.Special...) {
		if !seen[id] {
			seen[id] = true
			options = append(options, pluginConfigChoice{id, "原站点 " + id + "（当前不存在）"})
		}
	}
	for _, v := range []struct{ key, title string }{{"sign_sites", "签到站点"}, {"special_sites", "需要重复核对的特殊站点"}} {
		fields = append(fields, pluginConfigField{Key: v.key, Title: v.title, Type: "multiselect", Options: options, Tooltip: "空选择保持为空，不扩大到全部站点。"})
	}
	values := map[string]any{"enabled": settings.Enabled, "notify": settings.Notify, "onlyonce": settings.Once, "clean": settings.Clean, "missed_detection": settings.Missed, "sign_sites": settings.Sites, "special_sites": settings.Special}
	return pluginConfigDetail{ID: sitesignin.Plugin, Name: "自动签到", Fields: fields, Values: values, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: basis}}, nil
}
func objectSigninRaw(raw string) map[string]any {
	out := map[string]any{}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&out)
	return out
}
func (s pluginService) serveSigninPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.siteSignin
	actor, ok := api.actor(w, r, action != "get" && action != "page" && action != "status")
	if !ok {
		return
	}
	raw, e := api.system.ReadMany(r.Context(), []string{"UserInstalledPlugins", "plugin." + sitesignin.Plugin})
	if e != nil {
		signinError(w, e)
		return
	}
	installed, e := indexerInstalled(raw["UserInstalledPlugins"], sitesignin.Plugin)
	if e != nil {
		signinError(w, e)
		return
	}
	if action != "install" && !installed {
		writeAPIError(w, 404, 404, "自动签到插件未安装")
		return
	}
	if action == "get" {
		detail, e := api.detail(r.Context(), actor)
		if e != nil {
			signinError(w, e)
			return
		}
		signinJSON(w, detail)
		return
	}
	if action == "status" {
		on, known := s.signinSummary(r.Context())
		writeJSON(w, 200, map[string]any{"code": 0, "state": on, "stateKnown": known})
		return
	}
	if action == "page" {
		api.pluginPage(w, r, actor)
		return
	}
	if action != "install" && action != "uninstall" && action != "save" && action != "legacy-save" {
		writeAPIError(w, 405, 405, "不支持此签到配置操作")
		return
	}
	updates := map[string]json.RawMessage{}
	if action == "save" {
		var in struct {
			Fingerprint string                     `json:"fingerprint"`
			Values      map[string]json.RawMessage `json:"values"`
			ClearConfig []string                   `json:"clearConfig"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		basis, e := api.fingerprint(r.Context(), actor)
		if e != nil || basis != in.Fingerprint {
			signinError(w, sitesignin.ErrScope)
			return
		}
		updates = in.Values
		if updates == nil {
			updates = map[string]json.RawMessage{}
		}
		for _, key := range in.ClearConfig {
			updates[key] = json.RawMessage("null")
		}
	}
	if action == "legacy-save" {
		if json.Unmarshal([]byte(r.PostForm.Get("config")), &updates) != nil || updates == nil {
			writeAPIError(w, 400, 400, "无效的原签到设置")
			return
		}
	}
	next := raw["plugin."+sitesignin.Plugin]
	if action != "uninstall" {
		next, e = sitesignin.MergeSettings(next, updates)
		if e != nil {
			signinError(w, e)
			return
		}
		settings, e := sitesignin.ParseSettings(next)
		location, zoneErr := rssWorkerLocation()
		if e != nil || zoneErr != nil {
			signinError(w, sitesignin.ErrState)
			return
		}
		if _, e = parseSigninSchedule(settings.Cron, location); e != nil {
			writeAPIError(w, 400, 400, "签到时间或周期格式无效")
			return
		}
	}
	authority, _, e := api.login.authority(actor)
	if e != nil {
		signinError(w, e)
		return
	}
	e = api.system.UpdateMany(r.Context(), []string{"UserInstalledPlugins", "plugin." + sitesignin.Plugin}, func(current map[string]string) (map[string]string, error) {
		if sitelogin.Hash(current) != sitelogin.Hash(raw) {
			return nil, sitesignin.ErrScope
		}
		now, _, e := api.login.authority(actor)
		if e != nil || now != authority {
			return nil, sitesignin.ErrScope
		}
		ids, e := decodeInstalledPlugins(current["UserInstalledPlugins"])
		if e != nil {
			return nil, e
		}
		kept := []string{}
		found := false
		for _, id := range ids {
			if id == sitesignin.Plugin {
				found = true
				if action == "uninstall" {
					continue
				}
			}
			kept = append(kept, id)
		}
		if action == "install" && !found {
			kept = append(kept, sitesignin.Plugin)
		}
		encoded, _ := json.Marshal(kept)
		out := map[string]string{"UserInstalledPlugins": string(encoded)}
		if action != "uninstall" {
			out["plugin."+sitesignin.Plugin] = next
		}
		return out, nil
	})
	if e != nil {
		signinError(w, e)
		return
	}
	if action != "install" || !installed {
		if c, e := api.store.Control(r.Context()); e == nil && c.Scope.ID != "" {
			_ = api.store.StopControl(r.Context(), c.Scope.ID, "签到配置或安装状态已更新，请重新核对执行范围")
			api.cancelScope(c.Scope.ID)
		}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "签到设置已保存，原记录保留；在签到管理中核对后执行"})
}
func (api *siteSigninAPI) pluginPage(w http.ResponseWriter, r *http.Request, actor string) {
	status, e := api.status(r.Context(), actor, r.Header.Get("Authorization"), 1)
	if e != nil {
		signinError(w, e)
		return
	}
	data := pluginPageData{Title: "自动签到记录", ReadOnly: true, Sections: []string{"这里只读查看原记录。到站点页或本页签到管理核对目标、执行、停止或处理挑战。", "原 Python 日历史保留，不作为 Go 成功凭据；未知动作和中断任务不自动重试。", "当前 Go 接管有效：" + strconv.FormatBool(status.Control.Current)}, Tables: []pluginPageTable{}}
	table := pluginPageTable{Columns: []string{"稳定编号", "站点", "日期", "状态", "结果", "说明"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, j := range status.Jobs {
		table.Rows = append(table.Rows, []string{j.ID, j.Name, j.Day, j.State, j.Outcome.State, j.Outcome.Reason})
		table.RowActions = append(table.RowActions, nil)
	}
	data.Tables = append(data.Tables, table)
	signinJSON(w, data)
}
