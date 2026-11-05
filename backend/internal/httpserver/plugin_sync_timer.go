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
)

func (s pluginService) nativeSyncTimer(id string) bool {
	return strings.TrimSpace(id) == organization.SyncTimerPlugin && s.syncTimer != nil && s.system != nil
}

func (s pluginService) syncTimerPluginSettings(ctx context.Context) (organization.SyncTimerSettings, error) {
	if s.syncTimer.jobs != nil {
		return s.syncTimer.jobs.SyncTimerSettings(ctx)
	}
	values, err := s.system.ReadMany(ctx, []string{"UserInstalledPlugins", organization.SyncTimerConfig})
	if err != nil {
		return organization.SyncTimerSettings{}, err
	}
	installed, err := decodeInstalledPlugins(values["UserInstalledPlugins"])
	return organization.SyncTimerSettings{Raw: values[organization.SyncTimerConfig], Installed: slices.Contains(installed, organization.SyncTimerPlugin)}, err
}

func syncTimerDetail(actor string, settings organization.SyncTimerSettings) pluginConfigDetail {
	cron, _ := syncTimerCron(settings.Raw)
	return pluginConfigDetail{
		ID: organization.SyncTimerPlugin, Name: "定时目录同步",
		Fields: []pluginConfigField{{Key: "cron", Title: "同步周期", Type: "text", Placeholder: "0 */2 * * *", Tooltip: "五段 cron。保存后请在整理页重新审核范围并启用，留空可关闭周期。", Options: []pluginConfigChoice{}}},
		Values: map[string]any{"cron": cron},
		Meta:   pluginConfigDetailMeta{HasPage: true, Fingerprint: organization.Digest([]any{actor, settings})},
	}
}

func (s pluginService) syncTimerRunning(ctx context.Context) (bool, bool) {
	api := s.syncTimer
	if api.jobs == nil || !api.pureGo {
		return false, true
	}
	control, err := api.jobs.MonitorControl(ctx)
	if err != nil {
		return false, false
	}
	if control.Lease.Timer == nil || !control.Enabled() || !api.pureGo {
		return false, true
	}
	if err := api.jobs.VerifyMonitorControl(ctx, control.Lease.Generation); err != nil {
		return false, true
	}
	if _, err := nextSyncTimer(*control.Lease.Timer, time.Now()); err != nil {
		return false, true
	}
	_, err = api.jobs.SyncTimerClock(ctx, control.Lease.Generation)
	return err == nil, err == nil
}

func (s pluginService) serveSyncTimerPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.syncTimer
	if !api.authorize(w, r) {
		return
	}
	switch action {
	case "get", "page", "status":
	default:
		if !api.pureGo || api.jobs == nil {
			writeAPIError(w, 501, 501, "请在 Go 独立运行模式修改定时目录同步")
			return
		}
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "请重新登录")
		return
	}
	ctx := r.Context()
	settings, err := s.syncTimerPluginSettings(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if action == "install" || action == "uninstall" {
		next := settings
		next.Installed = action == "install"
		if err := api.jobs.ChangeSyncTimer(ctx, settings, next, !next.Installed); err != nil {
			writeOrganizationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "定时目录同步插件状态已保存"})
		return
	}
	if !settings.Installed {
		writeOrganizationError(w, organization.ErrNotFound)
		return
	}
	detail := syncTimerDetail(claims.Username, settings)
	switch action {
	case "get":
		signinJSON(w, detail)
	case "status":
		running, known := s.syncTimerRunning(ctx)
		writeJSON(w, 200, map[string]any{"code": 0, "state": running, "stateKnown": known})
	case "page":
		var runs []organization.SyncTimerRun
		if api.jobs != nil {
			runs, err = api.jobs.SyncTimerRuns(ctx, "")
			if err != nil {
				writeOrganizationError(w, err)
				return
			}
		}
		rows := [][]string{}
		for _, run := range runs {
			rows = append(rows, []string{run.ID, run.Slot, run.State, strconv.Itoa(run.Candidates)})
		}
		signinJSON(w, pluginPageData{Title: "定时目录同步记录", Sections: []string{"在整理页的自动同步区域核对范围、启用定时并查看文件任务。"}, ReadOnly: true, Tables: []pluginPageTable{{Columns: []string{"编号", "周期", "状态", "候选范围"}, Rows: rows, RowActions: make([]*pluginPageRowAction, len(rows))}}})
	case "save", "legacy-save":
		var in struct {
			Fingerprint string                     `json:"fingerprint"`
			Values      map[string]json.RawMessage `json:"values"`
			ClearConfig []string                   `json:"clearConfig"`
		}
		if action == "legacy-save" {
			if json.Unmarshal([]byte(r.FormValue("config")), &in.Values) != nil {
				writeAPIError(w, 400, 400, "同步周期格式无效")
				return
			}
			in.Fingerprint = r.FormValue("fingerprint")
			if in.Fingerprint == "" {
				in.Fingerprint = detail.Meta.Fingerprint
			}
		} else if !siteLoginJSON(w, r, &in) {
			return
		}
		if in.Fingerprint != detail.Meta.Fingerprint || len(in.ClearConfig) != 0 || len(in.Values) != 1 {
			writeOrganizationError(w, organization.ErrState)
			return
		}
		field, exists := in.Values["cron"]
		if !exists {
			writeAPIError(w, 400, 400, "缺少同步周期")
			return
		}
		var cron string
		if json.Unmarshal(field, &cron) != nil {
			writeAPIError(w, 400, 400, "同步周期格式无效")
			return
		}
		cron = strings.TrimSpace(cron)
		if cron != "" {
			loc, err := rssWorkerLocation()
			if err != nil {
				writeOrganizationError(w, err)
				return
			}
			if _, err := nextSyncTimer(organization.MonitorTimer{Cron: cron, Timezone: loc.String()}, time.Now()); err != nil {
				writeAPIError(w, 400, 400, "请输入有效的五段 cron 周期")
				return
			}
		}
		fields := map[string]json.RawMessage{}
		if settings.Raw != "" && (json.Unmarshal([]byte(settings.Raw), &fields) != nil || fields == nil) {
			writeOrganizationError(w, organization.ErrState)
			return
		}
		fields["cron"], _ = json.Marshal(cron)
		raw, err := json.Marshal(fields)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		next := settings
		next.Raw = string(raw)
		if err := api.jobs.ChangeSyncTimer(ctx, settings, next, true); err != nil {
			writeOrganizationError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "周期已保存，请在整理页重新审核并启用"})
	default:
		writeOrganizationError(w, organization.ErrNotFound)
	}
}
