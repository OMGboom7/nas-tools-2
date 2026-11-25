package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

func (s pluginService) nativeTorrentTransfer(id string) bool {
	api := s.torrentTransfer
	return strings.TrimSpace(id) == torrenttransfer.Plugin && s.system != nil && api != nil && api.login != nil && api.login.pureGo && api.downloaders != nil
}

type transferPluginConfig struct {
	detail            pluginConfigDetail
	raw, installedRaw string
	installed         []string
	choices           map[string]bool
}

func (s pluginService) transferConfig(ctx context.Context, actor string) (transferPluginConfig, error) {
	var state transferPluginConfig
	values, err := s.system.ReadMany(ctx, []string{"UserInstalledPlugins", torrenttransfer.ConfigKey})
	if err != nil {
		return state, err
	}
	state.raw, state.installedRaw = values[torrenttransfer.ConfigKey], values["UserInstalledPlugins"]
	state.installed, err = decodeInstalledPlugins(state.installedRaw)
	if err != nil {
		return state, err
	}
	cfg, err := torrenttransfer.Parse(state.raw)
	if err != nil {
		return state, err
	}
	items, err := s.torrentTransfer.downloaders.List(ctx)
	if err != nil {
		return state, err
	}
	choices := []pluginConfigChoice{}
	state.choices = map[string]bool{}
	for _, item := range items {
		if item.Enabled == 1 && (item.Type == "qbittorrent" || item.Type == "transmission") {
			id := strconv.FormatInt(item.ID, 10)
			choices = append(choices, pluginConfigChoice{Value: id, Label: item.Name})
			state.choices[id] = true
		}
	}
	authority, _, err := s.torrentTransfer.login.authority(actor)
	if err != nil {
		return state, err
	}
	state.detail = pluginConfigDetail{ID: torrenttransfer.Plugin, Name: "自动转移做种", Fields: []pluginConfigField{
		{Key: "enable", Title: "启用", Type: "switch"}, {Key: "cron", Title: "执行周期", Type: "text", Placeholder: "0 */6 * * *"},
		{Key: "onlyonce", Title: "运行一次", Type: "switch"}, {Key: "notify", Title: "发送结果通知", Type: "switch"},
		{Key: "fromdownloader", Title: "源下载器", Type: "multiselect", Options: choices}, {Key: "todownloader", Title: "目标下载器", Type: "multiselect", Options: choices},
		{Key: "fromtorrentpath", Title: "源种子文件目录", Type: "text"},
		{Key: "frompath", Title: "源下载路径", Type: "text"}, {Key: "topath", Title: "目标下载路径", Type: "text"},
		{Key: "nolabels", Title: "排除的标签（逗号分隔）", Type: "text"}, {Key: "nopaths", Title: "排除的目录（每行一个）", Type: "textarea"},
		{Key: "autostart", Title: "校验后自动启动", Type: "switch"}, {Key: "deletesource", Title: "移除源下载任务并保留文件", Type: "switch"},
	}, Meta: pluginConfigDetailMeta{Fingerprint: torrenttransfer.Hash([]any{actor, authority, state.raw, state.installedRaw, choices})}}
	b, err := json.Marshal(cfg)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(b, &state.detail.Values)
	return state, err
}

func (s pluginService) serveTorrentTransferPlugin(w http.ResponseWriter, r *http.Request, action string) {
	actor, ok := s.torrentTransfer.login.actor(w, r, action != "get" && action != "status")
	if !ok {
		return
	}
	ctx := r.Context()
	st, err := s.transferConfig(ctx, actor)
	if err != nil {
		writeAPIError(w, 409, 409, "无法读取转种配置")
		return
	}
	if action != "install" && action != "uninstall" && !slices.Contains(st.installed, torrenttransfer.Plugin) {
		writeAPIError(w, 404, 404, "自动转移做种尚未安装")
		return
	}
	changes := map[string]string{}
	switch action {
	case "get":
		signinJSON(w, st.detail)
		return
	case "status":
		writeJSON(w, 200, map[string]any{"code": 0, "state": false, "stateKnown": true})
		return
	case "install", "uninstall":
		list := slices.DeleteFunc(st.installed, func(id string) bool { return id == torrenttransfer.Plugin })
		if action == "install" {
			list = append(list, torrenttransfer.Plugin)
		}
		raw, err := json.Marshal(list)
		if err != nil {
			writeAPIError(w, 409, 409, "插件列表无效")
			return
		}
		changes["UserInstalledPlugins"] = string(raw)
	case "save", "legacy-save":
		var in struct {
			Fingerprint string                     `json:"fingerprint"`
			Values      map[string]json.RawMessage `json:"values"`
			ClearConfig []string                   `json:"clearConfig"`
		}
		if action == "legacy-save" {
			if err := json.Unmarshal([]byte(r.FormValue("config")), &in.Values); err != nil {
				writeAPIError(w, 400, 400, "配置格式无效")
				return
			}
			in.Fingerprint = r.FormValue("fingerprint")
			if in.Fingerprint == "" {
				in.Fingerprint = st.detail.Meta.Fingerprint
			}
		} else if !siteLoginJSON(w, r, &in) {
			return
		}
		if in.Fingerprint != st.detail.Meta.Fingerprint || in.Values == nil || len(in.ClearConfig) > 0 {
			writeAPIError(w, 409, 409, "请刷新并重新核对转种配置")
			return
		}
		next, err := torrenttransfer.Merge(st.raw, in.Values)
		if err != nil {
			writeAPIError(w, 400, 400, "转种配置字段或下载器编号无效")
			return
		}
		cfg, err := torrenttransfer.Parse(next)
		if err != nil || cfg.Enable && cfg.Ready() != nil {
			writeAPIError(w, 400, 400, "启用转种需要两个不同的下载器和有效的源种子目录")
			return
		}
		if cfg.Enable && (!st.choices[cfg.From[0]] || !st.choices[cfg.To[0]]) {
			writeAPIError(w, 400, 400, "请选择可用的源和目标下载器")
			return
		}
		if cfg.Cron != "" {
			loc, err := rssWorkerLocation()
			if err != nil {
				writeAPIError(w, 409, 409, "无法读取调度时区")
				return
			}
			if _, err := nextSyncTimer(organization.MonitorTimer{Cron: cfg.Cron, Timezone: loc.String()}, time.Now()); err != nil {
				writeAPIError(w, 400, 400, "执行周期需要有效的五段 cron")
				return
			}
		}
		changes[torrenttransfer.ConfigKey] = next
	default:
		writeAPIError(w, 404, 404, "未找到转种操作")
		return
	}
	err = s.system.UpdateMany(ctx, []string{"UserInstalledPlugins", torrenttransfer.ConfigKey}, func(current map[string]string) (map[string]string, error) {
		if current["UserInstalledPlugins"] != st.installedRaw || current[torrenttransfer.ConfigKey] != st.raw {
			return nil, torrenttransfer.ErrState
		}
		return changes, nil
	})
	if err != nil {
		writeAPIError(w, 409, 409, "配置保存失败，请刷新后核对")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "已保存"})
}
