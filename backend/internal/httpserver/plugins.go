package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type pluginService struct {
	legacyURL string
	client    *http.Client
	system    *systemconfig.Store
	auth      *nativeAuthentication
	hosts     *nativeHostsPlugin
}

type pluginSummary struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	Version          string `json:"version"`
	Author           string `json:"author"`
	AuthorURL        string `json:"authorUrl"`
	Installed        bool   `json:"installed"`
	Running          bool   `json:"running"`
	Configurable     bool   `json:"configurable"`
	HasPage          bool   `json:"hasPage"`
	StateKnown       bool   `json:"stateKnown"`
	Native           bool   `json:"native"`
	ActionsAvailable bool   `json:"actionsAvailable"`
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
	data, result, err := service.list(ctx, token)
	if !handlePluginResult(response, result, err, "plugin list is unavailable") {
		return
	}
	var current *pluginSummary
	for index := range data.Items {
		if data.Items[index].ID == id {
			current = &data.Items[index]
			break
		}
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
	action := "install"
	if !install {
		action = "uninstall"
	}
	result, err = postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/"+action, token, url.Values{"id": {id}})
	if !handlePluginResult(response, result, err, "plugin operation failed") {
		return
	}
	message := text(result["msg"])
	if message == "" {
		message = "plugin operation completed"
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": message})
}

func (service pluginService) list(ctx context.Context, token string) (pluginsData, map[string]any, error) {
	appsResult, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/apps", token, nil)
	if err != nil || int(number(appsResult["code"])) != 0 {
		return pluginsData{}, appsResult, err
	}
	runningResult, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/list", token, nil)
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
