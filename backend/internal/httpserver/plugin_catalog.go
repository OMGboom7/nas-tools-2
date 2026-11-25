package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/0xforee/nas-tools/backend/internal/auth"
)

type pluginCatalogEntry struct {
	pluginSummary
	AuthLevel int    `json:"authLevel"`
	Icon      string `json:"icon"`
	Color     string `json:"color"`
}

var bundledPluginCatalog = sync.OnceValues(func() (map[string]pluginCatalogEntry, error) {
	var entries []pluginCatalogEntry
	if err := json.Unmarshal([]byte(nativePluginCatalogJSON), &entries); err != nil {
		return nil, err
	}
	catalog := make(map[string]pluginCatalogEntry, len(entries))
	for _, entry := range entries {
		catalog[entry.ID] = entry
	}
	return catalog, nil
})

func (service pluginService) catalogReader(response http.ResponseWriter, request *http.Request) (administrator, ok bool) {
	if service.auth == nil {
		writeAPIError(response, 503, 503, "native authentication is unavailable")
		return false, false
	}
	claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "authorization token is invalid or expired")
		return false, false
	}
	if _, err := service.auth.service.FindUser(request.Context(), claims.Username); err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			writeAPIError(response, 401, 401, "current account is unavailable")
		} else {
			writeAPIError(response, 503, 503, "account information is unavailable")
		}
		return false, false
	}
	return service.auth.service.IsAdministrator(claims.Username), true
}

func (service pluginService) serveNativePluginCatalog(response http.ResponseWriter, request *http.Request) {
	administrator, ok := service.catalogReader(response, request)
	if !ok {
		return
	}
	data, err := service.nativePluginCatalogData(request.Context(), administrator, true)
	if err != nil {
		writeAPIError(response, 502, 502, "plugin catalog or state is unavailable")
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "success": true, "data": data})
}

func (service pluginService) nativePluginCatalogData(ctx context.Context, administrator, readState bool) (pluginsData, error) {
	installed, err := service.nativeMetadataInstalled(ctx)
	if err != nil {
		return pluginsData{}, err
	}
	catalog, err := bundledPluginCatalog()
	if err != nil {
		return pluginsData{}, err
	}
	installedSet := map[string]bool{}
	for _, id := range installed {
		installedSet[id] = true
	}
	data := pluginsData{Items: []pluginSummary{}}
	for _, entry := range catalog {
		item := entry.pluginSummary
		if entry.AuthLevel > 1 && !administrator {
			continue
		}
		item.Installed = installedSet[item.ID]
		item.NativeHistoryPage = service.nativeSubscriptionHistory(item.ID)
		item.NativeArchivePage = service.nativeArchivePage(item.ID)
		if item.NativeHistoryPage || item.NativeArchivePage {
			item.HasPage = true
		}
		item.Native = service.ownsPlugin(item.ID)
		item.StateKnown = !item.Installed || item.Native && readState
		item.ActionsAvailable = item.Native || service.legacyURL != ""
		if item.ID == diskID && service.disk != nil || item.ID == autoSubID && service.autoSub != nil {
			item.HasPage = administrator
			item.ActionsAvailable = administrator
		}
		if item.ID == helperID && service.helper != nil {
			item.ActionsAvailable = administrator
		}
		if item.ID == "IYUUAutoSeed" && service.iyuu != nil {
			item.NativeBindingPage = administrator
			item.ActionsAvailable = administrator
			item.HasPage = administrator || !service.iyuu.transfer.login.pureGo
			if service.iyuu.transfer.login.pureGo {
				item.Description = "绑定 IYUU 合作站点，手动或定时匹配已完成的种子，暂停添加、校验后开始做种。"
			}
		}
		if service.nativeArchiveRunner(item.ID) {
			item.ActionsAvailable = administrator
		}
		if service.nativeCloudflare(item.ID) {
			item.ActionsAvailable = service.cloudflare.organizer.pureGo
		}
		if service.nativeTorrentTransfer(item.ID) {
			item.ActionsAvailable = administrator
		}
		if service.nativeTorrentRemover(item.ID) {
			item.ActionsAvailable = service.torrentRemover.login.pureGo && administrator
			item.HasPage = true
		}
		if service.nativeTorrentMark(item.ID) {
			item.ActionsAvailable = service.torrentMark.login.pureGo && administrator
			item.HasPage = true
		}
		if service.nativeSyncTimer(item.ID) {
			item.ActionsAvailable = service.syncTimer.pureGo && administrator
			item.HasPage = true
		}
		if service.nativeAutoBackup(item.ID) {
			item.ActionsAvailable = service.autoBackup.login.pureGo
			item.HasPage = true
		}
		if service.nativeCookieCloud(item.ID) {
			item.ActionsAvailable = service.cookieCloud.login.pureGo
			item.HasPage = true
		}
		if service.nativeSignin(item.ID) {
			item.ActionsAvailable = service.siteSignin.login.pureGo
			item.HasPage = true
		}
		if service.nativeMovieLike(item.ID) {
			item.ActionsAvailable = service.movieLike.organizer.pureGo
			item.HasPage = true
		}
		if service.nativeMediaSyncDelete(item.ID) {
			item.ActionsAvailable = service.mediaSyncDelete.organizer.pureGo
			item.HasPage = true
		}
		if service.nativeSpeedLimiter(item.ID) {
			item.ActionsAvailable = service.speedLimiter.organizer.pureGo
			item.HasPage = true
		}
		if service.nativeEventWebhook(item.ID) {
			item.ActionsAvailable = service.eventWebhook.organizer.pureGo
			item.HasPage = true
		}
		if service.nativeMediaRefresh(item.ID) {
			item.ActionsAvailable = service.mediaRefresh.organizer.pureGo
			item.HasPage = true
		}
		if service.nativeLibrary(item.ID) {
			item.ActionsAvailable = service.library.intake.organizer.pureGo
			item.HasPage = true
		}
		if service.nativeIndexer(item.ID) {
			item.ActionsAvailable = service.indexers.pureGo
		}
		if service.nativeSubtitle(item.ID) {
			item.ActionsAvailable = service.subtitles.organizer.pureGo
			item.HasPage = true
		}
		if service.nativeOpenSubtitles(item.ID) {
			item.ActionsAvailable = service.openSubtitles.subtitle.organizer.pureGo
			item.HasPage = true
			item.Description = "使用原网页或明确配置的官方 REST 提供方，下载中文字幕并保留已有文件"
		}
		item.AuthorURL = safePluginURL(item.AuthorURL)
		if item.Installed && item.Native && readState {
			if service.nativeFeedPlugin(item.ID) {
				p := service.feedPlugin(item.ID)
				raw, err := service.system.ReadMany(ctx, p.keys)
				if err == nil {
					item.Running, item.StateKnown = p.summary(ctx, raw)
				} else {
					item.StateKnown = false
				}
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeArchiveRunner(item.ID) {
				raw, err := service.system.ReadMany(ctx, archiveKeys)
				if err == nil {
					item.Running, item.StateKnown = service.archiveStatus(raw)
				} else {
					item.StateKnown = false
				}
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeTorrentTransfer(item.ID) {
				item.Running, item.StateKnown = service.torrentTransfer.scheduleSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeTorrentRemover(item.ID) {
				item.Running, item.StateKnown = service.torrentRemover.summary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeTorrentMark(item.ID) {
				item.Running, item.StateKnown = service.torrentMark.summary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeSyncTimer(item.ID) {
				item.Running, item.StateKnown = service.syncTimerRunning(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeAutoBackup(item.ID) {
				item.Running, item.StateKnown = service.autoBackup.summary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeCookieCloud(item.ID) {
				item.Running, item.StateKnown = service.cookieCloud.summary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeCloudflare(item.ID) {
				c, err := service.cloudflare.store.Control(ctx)
				item.Running = err == nil && service.cloudflare.validate(ctx, c) == nil
				item.StateKnown = err == nil
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeSignin(item.ID) {
				item.Running, item.StateKnown = service.signinSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeOpenSubtitles(item.ID) {
				item.Running, item.StateKnown = service.openSubtitleSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeMovieLike(item.ID) {
				item.Running, item.StateKnown = service.movieLikeSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeMediaSyncDelete(item.ID) {
				item.Running, item.StateKnown = service.mediaSyncDeleteSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeSpeedLimiter(item.ID) {
				item.Running, item.StateKnown = service.speedLimiterSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeEventWebhook(item.ID) {
				item.Running, item.StateKnown = service.eventWebhookSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeMediaRefresh(item.ID) {
				item.Running, item.StateKnown = service.mediaRefreshSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeLibrary(item.ID) {
				item.Running, item.StateKnown = service.librarySummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeSubtitle(item.ID) {
				item.Running, item.StateKnown = service.subtitleSummary(ctx)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeIndexer(item.ID) {
				item.Running, item.StateKnown = service.indexers.summary(ctx, item.ID)
				data.Items = append(data.Items, item)
				continue
			}
			if service.nativeHosts(item.ID) {
				service.hosts.mu.Lock()
				item.Running = service.hosts.active
				service.hosts.mu.Unlock()
				data.Items = append(data.Items, item)
				continue
			}
			_, active, err := service.nativeMetadataReadConfig(ctx, item.ID)
			if err != nil {
				return pluginsData{}, err
			}
			item.Running = active
		}
		data.Items = append(data.Items, item)
	}
	// Preserve out-of-catalog installed IDs for administrators. They are not
	// silently dropped or presented as known stopped/running implementations.
	if administrator {
		for id := range installedSet {
			if _, known := catalog[id]; known {
				continue
			}
			data.Items = append(data.Items, pluginSummary{ID: id, Name: id, Description: "已保留原安装记录和配置。此扩展需要对应的 Go 实现才能运行。", Installed: true})
		}
	}
	for i := range data.Items {
		item := &data.Items[i]
		item.CanViewPage = item.HasPage && (item.ActionsAvailable || item.NativeHistoryPage || item.NativeArchivePage || item.ID == helperID || item.ID == "IYUUAutoSeed" && service.iyuu != nil)
		if item.Installed {
			data.InstalledCount++
		}
		if item.StateKnown && item.Running {
			data.RunningCount++
		}
		if item.Installed && !item.StateKnown {
			data.UnknownStateCount++
		}
	}
	sort.Slice(data.Items, func(left, right int) bool {
		if data.Items[left].Installed != data.Items[right].Installed {
			return data.Items[left].Installed
		}
		a, b := strings.ToLower(data.Items[left].Name), strings.ToLower(data.Items[right].Name)
		if a == b {
			return data.Items[left].ID < data.Items[right].ID
		}
		return a < b
	})
	return data, nil
}

func (service pluginService) ownsPlugin(id string) bool {
	return service.pluginHandler(id) != nil
}
