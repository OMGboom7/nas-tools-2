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
	Title              string            `json:"title"`
	Sections           []string          `json:"sections"`
	Tables             []pluginPageTable `json:"tables"`
	ReadOnly           bool              `json:"readOnly"`
	ActionsOmitted     bool              `json:"actionsOmitted"`
	CanDeleteRecords   bool              `json:"canDeleteRecords"`
	DeleteConfirmation string            `json:"deleteConfirmation"`
}

type pluginPageActionRequest struct {
	RecordID     string `json:"recordId"`
	Confirmation string `json:"confirmation"`
}

func (service pluginService) servePage(response http.ResponseWriter, request *http.Request) {
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
