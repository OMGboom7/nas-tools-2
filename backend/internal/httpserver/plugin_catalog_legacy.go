package httpserver

import (
	"encoding/json"
	"net/http"
)

func (service pluginService) serveNativeLegacyPluginApps(response http.ResponseWriter, request *http.Request) {
	if service.auth == nil {
		writeAPIError(response, 503, 503, "native authentication is unavailable")
		return
	}
	claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "authorization token is invalid or expired")
		return
	}
	data, err := service.nativePluginCatalogData(request.Context(), service.auth.service.IsAdministrator(claims.Username), false)
	if err != nil {
		writeAPIError(response, 502, 502, "plugin catalog is unavailable")
		return
	}
	var entries []pluginCatalogEntry
	if err := json.Unmarshal([]byte(nativePluginCatalogJSON), &entries); err != nil {
		writeAPIError(response, 500, 500, "bundled plugin catalog is invalid")
		return
	}
	metadata := map[string]pluginCatalogEntry{}
	for _, entry := range entries {
		metadata[entry.ID] = entry
	}
	result := map[string]any{}
	for _, item := range data.Items {
		entry := metadata[item.ID]
		result[item.ID] = map[string]any{"id": item.ID, "name": item.Name, "desc": item.Description, "version": item.Version, "author": item.Author, "author_url": item.AuthorURL, "installed": item.Installed, "icon": entry.Icon, "color": entry.Color, "native": item.Native}
	}
	// The old helper's statistics backend was disabled and returned {}.
	// Preserve that contract without introducing a fictitious external count.
	writeJSON(response, 200, map[string]any{"code": 0, "result": result, "statistic": map[string]any{}})
}
