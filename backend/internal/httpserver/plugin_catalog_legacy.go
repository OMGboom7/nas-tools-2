package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func (service pluginService) serveNativeLegacyPluginApps(response http.ResponseWriter, request *http.Request) {
	administrator, ok := service.catalogReader(response, request)
	if !ok {
		return
	}
	data, err := service.nativePluginCatalogData(request.Context(), administrator, false)
	if err != nil {
		writeAPIError(response, 502, 502, "plugin catalog is unavailable")
		return
	}
	metadata, err := bundledPluginCatalog()
	if err != nil {
		writeAPIError(response, 500, 500, "bundled plugin catalog is invalid")
		return
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

func legacyCommandRoutes(reads map[string]http.HandlerFunc, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		var envelope struct {
			Cmd string `json:"cmd"`
		}
		if err != nil || json.Unmarshal(raw, &envelope) != nil {
			writeAPIError(w, 400, 400, "原操作格式无效")
			return
		}
		if strings.EqualFold(strings.TrimSpace(envelope.Cmd), "restory_backup") {
			writeAPIError(w, 410, 410, "旧文件名恢复入口已迁移，请在服务页上传 ZIP 并按提示离线恢复")
			return
		}
		if read := reads[envelope.Cmd]; read != nil {
			// Both catalog reads are argument-free; never pass the command
			// envelope into the installed-list form.
			r.Body = http.NoBody
			r.ContentLength = 0
			read(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next(w, r)
	}
}
