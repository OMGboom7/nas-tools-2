package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (service subscriptionService) serveCompatSubscriptionHistoryRedo(response http.ResponseWriter, request *http.Request) {
	if _, ok := requireServiceToken(response, request); !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription history request")
		return
	}
	kind := strings.ToUpper(strings.TrimSpace(request.PostForm.Get("type")))
	if kind != "MOV" && kind != "TV" {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription history type")
		return
	}
	service.serveNativeSubscriptionHistoryRedo(response, request, kind, request.PostForm.Get("rssid"), true)
}

func (service subscriptionService) serveNativeSubscriptionHistoryRedo(response http.ResponseWriter, request *http.Request, kind, rawID string, compat bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "valid history ID is required")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	if request.ParseForm() != nil {
		writeAPIError(response, 400, 400, "invalid history request")
		return
	}
	batch, lookupErr := service.fuzzyHistoryBatch(ctx, kind, id)
	if lookupErr != nil {
		writeAPIError(response, 409, 409, "original fuzzy history linkage could not be established")
		return
	}
	if batch != "" {
		requestID := request.PostForm.Get("request_id")
		if requestID == "" {
			requestID = fuzzyDigest([]any{"legacy-fuzzy-history-restore", batch, id})[:32]
		}
		created, err := restoreFuzzyRule(ctx, service, batch, requestID, id)
		if errors.Is(err, errSubscriptionAlreadyExists) {
			writeJSON(response, 409, map[string]any{"code": 9, "success": false, "message": "订阅已存在"})
			return
		}
		if err != nil {
			writeAPIError(response, 409, 409, "original fuzzy rule could not be restored without overwriting current data")
			return
		}
		writeJSON(response, 200, map[string]any{"code": 0, "success": true, "message": "原模糊规则已恢复，执行授权保持关闭", "data": map[string]any{"id": strconv.FormatInt(created, 10)}})
		return
	}
	created, handled, restoreErr := service.restoreEditionHistory(ctx, kind, id, request.PostForm.Get("request_id"))
	if handled || restoreErr != nil {
		if errors.Is(restoreErr, errSubscriptionAlreadyExists) {
			writeJSON(response, 409, map[string]any{"code": 9, "success": false, "message": "订阅已存在"})
			return
		}
		if restoreErr != nil {
			writeAPIError(response, 409, 409, "original edition rule could not be restored without overwriting current data")
			return
		}
		writeJSON(response, 200, map[string]any{"code": 0, "success": true, "message": "原洗版订阅配置已恢复并排队，未直接提交下载", "data": map[string]any{"id": strconv.FormatInt(created, 10)}})
		return
	}
	input, err := service.nativeHistorySubscriptionInput(ctx, kind, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(response, http.StatusNotFound, 404, "subscription history not found")
			return
		}
		writeAPIError(response, http.StatusBadGateway, 502, "subscription history is unavailable")
		return
	}
	if err := service.applyCompatSubscriptionDefaults(ctx, &input); err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "default subscription settings are unavailable")
		return
	}
	if message := validateSubscription(&input); message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	if input.MediaID == "" || strings.HasPrefix(input.MediaID, "BG:") || strings.HasPrefix(input.MediaID, "DB:") {
		input, err = service.resolveNativeSubscriptionInput(ctx, input)
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "TMDB subscription search is unavailable")
			return
		}
		if input.MediaID == "" {
			writeAPIError(response, http.StatusNotFound, 404, "matching media was not found")
			return
		}
	}
	metadata, err := service.fetchNativeSubscriptionMetadata(ctx, input)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "TMDB subscription metadata is unavailable")
		return
	}
	subscriptionID, err := service.upsertNativeSubscription(ctx, input, metadata)
	if err != nil {
		if errors.Is(err, errSubscriptionAlreadyExists) {
			writeJSON(response, http.StatusConflict, map[string]any{"code": 9, "success": false, "msg": "订阅已存在"})
			return
		}
		writeAPIError(response, http.StatusBadGateway, 502, "subscription storage is unavailable")
		return
	}
	if compat {
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "msg": "添加订阅成功"})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "添加订阅成功", "data": map[string]any{"id": strconv.FormatInt(subscriptionID, 10)}})
}

func (service subscriptionService) nativeHistorySubscriptionInput(ctx context.Context, kind string, id int64) (subscriptionUpsertRequest, error) {
	input := subscriptionUpsertRequest{Type: kind}
	database, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return input, err
	}
	defer database.Close()
	var tableExists int
	if err := database.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='RSS_HISTORY')").Scan(&tableExists); err != nil {
		return input, err
	}
	if tableExists == 0 {
		return input, sql.ErrNoRows
	}
	var name, year, mediaID, season sql.NullString
	var total, start sql.NullInt64
	err = database.QueryRowContext(ctx, "SELECT NAME,YEAR,TMDBID,SEASON,TOTAL,START FROM RSS_HISTORY WHERE ID=? AND TYPE=?", id, kind).
		Scan(&name, &year, &mediaID, &season, &total, &start)
	if err != nil {
		return input, err
	}
	input.Name, input.Year, input.MediaID = name.String, year.String, mediaID.String
	input.Season = strings.TrimPrefix(strings.ToUpper(season.String), "S")
	if total.Valid {
		value := int(total.Int64)
		input.TotalEpisodes = &value
	}
	if start.Valid {
		value := int(start.Int64)
		input.CurrentEpisode = &value
	}
	return input, nil
}
