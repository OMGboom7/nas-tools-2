package httpserver

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (service subscriptionService) serveCompatMovieList(response http.ResponseWriter, request *http.Request) {
	service.serveCompatSubscriptions(response, request, "RSS_MOVIES")
}

func (service subscriptionService) serveCompatTVList(response http.ResponseWriter, request *http.Request) {
	service.serveCompatSubscriptions(response, request, "RSS_TVS")
}

func (service subscriptionService) serveCompatHistoryList(response http.ResponseWriter, request *http.Request) {
	service.serveCompatSubscriptions(response, request, "RSS_HISTORY")
}

func (service subscriptionService) serveCompatSubscriptions(response http.ResponseWriter, request *http.Request, table string) {
	if _, ok := requireServiceToken(response, request); !ok {
		return
	}
	if service.databasePath == "" {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native subscription storage is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription list request")
		return
	}
	requestedType := strings.ToUpper(strings.TrimSpace(request.Form.Get("type")))
	if table == "RSS_HISTORY" && requestedType != "" && requestedType != "MOV" && requestedType != "TV" {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription type")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	database, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "subscription list is unavailable")
		return
	}
	defer database.Close()
	rows, err := readSubscriptionRows(ctx, database, table)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "subscription list is unavailable")
		return
	}
	if table == "RSS_HISTORY" {
		result := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if requestedType == "" || text(row["TYPE"]) == requestedType {
				result = append(result, row)
			}
		}
		sort.SliceStable(result, func(left, right int) bool {
			return text(result[left]["FINISH_TIME"]) > text(result[right]["FINISH_TIME"])
		})
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "result": result})
		return
	}
	result := make(map[string]any, len(rows))
	for _, row := range rows {
		item := normalizeNativeSubscriptionRow(row)
		id := text(item["id"])
		result[id] = map[string]any{
			"id": item["id"], "name": item["name"], "year": item["year"], "season": item["season"],
			"tmdbid": item["tmdbid"], "image": item["image"], "overview": item["overview"],
			"rss_sites": item["rss_sites"], "search_sites": item["search_sites"],
			"over_edition": truthy(item["over_edition"]), "filter_restype": item["filter_restype"],
			"filter_pix": item["filter_pix"], "filter_team": item["filter_team"], "filter_rule": item["filter_rule"],
			"filter_include": item["filter_include"], "filter_exclude": item["filter_exclude"],
			"save_path": item["save_path"], "download_setting": item["download_setting"],
			"fuzzy_match": truthy(item["fuzzy_match"]), "state": item["state"],
			"poster": item["poster"], "keyword": item["keyword"],
			"total": item["total"], "lack": item["lack"], "total_ep": item["total_ep"], "current_ep": item["current_ep"],
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "result": result})
}
