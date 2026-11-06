package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentremover"
)

func (s pluginService) nativeTorrentRemover(id string) bool {
	return strings.TrimSpace(id) == torrentremover.Plugin && s.torrentRemover != nil && s.torrentRemover.store != nil
}
func (api *torrentRemovalAPI) summary(ctx context.Context) (bool, bool) {
	st, err := api.store.Settings(ctx)
	if err != nil {
		return false, false
	}
	if !st.Control.Valid() {
		return false, true
	}
	scope, err := api.scope(ctx, st.Control.Actor)
	return err == nil && api.login.pureGo && st.Installed && scope.fingerprint() == st.Control.Scope, err == nil
}
func (api *torrentRemovalAPI) detail(actor string, st torrentremover.Settings, authority string) (pluginConfigDetail, error) {
	enabled, err := torrentremover.Enabled(st.Raw)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	return pluginConfigDetail{ID: torrentremover.Plugin, Name: "下载任务联动删除", Fields: []pluginConfigField{{Key: "enable", Title: "开启下载任务联动删除", Type: "switch", Tooltip: "保存后请回到联动删除区域审核下载器并启用。只处理以后产生的已核验源删除事实，保留全部文件。"}}, Values: map[string]any{"enable": enabled}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: torrentremover.Hash([]any{actor, authority, st.Raw, st.InstalledRaw})}}, nil
}
func (s pluginService) serveTorrentRemoverPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.torrentRemover
	write := action != "get" && action != "page" && action != "status"
	actor, ok := api.actor(w, r, write)
	if !ok {
		return
	}
	ctx := r.Context()
	st, err := api.store.Settings(ctx)
	if err != nil {
		removalError(w)
		return
	}
	authority, _, err := api.login.authority(actor)
	if err != nil {
		removalError(w)
		return
	}
	detail, err := api.detail(actor, st, authority)
	if err != nil {
		removalError(w)
		return
	}
	if !st.Installed && action != "install" && action != "uninstall" {
		writeAPIError(w, 404, 404, "联动删除尚未安装")
		return
	}
	switch action {
	case "get":
		signinJSON(w, detail)
	case "status":
		active, known := api.summary(ctx)
		writeJSON(w, 200, map[string]any{"code": 0, "state": active, "stateKnown": known})
	case "page":
		records, err := api.store.List(ctx, "")
		if err != nil {
			removalError(w)
			return
		}
		rows := [][]string{}
		for _, record := range records {
			if record.Actor == actor {
				rows = append(rows, []string{record.ID, record.Definition.Event.Title, record.State, record.Reason})
			}
		}
		signinJSON(w, pluginPageData{Title: "下载任务联动删除记录", Sections: []string{"在插件页审核未来事件范围、停止后台或只读核对未知任务。"}, ReadOnly: true, Tables: []pluginPageTable{{Columns: []string{"编号", "媒体", "状态", "说明"}, Rows: rows, RowActions: make([]*pluginPageRowAction, len(rows))}}})
	case "save", "legacy-save":
		var input struct {
			Fingerprint string                     `json:"fingerprint"`
			Values      map[string]json.RawMessage `json:"values"`
			ClearConfig []string                   `json:"clearConfig"`
		}
		if action == "legacy-save" {
			if json.Unmarshal([]byte(r.FormValue("config")), &input.Values) != nil {
				removalError(w)
				return
			}
			input.Fingerprint = r.FormValue("fingerprint")
			if input.Fingerprint == "" {
				input.Fingerprint = detail.Meta.Fingerprint
			}
		} else if !siteLoginJSON(w, r, &input) {
			return
		}
		var enabled bool
		rawEnable, exists := input.Values["enable"]
		if input.Fingerprint != detail.Meta.Fingerprint || len(input.Values) != 1 || len(input.ClearConfig) != 0 || !exists || string(rawEnable) == "null" || json.Unmarshal(rawEnable, &enabled) != nil {
			removalError(w)
			return
		}
		raw, err := torrentremover.Merge(st.Raw, enabled)
		if err != nil || api.store.Change(ctx, st, &raw, nil, torrentremover.Control{}) != nil {
			removalError(w)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "设置已保存，原后台授权已撤销"})
	case "install", "uninstall":
		list, err := decodeInstalledPlugins(st.InstalledRaw)
		if err != nil {
			removalError(w)
			return
		}
		list = slices.DeleteFunc(list, func(id string) bool { return id == torrentremover.Plugin })
		if action == "install" {
			list = append(list, torrentremover.Plugin)
		}
		b, _ := json.Marshal(list)
		raw := string(b)
		if api.store.Change(ctx, st, nil, &raw, torrentremover.Control{}) != nil {
			removalError(w)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "插件状态已保存，后台需另行审核启用"})
	default:
		writeAPIError(w, 404, 404, "未找到联动删除操作")
	}
}
