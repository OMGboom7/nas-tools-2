package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaarchive"
	"github.com/0xforee/nas-tools/backend/internal/pluginpage"
)

var pluginPageRecordIDPattern = regexp.MustCompile(`^[0-9]{1,30}$`)

var pluginPageDeleteConfirmation = map[string]string{
	"DoubanSync": "confirm", "DoubanRank": "confirm", "MovieRandom": "confirm",
	"MediaLibraryArchive": "typeRecordId",
}

type pluginPageRowAction struct {
	Proof        string `json:"proof,omitempty"`
	Type         string `json:"type"`
	RecordID     string `json:"recordId"`
	Confirmation string `json:"confirmation"`
}

type pluginPageTable struct {
	Columns    []string               `json:"columns"`
	Rows       [][]string             `json:"rows"`
	RowActions []*pluginPageRowAction `json:"rowActions"`
}

type pluginPageData struct {
	NextCursor         string            `json:"nextCursor,omitempty"`
	Title              string            `json:"title"`
	Sections           []string          `json:"sections"`
	Tables             []pluginPageTable `json:"tables"`
	ReadOnly           bool              `json:"readOnly"`
	ActionsOmitted     bool              `json:"actionsOmitted"`
	CanDeleteRecords   bool              `json:"canDeleteRecords"`
	DeleteConfirmation string            `json:"deleteConfirmation"`
}

type pluginPageActionRequest struct {
	Proof        string `json:"proof,omitempty"`
	RecordID     string `json:"recordId"`
	Confirmation string `json:"confirmation"`
}

func (s pluginService) serveNativePage(w http.ResponseWriter, r *http.Request, id string) bool {
	r.SetPathValue("id", id)
	switch {
	case s.autoSub != nil && id == autoSubID:
		s.autoSub.page(w, r, s)
	case s.disk != nil && id == diskID:
		s.disk.page(w, r, s)
	case s.system != nil && id == helperID:
		s.helperPage(w, r)
	case s.iyuu != nil && id == "IYUUAutoSeed":
		s.iyuuPartnerPage(w, r)
	case s.nativeArchivePage(id):
		s.serveArchivePage(w, r)
	case s.nativeSubscriptionHistory(id):
		s.subscriptionHistoryPage(w, r, id)
	case s.nativeTorrentRemover(id):
		s.serveTorrentRemoverPlugin(w, r, "page")
	case s.nativeTorrentMark(id):
		s.serveTorrentMarkPlugin(w, r, "page")
	case s.nativeSyncTimer(id):
		s.serveSyncTimerPlugin(w, r, "page")
	case s.nativeAutoBackup(id):
		s.serveAutoBackupPlugin(w, r, "page")
	case s.nativeCookieCloud(id):
		s.serveCookieCloudPlugin(w, r, "page")
	case s.nativeSignin(id):
		s.serveSigninPlugin(w, r, "page")
	case s.nativeMovieLike(id):
		s.serveMovieLikePlugin(w, r, "page")
	case s.nativeMediaSyncDelete(id):
		s.serveMediaSyncDeletePlugin(w, r, "page")
	case s.nativeSpeedLimiter(id):
		s.serveSpeedLimiterPlugin(w, r, "page")
	case s.nativeEventWebhook(id):
		s.serveEventWebhookPlugin(w, r, "page")
	case s.nativeMediaRefresh(id):
		s.serveMediaRefreshPlugin(w, r, "page")
	case s.nativeLibrary(id):
		s.serveLibraryPlugin(w, r, "page")
	case s.nativeSubtitle(id):
		s.serveSubtitlePlugin(w, r, "page")
	case s.nativeOpenSubtitles(id):
		s.serveOpenSubtitlePlugin(w, r, "page")
	case s.nativeIndexer(id):
		s.serveIndexerPlugin(w, r, "page")
	default:
		return false
	}
	return true
}

func (service pluginService) servePage(response http.ResponseWriter, request *http.Request) {
	service.pluginPage(response, request, strings.TrimSpace(request.PathValue("id")))
}

func (s pluginService) legacyPage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, 400, 400, "插件扩展页请求无效")
		return
	}
	s.pluginPage(pluginFormPageWriter{w}, r, strings.TrimSpace(r.PostForm.Get("id")))
}

func (service pluginService) pluginPage(response http.ResponseWriter, request *http.Request, id string) {
	if service.serveNativePage(response, request, id) || service.unavailablePlugin(response, request, id) {
		return
	}
	token, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	if !pluginIDPattern.MatchString(id) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid plugin id")
		return
	}
	// The bundled catalog is authoritative for original pages. Only pages
	// actually provided by another executor need the transitional reader.
	if service.system != nil {
		catalog, err := bundledPluginCatalog()
		if err != nil {
			writeAPIError(response, 500, 500, "插件目录不可用")
			return
		}
		if entry, known := catalog[id]; known {
			admin, ok := service.catalogReader(response, request)
			if !ok {
				return
			}
			if !entry.HasPage || !admin && entry.AuthLevel > 1 {
				writeAPIError(response, 404, 404, "未找到插件扩展页")
				return
			}
			installed, err := service.nativeMetadataInstalled(request.Context())
			if err != nil {
				writeAPIError(response, 503, 503, "无法读取已安装插件")
				return
			}
			if !slices.Contains(installed, id) {
				writeAPIError(response, 404, 404, "尚未安装此插件")
				return
			}
		}
	}

	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	result, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/page", token, url.Values{"id": {id}})
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "legacy service is unavailable")
		return
	}
	code := int(number(result["code"]))
	if code == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if code == 404 {
		message := text(result["message"])
		if message == "" {
			message = "plugin page not found"
		}
		writeAPIError(response, http.StatusNotFound, 404, message)
		return
	}
	if code != 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "plugin page is unavailable")
		return
	}
	data := normalizePluginPage(legacyPayload(result))
	if data.Title == "" {
		writeAPIError(response, http.StatusBadGateway, 502, "plugin page is invalid")
		return
	}
	writeNativeRecordPage(response, data)
}

func normalizePluginPage(raw map[string]any) pluginPageData {
	data := pluginPageData{
		Title: cleanPluginPageText(raw["title"], 120), ReadOnly: true,
		ActionsOmitted: truthy(raw["actions_omitted"]), Sections: []string{}, Tables: []pluginPageTable{},
	}
	for _, rawSection := range sliceOrJSON(raw["sections"]) {
		if len(data.Sections) >= 80 {
			break
		}
		section := cleanPluginPageText(rawSection, 500)
		if section != "" {
			data.Sections = append(data.Sections, section)
		}
	}
	for _, rawTable := range sliceOrJSON(raw["tables"]) {
		if len(data.Tables) >= 10 {
			break
		}
		tableObject := objectValue(rawTable)
		table := pluginPageTable{Columns: []string{}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
		for _, rawColumn := range sliceOrJSON(tableObject["columns"]) {
			if len(table.Columns) >= 30 {
				break
			}
			column := cleanPluginPageText(rawColumn, 120)
			if column != "" {
				table.Columns = append(table.Columns, column)
			}
		}
		if len(table.Columns) == 0 {
			continue
		}
		for _, action := range sliceOrJSON(tableObject["row_actions"]) {
			if action != nil {
				data.ActionsOmitted = true
				break
			}
		}
		for _, rawRow := range sliceOrJSON(tableObject["rows"]) {
			if len(table.Rows) >= 200 {
				break
			}
			values := sliceOrJSON(rawRow)
			row := make([]string, len(table.Columns))
			nonempty := false
			for index := range table.Columns {
				if index < len(values) {
					row[index] = cleanPluginPageText(values[index], 500)
					nonempty = nonempty || row[index] != ""
				}
			}
			if nonempty {
				table.Rows = append(table.Rows, row)
				table.RowActions = append(table.RowActions, nil)
			}
		}
		data.Tables = append(data.Tables, table)
	}
	return data
}

func (service pluginService) deletePageRecord(response http.ResponseWriter, request *http.Request) {
	if service.helper != nil && request.PathValue("id") == helperID {
		writeAPIError(response, 400, 400, "下载器助手扩展页只支持查看")
		return
	}
	if _, ok := requireServiceToken(response, request); !ok {
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	confirmationMode := pluginPageDeleteConfirmation[id]
	if confirmationMode == "" {
		writeAPIError(response, http.StatusNotFound, 404, "plugin page action is not available")
		return
	}
	var input pluginPageActionRequest
	if !decodeServiceRequest(response, request, &input, "invalid plugin page action request") {
		return
	}
	input.RecordID = strings.TrimSpace(input.RecordID)
	if !validPluginPageRecordID(id, input.RecordID) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid plugin page record id")
		return
	}
	if confirmationMode == "typeRecordId" && input.Confirmation != input.RecordID {
		writeAPIError(response, http.StatusBadRequest, 400, "plugin page confirmation does not match")
		return
	}
	service.removePageRecord(response, request, id, input)
}

// The original page action API only supports three history plugins and the
// media archive. All four operate on the data host that verified the account.
func (s pluginService) removePageRecord(w http.ResponseWriter, r *http.Request, id string, input pluginPageActionRequest) {
	switch {
	case s.nativeArchivePage(id):
		s.removeArchiveRecord(w, r, input.RecordID, input.Confirmation, input.Proof)
	case s.nativeSubscriptionHistory(id):
		s.removeSubscriptionHistory(w, r, id, input.RecordID)
	default:
		writeAPIError(w, 503, 503, "请连接可访问插件数据和归档目录的 Go 服务")
	}
}

func (s pluginService) legacyPageAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, 405, 405, "此接口仅接受 POST 请求")
		return
	}
	if _, ok := requireServiceToken(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, 400, 400, "插件扩展页请求无效")
		return
	}
	id := strings.TrimSpace(r.PostForm.Get("id"))
	if pluginPageDeleteConfirmation[id] == "" {
		if s.nativeFeedPlugin(id) && (id == autoSubID || id == diskID || id == helperID || id == "IYUUAutoSeed") {
			writeAPIError(w, 400, 400, "此扩展页只支持查看")
			return
		}
		if s.unavailablePlugin(w, r, id) {
			return
		}
		writeAPIError(w, 400, 400, "此扩展页没有可执行的记录操作")
		return
	}
	if r.PostForm.Get("action") != "delete" {
		writeAPIError(w, 400, 400, "不支持的插件扩展页操作")
		return
	}
	s.removePageRecord(w, r, id, pluginPageActionRequest{
		RecordID: r.PostForm.Get("record_id"), Confirmation: r.PostForm.Get("confirmation"), Proof: r.PostForm.Get("proof"),
	})
}

func validPluginPageRecordID(pluginID, recordID string) bool {
	if pluginID == "MediaLibraryArchive" {
		return mediaarchive.ValidName(recordID)
	}
	return pluginPageDeleteConfirmation[pluginID] == "confirm" && pluginPageRecordIDPattern.MatchString(recordID)
}

func cleanPluginPageText(value any, limit int) string {
	return pluginpage.CleanText(text(value), limit)
}

// The legacy form endpoint uses snake-case action keys; the React client
// consumes the typed page directly. Both expose the same rows and cursor.
func writeNativeRecordPage(w http.ResponseWriter, data pluginPageData) {
	if output, ok := w.(interface{ writePage(pluginPageData) }); ok {
		output.writePage(data)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}

type pluginFormPageWriter struct{ http.ResponseWriter }

func (w pluginFormPageWriter) writePage(data pluginPageData) {
	tables := make([]any, 0, len(data.Tables))
	for _, table := range data.Tables {
		actions := make([]any, len(table.RowActions))
		for i, a := range table.RowActions {
			if a != nil {
				action := map[string]string{"type": a.Type, "record_id": a.RecordID}
				if a.Proof != "" {
					action["proof"] = a.Proof
				}
				actions[i] = action
			}
		}
		tables = append(tables, map[string]any{"columns": table.Columns, "rows": table.Rows, "row_actions": actions})
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"title": data.Title, "sections": data.Sections, "tables": tables, "actions_omitted": data.ActionsOmitted, "nextCursor": data.NextCursor}})
}
