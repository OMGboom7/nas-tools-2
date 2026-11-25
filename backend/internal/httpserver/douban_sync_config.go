package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

const doubanSettingsKey = "plugin.DoubanSync"
const doubanControlKey = "GoDoubanSync"

var doubanKeys = []string{"UserInstalledPlugins", doubanSettingsKey, doubanControlKey, "DefaultRssSettingMOV", "DefaultRssSettingTV"}

type doubanControl struct {
	Actor, Scope, Last string
	Next, Once         time.Time
	Running            bool
	RemoteSince        time.Time
	RemoteChannels     string
}

func doubanState(raw string) (doubanControl, error) {
	var c doubanControl
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return c, err
		}
	}
	return c, nil
}

type doubanSyncAPI struct {
	runner        *rssRunAPI
	plugins       *pluginService
	subscriptions subscriptionService
	login         *siteLoginAPI
	pause         func(context.Context, time.Duration) error
}

func (api *doubanSyncAPI) available() bool {
	return api != nil && api.plugins.system != nil && api.login.pureGo && api.login.guards != nil && *api.login.guards != nil
}
func (s pluginService) nativeSubscriptionPlugin(id string) bool {
	return (id == douban.SyncPlugin || id == douban.RankPlugin || id == movieRandomID) && s.doubanSync.available()
}
func (api *doubanSyncAPI) scope(ctx context.Context, actor string, raw map[string]string) (string, error) {
	auth, cfg, err := api.login.authority(actor)
	if err != nil {
		return "", err
	}
	settings, err := douban.ReadSettings(raw[doubanSettingsKey])
	if err != nil {
		return "", err
	}
	scope := []any{auth, raw[doubanSettingsKey]}
	if settings.Search && !settings.Subscribe {
		err := api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
			digest, err := nativeSubscriptionConfiguration(ctx, tx, api.subscriptions)
			if err == nil {
				scope = append(scope, digest)
			}
			return err
		})
		if err != nil {
			return "", err
		}
	} else if settings.Subscribe {
		app := objectValue(cfg["app"])
		media := objectValue(cfg["media"])
		scope = append(scope, app["tmdb_domain"], app["rmt_tmdbkey"], media["tmdb_language"])
		if settings.Search {
			scope = append(scope, media, cfg[strings.ToLower(strings.TrimSpace(text(media["media_server"])))])
		}
	}
	return sitelogin.Hash(scope), nil
}
func (api *doubanSyncAPI) summary(ctx context.Context, raw map[string]string) (bool, bool) {
	c, err := doubanState(raw[doubanControlKey])
	if err != nil {
		return false, false
	}
	if !c.Running && c.RemoteSince.IsZero() && c.Once.IsZero() && c.Next.IsZero() {
		return false, true
	}
	if !c.Running && !c.RemoteSince.IsZero() && c.Once.IsZero() && c.Next.IsZero() {
		_, digest, e := api.commandSources(ctx)
		if e != nil || digest != c.RemoteChannels {
			return false, e == nil
		}
	}
	s, err := api.scope(ctx, c.Actor, raw)
	return err == nil && s == c.Scope, err == nil
}

func doubanDetail(actor string, raw map[string]string, cfg douban.Settings) pluginConfigDetail {
	mode := "0"
	if cfg.Recent {
		mode = "1"
	}
	options := []pluginConfigChoice{{"wish", "想看"}, {"do", "在看"}, {"collect", "看过"}}
	fields := []pluginConfigField{
		{Key: "enable", Title: "启用定时同步", Type: "switch"},
		{Key: "go_remote", Title: "允许从已配置的交互渠道发送 /db 同步", Type: "switch", Tooltip: "沿用渠道的发送者权限。更换渠道后，请重新保存。"},
		{Key: "onlyonce", Title: "保存后同步一次", Type: "switch"},
		{Key: "users", Title: "豆瓣用户 ID", Type: "text", Placeholder: "多个用户用逗号隔开"},
		{Key: "types", Title: "同步标记", Type: "multiselect", Options: options},
		{Key: "sync_type", Title: "同步方式", Type: "select", Options: []pluginConfigChoice{{"0", "全量片单"}, {"1", "近期动态"}}},
		{Key: "days", Title: "同步最近多少天", Type: "text", Placeholder: "0 表示全部"},
		{Key: "interval", Title: "全量同步间隔（小时）", Type: "text"},
		{Key: "rss_interval", Title: "近期动态间隔（秒）", Type: "text", Placeholder: "最短 300 秒"},
		{Key: "cookie", Title: "豆瓣 Cookie", Type: "password", WriteOnly: true, Configured: cfg.Cookie != "", Tooltip: "仅发送至豆瓣电影，用于读取片单或详情。"},
		{Key: "auto_search", Title: "自动搜索", Type: "switch"},
		{Key: "auto_rss", Title: "自动添加订阅", Type: "switch"},
	}
	for i := range fields {
		if fields[i].Options == nil {
			fields[i].Options = []pluginConfigChoice{}
		}
	}
	return pluginConfigDetail{ID: douban.SyncPlugin, Name: "豆瓣同步", Fields: fields, Values: map[string]any{"go_remote": cfg.Remote, "enable": cfg.Enabled, "onlyonce": cfg.Once, "users": strings.Join(cfg.Users, ","), "types": append([]string{}, cfg.Types...), "sync_type": mode, "days": strconv.Itoa(cfg.Days), "interval": strconv.Itoa(cfg.Hours), "rss_interval": strconv.Itoa(cfg.Seconds), "auto_search": cfg.Search, "auto_rss": cfg.Subscribe}, Meta: pluginConfigDetailMeta{HasPage: true, Fingerprint: sitelogin.Hash([]any{actor, raw})}}
}
func (api *doubanSyncAPI) changes(ctx context.Context, actor string, raw map[string]string, in map[string]json.RawMessage, clear []string, now time.Time) (map[string]string, error) {
	v := map[string]json.RawMessage{}
	if old := raw[doubanSettingsKey]; old != "" && (json.Unmarshal([]byte(old), &v) != nil || v == nil) {
		return nil, douban.ErrSettings
	}
	for k, value := range in {
		switch k {
		case "go_remote", "enable", "onlyonce", "users", "types", "sync_type", "days", "interval", "rss_interval", "auto_search", "auto_rss", "cookie":
		default:
			return nil, douban.ErrSettings
		}
		v[k] = value
	}
	if len(clear) > 0 {
		if len(clear) != 1 || clear[0] != "cookie" {
			return nil, douban.ErrSettings
		}
		delete(v, "cookie")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	cfg, err := douban.ReadSettings(string(b))
	if err != nil {
		return nil, err
	}
	c := doubanControl{}
	if cfg.Enabled || cfg.Once || cfg.Remote {
		if !cfg.Valid() || (cfg.Enabled || cfg.Remote) && cfg.Interval() == 0 {
			return nil, douban.ErrSettings
		}
		if cfg.Remote {
			_, digest, e := api.commandSources(ctx)
			if e != nil {
				return nil, e
			}
			c.RemoteChannels = digest
			c.RemoteSince = now.UTC()
		}
		if cfg.Enabled {
			c.Next = now.Add(cfg.Interval()).UTC()
		}
		if cfg.Once {
			c.Once = now.Add(3 * time.Second).UTC()
			v["onlyonce"] = json.RawMessage("false")
			b, err = json.Marshal(v)
			if err != nil {
				return nil, err
			}
		}
		c.Actor = actor
		next := maps.Clone(raw)
		next[doubanSettingsKey] = string(b)
		c.Scope, err = api.scope(ctx, actor, next)
		if err != nil {
			return nil, err
		}
	}
	control, err := json.Marshal(c)
	return map[string]string{doubanSettingsKey: string(b), doubanControlKey: string(control)}, err
}

func (api *doubanSyncAPI) serve(w http.ResponseWriter, r *http.Request, id, action string) {
	p := api.plugin(id)
	if !api.plugins.historyReader(w, r) {
		return
	}
	claims, err := api.login.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "请重新登录")
		return
	}
	ctx := r.Context()
	raw, err := api.plugins.system.ReadMany(ctx, p.keys)
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	ids, err := decodeInstalledPlugins(raw["UserInstalledPlugins"])
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	installed := slices.Contains(ids, p.id)
	changes := map[string]string{}
	switch action {
	case "install", "uninstall":
		if action == "install" && !installed {
			ids = append(ids, p.id)
			changes[p.control] = "{}"
		}
		if action == "uninstall" {
			ids = slices.DeleteFunc(ids, func(id string) bool { return id == p.id })
			changes[p.control] = "{}"
		}
		b, err := json.Marshal(ids)
		if err != nil || len(ids) > 1024 {
			writeAPIError(w, 409, 409, "已安装插件列表无效")
			return
		}
		changes["UserInstalledPlugins"] = string(b)
	default:
		if !installed {
			writeAPIError(w, 404, 404, "未安装该订阅插件")
			return
		}
		switch action {
		case "get":
			detail, err := p.detail(claims.Username, raw)
			if err != nil {
				writeAPIError(w, 409, 409, "原订阅插件设置无效")
				return
			}
			signinJSON(w, detail)
			return
		case "status":
			running, known := p.summary(ctx, raw)
			writeJSON(w, 200, map[string]any{"code": 0, "state": running, "stateKnown": known})
			return
		case "save", "legacy-save":
			var in struct {
				Fingerprint string
				Values      map[string]json.RawMessage
				ClearConfig []string
			}
			want := sitelogin.Hash([]any{claims.Username, raw})
			if action == "legacy-save" {
				if json.Unmarshal([]byte(r.FormValue("config")), &in.Values) != nil {
					writeAPIError(w, 400, 400, "订阅插件设置无效")
					return
				}
				in.Fingerprint = r.FormValue("fingerprint")
				if in.Fingerprint == "" {
					in.Fingerprint = want
				}
			} else if !siteLoginJSON(w, r, &in) {
				return
			}
			if in.Values == nil || in.Fingerprint != want {
				writeAPIError(w, 409, 409, "设置已变化，请刷新")
				return
			}
			changes, err = p.changes(ctx, claims.Username, raw, in.Values, in.ClearConfig, time.Now())
			if err != nil {
				writeAPIError(w, 400, 400, "请核对插件设置、来源和执行周期")
				return
			}
		default:
			writeAPIError(w, 404, 404, "未知订阅插件操作")
			return
		}
	}
	err = api.plugins.system.UpdateMany(ctx, p.keys, func(current map[string]string) (map[string]string, error) {
		if !maps.Equal(current, raw) {
			return nil, douban.ErrSettings
		}
		return changes, nil
	})
	if err != nil {
		writeAPIError(w, 409, 409, "豆瓣设置未保存，请刷新核对")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": p.name + "设置已保存"})
}
