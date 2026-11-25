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

func (s pluginService) nativePluginPageRoute(fallback http.Handler, action bool) http.Handler {
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
		if !action && s.serveNativePage(w, probe, id, true) {
			return
		}
		if action {
			switch {
			case s.nativeArchivePage(id), s.nativeSubscriptionHistory(id):
				if probe.PostForm.Get("action") != "delete" {
					writeAPIError(w, 400, 400, "不支持的插件扩展页操作")
				} else if s.nativeArchivePage(id) {
					s.removeArchiveRecord(w, probe, probe.PostForm.Get("record_id"), probe.PostForm.Get("confirmation"), probe.PostForm.Get("proof"))
				} else {
					s.removeSubscriptionHistory(w, probe, id, probe.PostForm.Get("record_id"))
				}
				return
			case s.nativeFeedPlugin(id) && (id == autoSubID || id == diskID || id == helperID || id == "IYUUAutoSeed"):
				writeAPIError(w, 400, 400, "此扩展页只支持查看")
				return
			}
		}
		if s.unavailablePlugin(w, probe, id) {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		fallback.ServeHTTP(w, r)
	})
}
