package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type pluginService struct {
	mteamFeed         *mteamFeedService
	doubanSync        *doubanSyncAPI
	archiveMedia      *mediaSyncAPI
	archiveConfigPath string
	archiveContainer  bool
	legacyURL         string
	client            *http.Client
	system            *systemconfig.Store
	auth              *nativeAuthentication
	hosts             *nativeHostsPlugin
	indexers          *nativeIndexerPlugins
	subtitles         *subtitleAPI
	openSubtitles     *openSubtitleAPI
	library           *libraryScraper
	mediaRefresh      *mediaRefreshAPI
	eventWebhook      *eventWebhookAPI
	speedLimiter      *speedLimiterAPI
	mediaSyncDelete   *mediaSyncDeleteAPI
	movieLike         *movieLikeAPI
	siteSignin        *siteSigninAPI
	cloudflare        *cloudflareAPI
	cookieCloud       *cookieCloudAPI
	torrentMark       *torrentMarkAPI
	torrentTransfer   *torrentTransferAPI
	torrentRemover    *torrentRemovalAPI
	autoBackup        *autoBackupAPI
	syncTimer         *organizationAPI
}

type pluginSummary struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Version           string `json:"version"`
	Author            string `json:"author"`
	AuthorURL         string `json:"authorUrl"`
	Installed         bool   `json:"installed"`
	Running           bool   `json:"running"`
	Configurable      bool   `json:"configurable"`
	NativeArchivePage bool   `json:"nativeArchivePage,omitempty"`
	NativeHistoryPage bool   `json:"nativeHistoryPage,omitempty"`
	HasPage           bool   `json:"hasPage"`
	StateKnown        bool   `json:"stateKnown"`
	Native            bool   `json:"native"`
	ActionsAvailable  bool   `json:"actionsAvailable"`
}

type pluginsData struct {
	Items             []pluginSummary `json:"items"`
	InstalledCount    int             `json:"installedCount"`
	RunningCount      int             `json:"runningCount"`
	UnknownStateCount int             `json:"unknownStateCount"`
}

var pluginIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,79}$`)

func (service pluginService) serveList(response http.ResponseWriter, request *http.Request) {
	if service.system != nil {
		service.serveNativePluginCatalog(response, request)
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	data, result, err := service.list(ctx, token)
	if !handlePluginResult(response, result, err, "plugin list is unavailable") {
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service pluginService) install(response http.ResponseWriter, request *http.Request) {
	service.mutate(response, request, true)
}

func (service pluginService) uninstall(response http.ResponseWriter, request *http.Request) {
	service.mutate(response, request, false)
}

func (service pluginService) mutate(response http.ResponseWriter, request *http.Request, install bool) {
	if service.nativeTorrentTransfer(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveTorrentTransferPlugin(response, request, action)
		return
	}
	if service.nativeTorrentRemover(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveTorrentRemoverPlugin(response, request, action)
		return
	}
	if service.nativeTorrentMark(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveTorrentMarkPlugin(response, request, action)
		return
	}
	if service.nativeSyncTimer(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveSyncTimerPlugin(response, request, action)
		return
	}
	if service.nativeFeedPlugin(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveFeedPlugin(response, request, request.PathValue("id"), action)
		return
	}
	if service.nativeArchiveRunner(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveArchivePlugin(response, request, action)
		return
	}
	if service.nativeAutoBackup(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveAutoBackupPlugin(response, request, action)
		return
	}
	if service.nativeCookieCloud(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveCookieCloudPlugin(response, request, action)
		return
	}
	if service.nativeCloudflare(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveCloudflarePlugin(response, request, action)
		return
	}
	if service.nativeSignin(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveSigninPlugin(response, request, action)
		return
	}
	if service.nativeMovieLike(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveMovieLikePlugin(response, request, action)
		return
	}
	if service.nativeMediaSyncDelete(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveMediaSyncDeletePlugin(response, request, action)
		return
	}
	if service.nativeSpeedLimiter(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveSpeedLimiterPlugin(response, request, action)
		return
	}
	if service.nativeEventWebhook(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveEventWebhookPlugin(response, request, action)
		return
	}
	if service.nativeMediaRefresh(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveMediaRefreshPlugin(response, request, action)
		return
	}
	if service.nativeLibrary(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveLibraryPlugin(response, request, action)
		return
	}
	if service.nativeSubtitle(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveSubtitlePlugin(response, request, action)
		return
	}
	if service.nativeOpenSubtitles(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveOpenSubtitlePlugin(response, request, action)
		return
	}
	if service.nativeIndexer(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveIndexerPlugin(response, request, action)
		return
	}
	if service.nativeHosts(request.PathValue("id")) {
		action := "uninstall"
		if install {
			action = "install"
		}
		service.serveHostsPlugin(response, request, action)
		return
	}
	if service.system != nil && nativeMetadataPlugin(strings.TrimSpace(request.PathValue("id"))) {
		service.mutateNativeMetadataPlugin(response, request, install)
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	if !pluginIDPattern.MatchString(id) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid plugin id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	current, ok := service.pluginForMutation(ctx, response, request, id, token)
	if !ok {
		return
	}
	if current == nil {
		writeAPIError(response, http.StatusNotFound, 404, "plugin not found")
		return
	}
	if install && current.Installed {
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "plugin is already installed"})
		return
	}
	if !install && !current.Installed {
		writeAPIError(response, http.StatusConflict, 409, "plugin is not installed")
		return
	}
	if service.legacyURL == "" {
		writeAPIError(response, 501, 501, "此插件的安装和卸载仍需要旧后端")
		return
	}
	action := "install"
	if !install {
		action = "uninstall"
	}
	result, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/"+action, token, url.Values{"id": {id}})
	if !handlePluginResult(response, result, err, "plugin operation failed") {
		return
	}
	message := text(result["msg"])
	if message == "" {
		message = "plugin operation completed"
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": message})
}

// Bundled plugins use the same installation record as the market. External
// extensions can still be discovered by their owner in a proxy deployment.
func (service pluginService) pluginForMutation(ctx context.Context, response http.ResponseWriter, request *http.Request, id, token string) (*pluginSummary, bool) {
	if service.system != nil {
		administrator, ok := service.catalogReader(response, request)
		if !ok {
			return nil, false
		}
		catalog, err := bundledPluginCatalog()
		if err != nil {
			writeAPIError(response, 500, 500, "bundled plugin catalog is invalid")
			return nil, false
		}
		if entry, found := catalog[id]; found {
			if entry.AuthLevel > 1 && !administrator {
				return nil, true
			}
			installed, err := service.nativeMetadataInstalled(ctx)
			if err != nil {
				writeAPIError(response, 502, 502, "installed plugin state is unavailable")
				return nil, false
			}
			return &pluginSummary{ID: id, Installed: slices.Contains(installed, id)}, true
		}
		if service.legacyURL == "" {
			return nil, true
		}
	}
	data, result, err := service.list(ctx, token)
	if !handlePluginResult(response, result, err, "plugin list is unavailable") {
		return nil, false
	}
	for _, item := range data.Items {
		if item.ID == id {
			return &item, true
		}
	}
	return nil, true
}

func (service pluginService) list(ctx context.Context, token string) (pluginsData, map[string]any, error) {
	appsResult, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/apps", token, nil)
	if err != nil || int(number(appsResult["code"])) != 0 {
		return pluginsData{}, appsResult, err
	}
	runningResult, err := service.legacyInstalledPlugins(ctx, token, nil)
	if err != nil || int(number(runningResult["code"])) != 0 {
		return pluginsData{}, runningResult, err
	}

	apps := objectValue(appsResult["result"])
	running := objectValue(runningResult["result"])
	data := pluginsData{Items: make([]pluginSummary, 0, len(apps))}
	for key, raw := range apps {
		app := objectValue(raw)
		id := strings.TrimSpace(text(app["id"]))
		if id == "" {
			id = strings.TrimSpace(key)
		}
		if !pluginIDPattern.MatchString(id) {
			continue
		}
		name := cleanPluginText(app["name"], 120)
		if name == "" {
			name = id
		}
		item := pluginSummary{
			ID:          id,
			Name:        name,
			Description: cleanPluginText(app["desc"], 500),
			Version:     cleanPluginText(app["version"], 40),
			Author:      cleanPluginText(app["author"], 100),
			AuthorURL:   safePluginURL(text(app["author_url"])),
			Installed:   truthy(app["installed"]),
			StateKnown:  true, ActionsAvailable: true,
		}
		if rawRunning, found := running[id]; found {
			configuration := objectValue(rawRunning)
			item.Installed = true
			item.Running = truthy(configuration["state"])
			item.Configurable = len(sliceOrJSON(configuration["fields"])) > 0 || len(objectValue(configuration["fields"])) > 0
			item.HasPage = cleanPluginText(configuration["page"], 120) != ""
		}
		if item.Installed {
			data.InstalledCount++
		}
		if item.Running {
			data.RunningCount++
		}
		data.Items = append(data.Items, item)
	}
	sort.Slice(data.Items, func(left, right int) bool {
		if data.Items[left].Installed != data.Items[right].Installed {
			return data.Items[left].Installed
		}
		return strings.ToLower(data.Items[left].Name) < strings.ToLower(data.Items[right].Name)
	})
	return data, runningResult, nil
}

func handlePluginResult(response http.ResponseWriter, result map[string]any, err error, fallback string) bool {
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "legacy service is unavailable")
		return false
	}
	code := int(number(result["code"]))
	if code == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return false
	}
	if code != 0 {
		message := text(result["msg"])
		if message == "" {
			message = text(result["message"])
		}
		if message == "" {
			message = fallback
		}
		writeAPIError(response, http.StatusBadGateway, 502, message)
		return false
	}
	return true
}

func cleanPluginText(value any, limit int) string {
	cleaned := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(text(value), "\x00", ""), "\r", " "))
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	runes := []rune(cleaned)
	if len(runes) > limit {
		cleaned = string(runes[:limit])
	}
	return cleaned
}

func safePluginURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return parsed.String()
}
