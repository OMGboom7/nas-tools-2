package httpserver

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
	"slices"
	"strings"
)

// The old client inserts content into a modal. Escape every plugin-provided
// value as text and send interactive actions to the authenticated React view.
var pluginCommandHTML = template.Must(template.New("plugin").Parse(
	`<div class="modal-body">{{range .Page.Sections}}<p>{{.}}</p>{{end}}{{range .Page.Tables}}<table class="table"><thead><tr>{{range .Columns}}<th scope="col">{{.}}</th>{{end}}</tr></thead><tbody>{{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}</tbody></table>{{end}}<a href="/plugins/{{.ID}}">打开插件扩展页</a></div>`,
))

type pluginCommandPageWriter struct {
	http.ResponseWriter
	id string
}

func (w pluginCommandPageWriter) writePage(page pluginPageData) {
	var content bytes.Buffer
	if err := pluginCommandHTML.Execute(&content, struct {
		ID   string
		Page pluginPageData
	}{w.id, page}); err != nil {
		writeAPIError(w, 500, 500, "无法生成插件扩展页")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "title": page.Title, "content": content.String(), "func": nil, "data": page})
}

func (s pluginService) commandPage(w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	var input struct{ ID, After string }
	if json.Unmarshal(raw, &input) != nil || !pluginIDPattern.MatchString(input.ID) {
		writeAPIError(w, 400, 400, "插件编号无效")
		return
	}
	admin, ok := s.catalogReader(w, r)
	if !ok {
		return
	}
	catalog, err := bundledPluginCatalog()
	if err != nil || s.system == nil {
		writeAPIError(w, 503, 503, "插件目录不可用")
		return
	}
	entry, known := catalog[input.ID]
	if !known || entry.AuthLevel > 1 && !admin {
		if !known && s.unavailablePlugin(w, r, input.ID) {
			return
		}
		writeAPIError(w, 404, 404, "未找到插件")
		return
	}
	installed, err := s.nativeMetadataInstalled(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "无法读取已安装插件")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	if slices.Contains(installed, input.ID) {
		q := r.URL.Query()
		if input.After != "" {
			q.Set("after", input.After)
		}
		req := r.Clone(r.Context())
		req.URL.RawQuery = q.Encode()
		if s.serveNativePage(pluginCommandPageWriter{w, input.ID}, req, input.ID, false) {
			return
		}
		if entry.HasPage && !s.ownsPlugin(input.ID) {
			writeAPIError(w, 409, 409, "当前部署缺少此插件扩展页的运行条件")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"code": 0, "title": nil, "content": nil, "func": nil})
}

// Original clients use both JSON integers and strings for history keys.
// Preserve the exact digits when a key exceeds JavaScript's integer precision.
func commandHistoryKey(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return string(raw)
	}
	var key string
	if json.Unmarshal(raw, &key) != nil {
		return ""
	}
	return key
}

func (s pluginService) commandPageMethod(w http.ResponseWriter, r *http.Request, raw json.RawMessage) {
	var input struct {
		ID           string `json:"plugin_id"`
		Method       string
		DoubanID     json.RawMessage `json:"douban_id"`
		TMDBID       json.RawMessage `json:"tmdb_id"`
		FileName     string          `json:"file_name"`
		Confirmation *string
		Proof        *string
	}
	if json.Unmarshal(raw, &input) != nil || !pluginIDPattern.MatchString(input.ID) {
		writeAPIError(w, 400, 400, "插件操作格式无效")
		return
	}
	if _, ok := s.catalogReader(w, r); !ok {
		return
	}
	if s.iyuu != nil && input.ID == "IYUUAutoSeed" && s.iyuu.legacyBind(w, r, raw) {
		return
	}
	if s.nativeArchivePage(input.ID) && input.Method == "remove_archive_file_for_api" {
		// The original named method identifies its target by file_name alone.
		// Clients sending a reviewed selection must supply the complete pair.
		confirmation, proof := input.FileName, ""
		if input.Confirmation != nil || input.Proof != nil {
			if input.Proof == nil || strings.TrimSpace(*input.Proof) == "" || input.Confirmation == nil {
				writeAPIError(w, 409, 409, "请重新打开扩展页，核对并确认要删除的归档")
				return
			}
			confirmation, proof = *input.Confirmation, *input.Proof
		}
		s.removeArchiveRecord(w, r, input.FileName, confirmation, proof)
		return
	}
	if s.nativeSubscriptionHistory(input.ID) {
		var key json.RawMessage
		switch {
		case input.ID == "DoubanSync" && input.Method == "delete_sync_history":
			key = input.DoubanID
		case input.ID == "DoubanRank" && input.Method == "delete_rank_history",
			input.ID == "MovieRandom" && input.Method == "delete_random_history":
			key = input.TMDBID
		default:
			writeAPIError(w, 400, 400, "不支持的订阅历史操作")
			return
		}
		s.removeSubscriptionHistory(w, r, input.ID, commandHistoryKey(key))
		return
	}
	if !s.unavailablePlugin(w, r, input.ID) {
		writeAPIError(w, 404, 404, "未找到扩展页操作")
	}
}
