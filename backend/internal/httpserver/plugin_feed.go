package httpserver

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

// Feed plugins share installation and configuration editing. Scheduled plugins
// also have a control key that is reset on installation and removal.
type feedPluginDefinition struct {
	id, name, control string
	keys              []string
	detail            func(string, map[string]string) (pluginConfigDetail, error)
	summary           func(context.Context, map[string]string) (bool, bool)
	changes           func(context.Context, string, map[string]string, map[string]json.RawMessage, []string, time.Time) (map[string]string, error)
}

func (service pluginService) feedPlugin(id string) feedPluginDefinition {
	if id == mteamFeedID {
		return service.mteamFeed.plugin()
	}
	api := service.doubanSync
	if id == movieRandomID {
		return feedPluginDefinition{id, "电影随机订阅", movieRandomControlKey, movieRandomKeys, movieRandomDetail, api.movieRandomSummary, api.movieRandomChanges}
	}
	if id == douban.RankPlugin {
		return feedPluginDefinition{id, "豆瓣榜单订阅", rankControlKey, rankKeys, rankDetail, api.rankSummary, api.rankChanges}
	}
	return feedPluginDefinition{douban.SyncPlugin, "豆瓣同步", doubanControlKey, doubanKeys, func(actor string, raw map[string]string) (pluginConfigDetail, error) {
		cfg, err := douban.ReadSettings(raw[doubanSettingsKey])
		return doubanDetail(actor, raw, cfg), err
	}, api.summary, api.changes}
}

func (s pluginService) nativeFeedPlugin(id string) bool {
	if id == mteamFeedID {
		return s.mteamFeed != nil
	}
	return (id == douban.SyncPlugin || id == douban.RankPlugin || id == movieRandomID) && s.doubanSync.available()
}
func (service pluginService) serveFeedPlugin(w http.ResponseWriter, r *http.Request, id, action string) {
	p := service.feedPlugin(id)
	if !service.historyReader(w, r) {
		return
	}
	claims, err := service.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "请重新登录")
		return
	}
	ctx := r.Context()
	raw, err := service.system.ReadMany(ctx, p.keys)
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
			if p.control != "" {
				changes[p.control] = "{}"
			}
		}
		if action == "uninstall" {
			ids = slices.DeleteFunc(ids, func(id string) bool { return id == p.id })
			if p.control != "" {
				changes[p.control] = "{}"
			}
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
	err = service.system.UpdateMany(ctx, p.keys, func(current map[string]string) (map[string]string, error) {
		if !maps.Equal(current, raw) {
			return nil, douban.ErrSettings
		}
		return changes, nil
	})
	if err != nil {
		writeAPIError(w, 409, 409, "插件设置未保存，请刷新核对")
		return
	}
	if id == mteamFeedID {
		if err := service.mteamFeed.reload(); err != nil {
			writeAPIError(w, 503, 503, "设置已保存，RSS 服务未启动。请检查 8001 端口，服务将自动重试。")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": p.name + "设置已保存"})
}
