package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var pluginPageURLPattern = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)
var pluginPageRecordIDPattern = regexp.MustCompile(`^[0-9]{1,30}$`)
var pluginPageArchiveIDPattern = regexp.MustCompile(`^归档_[0-9]{14}\.md$`)

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

func (s pluginService) serveNativePage(w http.ResponseWriter, r *http.Request, id string, legacy bool) bool {
	r.SetPathValue("id", id)
	switch {
	case s.autoSub != nil && id == autoSubID:
		s.autoSub.page(w, r, s, legacy)
	case s.disk != nil && id == diskID:
		s.disk.page(w, r, s, legacy)
	case s.helper != nil && id == helperID:
		s.helperPage(w, r, legacy)
	case s.iyuu != nil && id == "IYUUAutoSeed":
		s.iyuu.partnerPage(w, r, legacy)
	case s.nativeArchivePage(id):
		s.serveArchivePage(w, r, legacy)
	case s.nativeSubscriptionHistory(id):
		s.subscriptionHistoryPage(w, r, id, legacy)
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
	if service.serveNativePage(response, request, strings.TrimSpace(request.PathValue("id")), false) {
		return
	}
	if service.unavailablePluginRoute(response, request) {
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
	data := normalizePluginPage(id, legacyPayload(result))
	if data.Title == "" {
		writeAPIError(response, http.StatusBadGateway, 502, "plugin page is invalid")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func normalizePluginPage(pluginID string, raw map[string]any) pluginPageData {
	data := pluginPageData{
		Title: cleanPluginPageText(raw["title"], 120), ReadOnly: true, CanDeleteRecords: pluginPageDeleteConfirmation[pluginID] != "",
		DeleteConfirmation: pluginPageDeleteConfirmation[pluginID], ActionsOmitted: truthy(raw["actions_omitted"]), Sections: []string{}, Tables: []pluginPageTable{},
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
		rawActions := sliceOrJSON(tableObject["row_actions"])
		for rowIndex, rawRow := range sliceOrJSON(tableObject["rows"]) {
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
				var action *pluginPageRowAction
				confirmation := pluginPageDeleteConfirmation[pluginID]
				if confirmation != "" && rowIndex < len(rawActions) {
					rawAction := objectValue(rawActions[rowIndex])
					recordID := strings.TrimSpace(text(rawAction["record_id"]))
					if text(rawAction["type"]) == "delete" && validPluginPageRecordID(pluginID, recordID) {
						action = &pluginPageRowAction{Type: "delete", RecordID: recordID, Confirmation: confirmation}
					}
				}
				table.RowActions = append(table.RowActions, action)
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
	token, ok := requireServiceToken(response, request)
	if !ok {
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
	if service.nativeArchivePage(id) {
		service.removeArchiveRecord(response, request, input.RecordID, input.Confirmation, input.Proof)
		return
	}
	if service.nativeSubscriptionHistory(id) {
		service.removeSubscriptionHistory(response, request, id, input.RecordID)
		return
	}
	if service.unavailablePlugin(response, request, id) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	result, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/plugin/page/action", token, url.Values{
		"id": {id}, "action": {"delete"}, "record_id": {input.RecordID}, "confirmation": {input.Confirmation},
	})
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "legacy service is unavailable")
		return
	}
	code := int(number(result["code"]))
	if code == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if code == 400 || code == 404 || code == 409 {
		message := text(result["message"])
		if message == "" {
			message = "plugin page action was rejected"
		}
		writeAPIError(response, code, code, message)
		return
	}
	if code != 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "plugin page action failed")
		return
	}
	message := cleanPluginPageText(result["message"], 200)
	if message == "" {
		message = "plugin page record deleted"
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": message})
}

func validPluginPageRecordID(pluginID, recordID string) bool {
	if pluginID == "MediaLibraryArchive" {
		return pluginPageArchiveIDPattern.MatchString(recordID)
	}
	return pluginPageDeleteConfirmation[pluginID] == "confirm" && pluginPageRecordIDPattern.MatchString(recordID)
}

func cleanPluginPageText(value any, limit int) string {
	cleaned := cleanPluginText(value, limit*4)
	cleaned = pluginPageURLPattern.ReplaceAllStringFunc(cleaned, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return parsed.Hostname()
	})
	runes := []rune(strings.TrimSpace(cleaned))
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return string(runes)
}

// The legacy form endpoint uses snake-case action keys; the React client
// consumes the typed page directly. Both expose the same rows and cursor.
func writeNativeRecordPage(w http.ResponseWriter, data pluginPageData, legacy bool) {
	if command, ok := w.(pluginCommandPageWriter); ok {
		command.writePage(data)
		return
	}
	if !legacy {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
		return
	}
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
