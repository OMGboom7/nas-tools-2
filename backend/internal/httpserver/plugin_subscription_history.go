package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func (service pluginService) nativeSubscriptionHistory(id string) bool {
	return service.system != nil && systemconfig.SubscriptionHistoryTitle(id) != ""
}

func (service pluginService) historyReader(w http.ResponseWriter, r *http.Request) bool {
	admin, ok := service.catalogReader(w, r)
	if !ok {
		return false
	}
	// All three subscription-history plugins have auth_level 2 in the catalog.
	if !admin {
		writeAPIError(w, 403, 403, "仅管理员可访问订阅插件历史")
		return false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	return true
}

func subscriptionHistoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, systemconfig.ErrHistoryNotFound):
		writeAPIError(w, 404, 404, "未找到已安装的插件或历史记录")
	case errors.Is(err, systemconfig.ErrHistoryConflict):
		writeAPIError(w, 409, 409, "历史编号与存储记录不一致，请重新核对")
	default:
		writeAPIError(w, 503, 503, "插件历史数据库暂时不可用")
	}
}

func historyCell(value any) string {
	switch value.(type) {
	case string, json.Number:
		return cleanPluginPageText(value, 500)
	default:
		return ""
	}
}

func (service pluginService) subscriptionHistoryPage(w http.ResponseWriter, r *http.Request, id string, legacy bool) {
	if !service.historyReader(w, r) {
		return
	}
	var after int64
	if cursor := r.URL.Query().Get("after"); cursor != "" {
		n, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || n <= 0 || strconv.FormatInt(n, 10) != cursor {
			writeAPIError(w, 400, 400, "历史分页编号无效")
			return
		}
		after = n
	}
	records, more, err := service.system.SubscriptionHistory(r.Context(), id, after)
	if err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	table := pluginPageTable{Columns: []string{"标题", "类型", "状态", "添加时间"}, Rows: [][]string{}, RowActions: []*pluginPageRowAction{}}
	for _, record := range records {
		fields := record.Fields
		title := historyCell(fields["name"])
		if year := historyCell(fields["year"]); year != "" {
			title = strings.TrimSpace(title + " (" + year + ")")
		}
		if rating := historyCell(fields["rating"]); rating != "" && rating != "0" {
			title += " 评分：" + rating
		}
		status := "处理中"
		switch fields["state"] {
		case "DOWNLOADED":
			status = "已下载"
		case "RSS":
			status = "已订阅"
		case "NEW":
			status = "新增"
		}
		if fields == nil {
			title = "历史记录格式异常"
			status = ""
		}
		table.Rows = append(table.Rows, []string{title, historyCell(fields["type"]), status, historyCell(fields["add_time"])})
		key := record.HistoryKey()
		var action *pluginPageRowAction
		if key != "" {
			action = &pluginPageRowAction{Type: "delete", RecordID: key, Confirmation: "confirm"}
		}
		table.RowActions = append(table.RowActions, action)
	}
	data := pluginPageData{Title: systemconfig.SubscriptionHistoryTitle(id), Sections: []string{}, Tables: []pluginPageTable{table}, ReadOnly: true, CanDeleteRecords: true, DeleteConfirmation: "confirm"}
	if more {
		data.NextCursor = strconv.FormatInt(records[len(records)-1].RowID, 10)
	}
	writeNativeRecordPage(w, data, legacy)
}

func (service pluginService) removeSubscriptionHistory(w http.ResponseWriter, r *http.Request, id, key string) {
	if !service.historyReader(w, r) {
		return
	}
	if !validPluginPageRecordID(id, key) {
		writeAPIError(w, 400, 400, "历史记录编号无效")
		return
	}
	if err := service.system.DeleteSubscriptionHistory(r.Context(), id, key); err != nil {
		subscriptionHistoryError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "扩展页记录已删除"})
}
