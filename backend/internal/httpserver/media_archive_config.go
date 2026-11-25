package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaarchive"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

const archiveSettingsKey = "plugin." + mediaarchive.Plugin
const archiveControlKey = "GoMediaLibraryArchive"

var archiveKeys = []string{"UserInstalledPlugins", archiveSettingsKey, archiveControlKey}

type archiveControl struct {
	Actor      string
	Scope      string
	Next, Once time.Time
	Running    bool
	Last       string
}

func decodeArchiveControl(raw string) (archiveControl, error) {
	var c archiveControl
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return c, err
		}
	}
	return c, nil
}

func (s pluginService) nativeArchiveRunner(id string) bool {
	return s.nativeArchivePage(id) && s.archiveMedia != nil && s.archiveMedia.pureGo && s.archiveMedia.guards != nil
}

func archiveFingerprint(actor string, raw map[string]string) string {
	return sitelogin.Hash([]any{actor, raw})
}

type archiveSource struct {
	client      *mediaserver.Client
	user, scope string
	location    *time.Location
}

func (s pluginService) archiveSource(actor, raw string) (archiveSource, error) {
	login := siteLoginAPI{config: s.archiveMedia.config, auth: s.auth}
	authority, snapshot, err := login.authority(actor)
	if err != nil {
		return archiveSource{}, err
	}
	client, _, user, server, err := s.archiveMedia.connectionFromSnapshot(snapshot)
	if err != nil {
		return archiveSource{}, err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return archiveSource{}, err
	}
	scope := sitelogin.Hash([]any{authority, server, raw, loc.String(), s.applicationConfigPath, s.archiveContainer})
	return archiveSource{client: client, user: user, scope: scope, location: loc}, nil
}

func (s pluginService) archiveSettings(actor string, raw map[string]string, cfg mediaarchive.Settings) pluginConfigDetail {
	fields := []pluginConfigField{
		{Key: "enable", Title: "启用定时归档", Type: "switch"},
		{Key: "cron", Title: "归档周期", Type: "text", Placeholder: "0 0 * * *"},
		{Key: "run_once", Title: "保存后运行一次", Type: "switch"},
		{Key: "enable_notify", Title: "归档完成时通知", Type: "switch"},
		{Key: "max_count", Title: "最大保留归档数", Type: "text", Placeholder: "10", Tooltip: "归档完成后会删除超出数量的最早归档，包括旧版创建的归档。填写 0 将清除所有归档。"},
	}
	values := map[string]any{"enable": cfg.Enabled, "cron": cfg.Cron, "run_once": cfg.Once, "enable_notify": cfg.Notify, "max_count": cfg.Count}
	if !s.archiveContainer {
		fields = append(fields, pluginConfigField{Key: "archive_path", Title: "自定义归档目录", Type: "text", Placeholder: "默认使用配置目录下的 archive_file/media_library"})
		values["archive_path"] = cfg.Path
	}
	for i := range fields {
		fields[i].Options = []pluginConfigChoice{}
	}
	return pluginConfigDetail{ID: mediaarchive.Plugin, Name: "媒体库归档", Fields: fields, Values: values, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: archiveFingerprint(actor, raw)}}
}

func (s pluginService) archiveSave(ctx context.Context, raw map[string]string, changes map[string]string) error {
	return s.system.UpdateMany(ctx, archiveKeys, func(actual map[string]string) (map[string]string, error) {
		if !maps.Equal(raw, actual) {
			return nil, mediaarchive.ErrChanged
		}
		return changes, nil
	})
}

func (s pluginService) configureArchive(actor string, raw map[string]string, in map[string]json.RawMessage, now time.Time) (map[string]string, error) {
	values := map[string]json.RawMessage{}
	if v := raw[archiveSettingsKey]; v != "" && (json.Unmarshal([]byte(v), &values) != nil || values == nil) {
		return nil, mediaarchive.ErrSettings
	}
	for key, v := range in {
		switch key {
		case "enable", "cron", "run_once", "enable_notify", "max_count":
		case "archive_path":
			if s.archiveContainer {
				return nil, mediaarchive.ErrSettings
			}
		default:
			return nil, mediaarchive.ErrSettings
		}
		values[key] = v
	}
	updated, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	cfg, _, err := mediaarchive.Parse(string(updated))
	if err != nil {
		return nil, err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, err
	}
	next, err := cookieCloudSchedule(cfg.Cron, loc, now)
	if err != nil {
		return nil, mediaarchive.ErrSettings
	}
	c := archiveControl{}
	if cfg.Enabled && !next.IsZero() {
		c.Next = next.UTC()
	}
	if cfg.Once {
		c.Once = now.Add(3 * time.Second).UTC()
		values["run_once"] = json.RawMessage("false")
		updated, err = json.Marshal(values)
		if err != nil {
			return nil, err
		}
	}
	if !c.Next.IsZero() || !c.Once.IsZero() {
		c.Actor = actor
		source, err := s.archiveSource(actor, string(updated))
		c.Scope = source.scope
		if err != nil {
			return nil, err
		}
	}
	control, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return map[string]string{archiveSettingsKey: string(updated), archiveControlKey: string(control)}, nil
}

func (s pluginService) serveArchivePlugin(w http.ResponseWriter, r *http.Request, action string) {
	admin, ok := s.catalogReader(w, r)
	if !ok {
		return
	}
	if !admin && action != "status" {
		writeAPIError(w, 403, 403, "归档配置需要管理员权限")
		return
	}
	ctx := r.Context()
	w.Header().Set("Cache-Control", "private, no-store")
	claims, err := s.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "登录已过期")
		return
	}
	raw, err := s.system.ReadMany(ctx, archiveKeys)
	if err != nil {
		archivePageError(w, err)
		return
	}
	list, err := decodeInstalledPlugins(raw["UserInstalledPlugins"])
	if err != nil {
		archivePageError(w, err)
		return
	}
	installed := slices.Contains(list, mediaarchive.Plugin)
	if action == "install" || action == "uninstall" {
		changes := map[string]string{}
		if action == "install" && !installed {
			list = append(list, mediaarchive.Plugin)
			changes[archiveControlKey] = "{}"
		}
		if action == "uninstall" {
			list = slices.DeleteFunc(list, func(id string) bool { return id == mediaarchive.Plugin })
			changes[archiveControlKey] = "{}"
		}
		b, err := json.Marshal(list)
		if err != nil || len(list) > 1024 {
			archivePageError(w, mediaarchive.ErrChanged)
			return
		}
		changes["UserInstalledPlugins"] = string(b)
		if err = s.archiveSave(ctx, raw, changes); err != nil {
			archivePageError(w, err)
			return
		}
	} else {
		if !installed {
			writeAPIError(w, 404, 404, "未安装媒体库归档插件")
			return
		}
		switch action {
		case "status":
			running, known := s.archiveStatus(raw)
			writeJSON(w, 200, map[string]any{"code": 0, "state": running, "stateKnown": known})
			return
		case "get":
			cfg, _, err := mediaarchive.Parse(raw[archiveSettingsKey])
			if err != nil {
				archivePageError(w, err)
				return
			}
			signinJSON(w, s.archiveSettings(claims.Username, raw, cfg))
			return
		case "save", "legacy-save":
			var in struct {
				Fingerprint string
				Values      map[string]json.RawMessage
				ClearConfig []string
			}
			if action == "legacy-save" {
				if json.Unmarshal([]byte(r.FormValue("config")), &in.Values) != nil {
					writeAPIError(w, 400, 400, "归档设置无效")
					return
				}
				in.Fingerprint = r.FormValue("fingerprint")
				if in.Fingerprint == "" {
					in.Fingerprint = archiveFingerprint(claims.Username, raw)
				}
			} else if !siteLoginJSON(w, r, &in) {
				return
			}
			if in.Values == nil || len(in.ClearConfig) != 0 {
				writeAPIError(w, 400, 400, "归档设置无效")
				return
			}
			if in.Fingerprint != archiveFingerprint(claims.Username, raw) {
				archivePageError(w, mediaarchive.ErrChanged)
				return
			}
			changes, err := s.configureArchive(claims.Username, raw, in.Values, time.Now())
			if errors.Is(err, mediaarchive.ErrSettings) {
				writeAPIError(w, 400, 400, "归档周期、数量或设置无效")
				return
			}
			if err == nil {
				err = s.archiveSave(ctx, raw, changes)
			}
			if err != nil {
				archivePageError(w, err)
				return
			}
		default:
			writeAPIError(w, 404, 404, "未知归档操作")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "媒体库归档设置已保存"})
}

func (s pluginService) archiveStatus(raw map[string]string) (bool, bool) {
	c, err := decodeArchiveControl(raw[archiveControlKey])
	if err != nil {
		return false, false
	}
	if c.Actor == "" || c.Next.IsZero() && c.Once.IsZero() && !c.Running {
		return false, true
	}
	source, err := s.archiveSource(c.Actor, raw[archiveSettingsKey])
	return err == nil && source.scope == c.Scope, err == nil
}
