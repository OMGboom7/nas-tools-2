package httpserver

import (
	"net/http"
	"strings"
)

type pluginActionHandler func(http.ResponseWriter, *http.Request, string)

// Select the same executor for the catalog, React, the original form API and
// the original JSON commands. Each executor owns its permissions and lifecycle.
func (s pluginService) pluginHandler(id string) pluginActionHandler {
	switch {
	case s.nativeFeedPlugin(id):
		return func(w http.ResponseWriter, r *http.Request, action string) {
			s.serveFeedPlugin(w, r, id, action)
		}
	case s.nativeTorrentTransfer(id):
		return s.serveTorrentTransferPlugin
	case s.nativeTorrentRemover(id):
		return s.serveTorrentRemoverPlugin
	case s.nativeTorrentMark(id):
		return s.serveTorrentMarkPlugin
	case s.nativeSyncTimer(id):
		return s.serveSyncTimerPlugin
	case s.nativeArchiveRunner(id):
		return s.serveArchivePlugin
	case s.nativeAutoBackup(id):
		return s.serveAutoBackupPlugin
	case s.nativeCookieCloud(id):
		return s.serveCookieCloudPlugin
	case s.nativeCloudflare(id):
		return s.serveCloudflarePlugin
	case s.nativeSignin(id):
		return s.serveSigninPlugin
	case s.nativeMovieLike(id):
		return s.serveMovieLikePlugin
	case s.nativeMediaSyncDelete(id):
		return s.serveMediaSyncDeletePlugin
	case s.nativeSpeedLimiter(id):
		return s.serveSpeedLimiterPlugin
	case s.nativeEventWebhook(id):
		return s.serveEventWebhookPlugin
	case s.nativeMediaRefresh(id):
		return s.serveMediaRefreshPlugin
	case s.nativeLibrary(id):
		return s.serveLibraryPlugin
	case s.nativeSubtitle(id):
		return s.serveSubtitlePlugin
	case s.nativeOpenSubtitles(id):
		return s.serveOpenSubtitlePlugin
	case s.nativeIndexer(id):
		return s.serveIndexerPlugin
	case s.nativeHosts(id):
		return s.serveHostsPlugin
	case s.system != nil && nativeMetadataPlugin(id):
		return s.metadataAction
	default:
		return nil
	}
}

func (s pluginService) dispatchPlugin(w http.ResponseWriter, r *http.Request, id, action string) bool {
	handle := s.pluginHandler(id)
	if handle == nil {
		return false
	}
	r.SetPathValue("id", id)
	if action == "config" && !s.nativeHosts(id) && !nativeMetadataPlugin(id) {
		action = "legacy-save"
	}
	handle(w, r, action)
	return true
}

func (s pluginService) metadataAction(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("id")
	switch action {
	case "get", "save":
		s.serveNativeMetadataConfig(w, r)
	case "install", "uninstall":
		s.mutateNativeMetadataPlugin(w, r, action == "install")
	case "config":
		s.saveLegacyMetadataConfig(w, r, id)
	case "status":
		if _, ok := s.catalogReader(w, r); ok {
			s.serveNativeMetadataStatus(w, r, id)
		}
	}
}

// In a Go-only installation the local catalog is authoritative even for IDs
// left in an upgraded database by a third-party plugin.
func (s pluginService) unavailablePlugin(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.system == nil || s.legacyURL != "" {
		return false
	}
	admin, ok := s.catalogReader(w, r)
	if !ok {
		return true
	}
	if !pluginIDPattern.MatchString(id) {
		writeAPIError(w, 400, 400, "插件编号无效")
		return true
	}
	catalog, err := bundledPluginCatalog()
	if err != nil {
		writeAPIError(w, 500, 500, "插件目录不可用")
		return true
	}
	if entry, found := catalog[id]; found {
		switch {
		case !admin && entry.AuthLevel > 1:
			writeAPIError(w, 404, 404, "未找到插件")
		case s.ownsPlugin(id):
			writeAPIError(w, 404, 404, "此插件没有该页面或操作")
		case id == "CustomHosts":
			writeAPIError(w, 409, 409, "启用自定义 Hosts 需要配置 NASTOOL_GO_HOSTS_PATH")
		default:
			writeAPIError(w, 409, 409, "当前部署缺少此插件所需的运行条件")
		}
		return true
	}
	if admin {
		installed, err := s.nativeMetadataInstalled(r.Context())
		if err != nil {
			writeAPIError(w, 503, 503, "无法读取已安装插件")
			return true
		}
		for _, current := range installed {
			if current == id {
				external, err := s.externalPluginCatalog(r.Context(), id)
				if err != nil {
					writeAPIError(w, 503, 503, "插件目录不可用")
					return true
				}
				if entry, ok := external[id]; ok && !entry.Visible(pluginReaderLevel(admin)) {
					writeAPIError(w, 404, 404, "未找到插件")
					return true
				}
				writeAPIError(w, 409, 409, "已保留此扩展的安装记录，需要相应的 Go 实现")
				return true
			}
		}
	}
	writeAPIError(w, 404, 404, "未找到插件")
	return true
}

func (s pluginService) unavailablePluginRoute(w http.ResponseWriter, r *http.Request) bool {
	return s.unavailablePlugin(w, r, strings.TrimSpace(r.PathValue("id")))
}
