package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/autobackup"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func (s pluginService) nativeAutoBackup(id string) bool {
	return backupPluginID(id) && s.autoBackup != nil && s.autoBackup.store != nil
}

func (api *autoBackupAPI) summary(ctx context.Context) (bool, bool) {
	st, err := api.store.State(ctx)
	if err != nil {
		return false, false
	}
	if !api.login.pureGo || st.Control.Actor == "" {
		return false, true
	}
	s, err := api.readScope(ctx, st.Control.Actor, false)
	return err == nil && backupAuthorized(s), err == nil
}

func backupSettingsFingerprint(actor string, state autobackup.State) string {
	return sitelogin.Hash([]any{"auto-backup-settings", actor, state.Raw, state.Installed})
}

func (api *autoBackupAPI) detail(actor string, state autobackup.State) pluginConfigDetail {
	s, _ := autobackup.Parse(state.Raw)
	fields := []pluginConfigField{
		{Key: "enabled", Title: "开启定时备份", Type: "switch"},
		{Key: "full", Title: "完整备份", Type: "switch"},
		{Key: "notify", Title: "发送备份完成通知", Type: "switch"},
		{Key: "onlyonce", Title: "接管后运行一次", Type: "switch"},
		{Key: "cron", Title: "备份周期", Type: "text", Placeholder: "0 3 * * *"},
		{Key: "cnt", Title: "最多保留 Go 备份数", Type: "text", Placeholder: "留空时保留全部"},
	}
	values := map[string]any{"enabled": s.Enabled, "full": s.Full, "notify": s.Notify, "onlyonce": s.OnlyOnce, "cron": s.Cron, "cnt": s.Keep}
	if !api.container {
		fields = append(fields, pluginConfigField{Key: "bk_path", Title: "自定义备份目录", Type: "text", Placeholder: "留空使用配置目录下的 backup_file"})
		values["bk_path"] = s.Path
	}
	for i := range fields {
		fields[i].Options = []pluginConfigChoice{}
	}
	return pluginConfigDetail{ID: autobackup.Plugin, Name: "自动备份", Fields: fields, Values: values, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: backupSettingsFingerprint(actor, state)}}
}

func (s pluginService) serveAutoBackupPlugin(w http.ResponseWriter, r *http.Request, action string) {
	api := s.autoBackup
	actor, ok := api.actor(w, r, action != "get" && action != "page" && action != "status")
	if !ok {
		return
	}
	ctx := r.Context()
	state, err := api.store.State(ctx)
	if err != nil {
		backupError(w, err)
		return
	}
	if action == "install" || action == "uninstall" {
		next := state
		next.Installed = action == "install"
		if !next.Installed {
			next.Control = autobackup.Control{}
		}
		if err := api.change(ctx, state, next, nil); err != nil {
			backupError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "自动备份插件状态已保存"})
		return
	}
	if !state.Installed {
		backupError(w, sql.ErrNoRows)
		return
	}
	switch action {
	case "get":
		signinJSON(w, api.detail(actor, state))
	case "status":
		running, known := api.summary(ctx)
		writeJSON(w, 200, map[string]any{"code": 0, "state": running, "stateKnown": known})
	case "page":
		runs, err := api.store.Runs(ctx, 50)
		if err != nil {
			backupError(w, err)
			return
		}
		rows := [][]string{}
		for _, run := range runs {
			if run.Actor == actor {
				rows = append(rows, []string{run.ID, run.Created, run.Message, run.Archive.Name, strconv.Itoa(run.Pruned)})
			}
		}
		signinJSON(w, pluginPageData{Title: "自动备份记录", Sections: []string{}, ReadOnly: true, Tables: []pluginPageTable{{Columns: []string{"编号", "时间", "结果", "归档", "清理数量"}, Rows: rows, RowActions: make([]*pluginPageRowAction, len(rows))}}})
	case "save", "legacy-save":
		var in struct {
			Fingerprint string                     `json:"fingerprint"`
			Values      map[string]json.RawMessage `json:"values"`
			ClearConfig []string                   `json:"clearConfig"`
		}
		if action == "legacy-save" {
			if json.Unmarshal([]byte(r.FormValue("config")), &in.Values) != nil {
				backupError(w, autobackup.ErrSettings)
				return
			}
			in.Fingerprint = r.FormValue("fingerprint")
			if in.Fingerprint == "" {
				in.Fingerprint = backupSettingsFingerprint(actor, state)
			}
		} else if !siteLoginJSON(w, r, &in) {
			return
		}
		if in.Fingerprint != backupSettingsFingerprint(actor, state) || len(in.ClearConfig) != 0 {
			backupError(w, autobackup.ErrScope)
			return
		}
		raw, err := autobackup.Merge(state.Raw, in.Values)
		if err != nil {
			backupError(w, err)
			return
		}
		cfg, err := autobackup.Parse(raw)
		if err != nil {
			backupError(w, err)
			return
		}
		location, err := rssWorkerLocation()
		if err != nil {
			backupError(w, err)
			return
		}
		if _, err := cookieCloudSchedule(cfg.Cron, location, time.Now()); err != nil {
			backupError(w, autobackup.ErrSettings)
			return
		}
		next := state
		next.Raw = raw
		if raw != state.Raw {
			next.Control = autobackup.Control{}
		}
		if err := api.change(ctx, state, next, nil); err != nil {
			backupError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "备份设置已保存"})
	default:
		backupError(w, sql.ErrNoRows)
	}
}
