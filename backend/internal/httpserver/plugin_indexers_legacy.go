package httpserver

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

func indexerLegacyFields(detail pluginConfigDetail) []any {
	rows := []any{}
	for _, f := range detail.Fields {
		column := map[string]any{"title": f.Title, "tooltip": f.Tooltip, "type": f.Type, "configured": f.Configured}
		if f.Type == "switch" {
			column["id"] = f.Key
		} else if f.Type == "multiselect" {
			choices := map[string]any{}
			for _, choice := range f.Options {
				choices[choice.Value] = map[string]string{"name": choice.Label}
			}
			column["type"], column["id"], column["content"] = "form-selectgroup", f.Key, choices
		} else {
			field := map[string]any{"id": f.Key, "placeholder": f.Placeholder, "configured": f.Configured}
			if f.Type == "select" {
				choices := map[string]string{}
				for _, choice := range f.Options {
					choices[choice.Value] = choice.Label
				}
				field["options"] = choices
			}
			column["content"] = []any{field}
		}
		rows = append(rows, []any{column})
	}
	return []any{map[string]any{"type": "div", "content": rows}}
}

func (s pluginService) nativePluginPageRoute(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if err != nil {
			writeAPIError(w, 400, 400, "invalid plugin page request")
			return
		}
		probe := r.Clone(r.Context())
		probe.Body = io.NopCloser(bytes.NewReader(body))
		if probe.ParseForm() != nil {
			writeAPIError(w, 400, 400, "invalid plugin page request")
			return
		}
		id := strings.TrimSpace(probe.PostForm.Get("id"))
		if s.nativeLibrary(id) {
			probe.SetPathValue("id", id)
			s.serveLibraryPlugin(w, probe, "page")
			return
		}
		if s.nativeSubtitle(id) {
			probe.SetPathValue("id", id)
			s.serveSubtitlePlugin(w, probe, "page")
			return
		}
		if s.nativeIndexer(id) {
			probe.SetPathValue("id", id)
			s.serveIndexerPlugin(w, probe, "page")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fallback.ServeHTTP(w, r)
	})
}
