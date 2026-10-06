package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

type pluginCatalogEntry struct {
	pluginSummary
	AuthLevel int    `json:"authLevel"`
	Icon      string `json:"icon"`
	Color     string `json:"color"`
}

func (service pluginService) serveNativePluginCatalog(response http.ResponseWriter, request *http.Request) {
	if service.auth == nil {
		writeAPIError(response, 503, 503, "native authentication is unavailable")
		return
	}
	claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "authorization token is invalid or expired")
		return
	}
	administrator := service.auth.service.IsAdministrator(claims.Username)
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
	var catalog []pluginCatalogEntry
	if err := json.Unmarshal([]byte(nativePluginCatalogJSON), &catalog); err != nil {
		return pluginsData{}, err
	}
	installedSet := map[string]bool{}
	for _, id := range installed {
		installedSet[id] = true
	}
	known := map[string]bool{}
	data := pluginsData{Items: []pluginSummary{}}
	for _, entry := range catalog {
		item := entry.pluginSummary
		known[item.ID] = true
		if entry.AuthLevel > 1 && !administrator {
			continue
		}
		item.Installed = installedSet[item.ID]
		item.Native = nativeMetadataPlugin(item.ID) || service.nativeHosts(item.ID)
		item.StateKnown = !item.Installed || item.Native && readState
		item.ActionsAvailable = item.Native || service.legacyURL != ""
		item.AuthorURL = safePluginURL(item.AuthorURL)
		if item.Installed && item.Native && readState {
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
		for _, id := range installed {
			if known[id] {
				continue
			}
			known[id] = true
			data.Items = append(data.Items, pluginSummary{ID: id, Name: id, Description: "已安装，但不在当前内置插件目录中。", Installed: true})
		}
	}
	for _, item := range data.Items {
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
		return strings.ToLower(data.Items[left].Name) < strings.ToLower(data.Items[right].Name)
	})
	return data, nil
}
