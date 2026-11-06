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
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
)

func (s pluginService) nativeTorrentMark(id string) bool {
	return strings.TrimSpace(id) == "TorrentMark" && s.torrentMark != nil && s.system != nil
}
func (api *torrentMarkAPI) summary(ctx context.Context) (bool, bool) {
	st, err := api.state(ctx)
	if err != nil {
		return false, false
	}
	if st.Control.Actor == "" {
		return false, true
	}
	s, err := api.scope(ctx, st.Control.Actor)
	return err == nil && api.login.pureGo && s.installed && st.Control.Scope == s.fingerprint() && (st.Control.Next != "" || st.Control.Once != ""), err == nil
}
func (api *torrentMarkAPI) detail(ctx context.Context, actor string) (pluginConfigDetail, string, string, bool, error) {
	values, err := api.system.ReadMany(ctx, []string{"UserInstalledPlugins", markConfigKey})
	if err != nil {
		return pluginConfigDetail{}, "", "", false, err
	}
	raw, installedRaw := values[markConfigKey], values["UserInstalledPlugins"]
	list, err := decodeInstalledPlugins(installedRaw)
	if err != nil {
		return pluginConfigDetail{}, "", "", false, err
	}
	cfg, err := parseMark(raw)
	if err != nil {
		return pluginConfigDetail{}, "", "", false, err
	}
	items, err := api.downloaders.List(ctx)
	if err != nil {
		return pluginConfigDetail{}, "", "", false, err
	}
	choices := []pluginConfigChoice{}
	for _, item := range items {
		if item.Enabled == 1 && (item.Type == "qbittorrent" || item.Type == "transmission") {
			choices = append(choices, pluginConfigChoice{Value: strconv.FormatInt(item.ID, 10), Label: item.Name})
		}
	}
	authority, _, err := api.login.authority(actor)
	if err != nil {
		return pluginConfigDetail{}, "", "", false, err
	}
	ids := cfg.Downloaders
	if ids == nil {
		ids = []string{}
	}
	d := pluginConfigDetail{ID: "TorrentMark", Name: "种子标记", Fields: []pluginConfigField{
		{Key: "enable", Title: "启用", Type: "switch"},
		{Key: "onlyonce", Title: "启用后运行一次", Type: "switch"},
		{Key: "cron", Title: "执行周期", Type: "text", Placeholder: "0 */6 * * *", Tooltip: "五段 cron。保存后请在种子标记区域重新确认并启用。"},
		{Key: "downloaders", Title: "下载器", Type: "multiselect", Options: choices},
	}, Values: map[string]any{"enable": cfg.Enable, "onlyonce": cfg.OnlyOnce, "cron": cfg.Cron, "downloaders": ids}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: organization.Digest([]any{actor, authority, raw, installedRaw})}}
	return d, raw, installedRaw, slices.Contains(list, "TorrentMark"), nil
}
func (s pluginService) serveTorrentMarkPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.torrentMark
	write := action != "get" && action != "status" && action != "page"
	actor, ok := api.actor(w, r, write)
	if !ok {
		return
	}
	ctx := r.Context()
	detail, raw, installedRaw, installed, err := api.detail(ctx, actor)
	if err != nil {
		markError(w, err)
		return
	}
	if action != "install" && action != "uninstall" && !installed {
		writeAPIError(w, 404, 404, "种子标记尚未安装")
		return
	}
	switch action {
	case "get":
		signinJSON(w, detail)
	case "status":
		active, known := api.summary(ctx)
		writeJSON(w, 200, map[string]any{"code": 0, "state": active, "stateKnown": known})
	case "page":
		st, err := api.state(ctx)
		if err != nil {
			markError(w, err)
			return
		}
		rows := [][]string{}
		for i := len(st.Runs) - 1; i >= 0; i-- {
			run := st.Runs[i]
			rows = append(rows, []string{run.ID, run.Kind, run.State, strconv.Itoa(run.Changed), strconv.Itoa(run.Skipped), strconv.Itoa(run.Unknown)})
		}
		signinJSON(w, pluginPageData{Title: "种子标记记录", Sections: []string{"请在插件页的种子标记区域核对下载器、执行或停止。结果未知的标签写入仅可只读核对。"}, ReadOnly: true, Tables: []pluginPageTable{{Columns: []string{"编号", "触发", "状态", "已标记", "跳过", "未知"}, Rows: rows, RowActions: make([]*pluginPageRowAction, len(rows))}}})
	case "save", "legacy-save", "install", "uninstall":
		next := raw
		if action == "save" || action == "legacy-save" {
			var in struct {
				Fingerprint string                     `json:"fingerprint"`
				Values      map[string]json.RawMessage `json:"values"`
				ClearConfig []string                   `json:"clearConfig"`
			}
			if action == "legacy-save" {
				if json.Unmarshal([]byte(r.FormValue("config")), &in.Values) != nil {
					markError(w, torrentmark.ErrEvidence)
					return
				}
				in.Fingerprint = r.FormValue("fingerprint")
				if in.Fingerprint == "" {
					in.Fingerprint = detail.Meta.Fingerprint
				}
			} else if !siteLoginJSON(w, r, &in) {
				return
			}
			if in.Fingerprint != detail.Meta.Fingerprint || len(in.ClearConfig) > 0 {
				markError(w, torrentmark.ErrEvidence)
				return
			}
			next, err = mergeMark(raw, in.Values)
			if err != nil {
				markError(w, err)
				return
			}
			cfg, _ := parseMark(next)
			loc, err := rssWorkerLocation()
			if err != nil {
				markError(w, err)
				return
			}
			if _, err := markSchedule(markScope{settings: cfg, location: loc}, time.Now()); err != nil {
				writeAPIError(w, 400, 400, "请输入有效的五段 cron 周期")
				return
			}
			allowed := map[string]bool{}
			for _, choice := range detail.Fields[3].Options {
				allowed[choice.Value] = true
			}
			for _, id := range cfg.Downloaders {
				if !allowed[id] {
					writeAPIError(w, 400, 400, "请选择可用的 qBittorrent 或 Transmission 下载器")
					return
				}
			}
		}
		err = api.update(ctx, func(values map[string]string, st *markState) error {
			if values[markConfigKey] != raw || values["UserInstalledPlugins"] != installedRaw {
				return torrentmark.ErrEvidence
			}
			if action == "install" || action == "uninstall" {
				list, err := decodeInstalledPlugins(installedRaw)
				if err != nil {
					return err
				}
				list = slices.DeleteFunc(list, func(id string) bool { return id == "TorrentMark" })
				if action == "install" {
					list = append(list, "TorrentMark")
				}
				b, _ := json.Marshal(list)
				values["UserInstalledPlugins"] = string(b)
			} else {
				values[markConfigKey] = next
			}
			st.Control = markControl{Epoch: sitelogin.Random()}
			for i := range st.Runs {
				if st.Runs[i].State == "running" {
					st.Runs[i].State = "stopped"
				}
			}
			return nil
		})
		if err != nil {
			markError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "种子标记配置已保存，请重新核对并启用"})
	default:
		writeAPIError(w, 404, 404, "未找到种子标记操作")
	}
}
