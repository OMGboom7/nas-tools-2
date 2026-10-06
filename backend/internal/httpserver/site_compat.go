package httpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func parseSiteForm(response http.ResponseWriter, request *http.Request) bool {
	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site form")
		return false
	}
	return true
}

func (service siteService) serveCompatList(response http.ResponseWriter, request *http.Request) {
	if service.store == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
		return
	}
	if !parseSiteForm(response, request) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	items, err := service.store.List(ctx)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "site list request failed")
		return
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		record, err := nativeSiteRecord(item)
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, 500, "site list request failed")
			return
		}
		if request.Form.Get("rss") == "1" && !truthy(record["rss_enable"]) || request.Form.Get("brush") == "1" && !truthy(record["brush_enable"]) || request.Form.Get("statistic") == "1" && !truthy(record["statistic_enable"]) {
			continue
		}
		if request.Form.Get("basic") == "1" {
			result = append(result, map[string]any{"id": item.ID, "name": item.Name})
		} else {
			result = append(result, record)
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "sites": result})
}

func (service siteService) serveCompatInfo(response http.ResponseWriter, request *http.Request) {
	if service.store == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
		return
	}
	if !parseSiteForm(response, request) {
		return
	}
	id, err := strconv.ParseInt(request.Form.Get("id"), 10, 64)
	if err != nil || id < 1 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	item, err := service.store.Get(ctx, id)
	if errors.Is(err, siteconfig.ErrNotFound) {
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "site": map[string]any{}, "site_free": false, "site_2xfree": false, "site_hr": false})
		return
	}
	if err != nil {
		writeNativeSiteError(response, err)
		return
	}
	record, err := nativeSiteRecord(item)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "stored site attributes are invalid")
		return
	}
	capabilities := map[string]bool{"FREE": false, "2XFREE": false, "HR": false}
	if service.catalogPath != "" && item.SignURL != "" {
		catalog, err := indexercatalog.Load(service.catalogPath)
		if err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, 503, "site capability catalog is unavailable")
			return
		}
		for key := range capabilities {
			capabilities[key] = catalog.HasTrackerRule(item.SignURL, key)
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "site": record,
		"site_free": capabilities["FREE"], "site_2xfree": capabilities["2XFREE"], "site_hr": capabilities["HR"]})
}

func (service siteService) serveCompatUpdate(response http.ResponseWriter, request *http.Request) {
	if service.store == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
		return
	}
	if !parseSiteForm(response, request) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	item := siteconfig.Site{}
	if raw := request.Form.Get("site_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
			return
		}
		item, err = service.store.Get(ctx, id)
		if err != nil {
			writeNativeSiteError(response, err)
			return
		}
	}
	if request.Form.Has("site_name") {
		item.Name = strings.TrimSpace(request.Form.Get("site_name"))
	}
	if item.Name == "" || len([]rune(item.Name)) > 80 {
		writeAPIError(response, http.StatusBadRequest, 400, "site name is required")
		return
	}
	for _, field := range []struct {
		key   string
		value *string
	}{
		{"site_pri", &item.Priority}, {"site_rssurl", &item.RSSURL}, {"site_signurl", &item.SignURL},
		{"site_cookie", &item.Cookie}, {"site_api_key", &item.APIKey}, {"site_include", &item.Include},
	} {
		if request.Form.Has(field.key) {
			*field.value = strings.TrimSpace(request.Form.Get(field.key))
		}
	}
	if item.SignURL == "" && item.RSSURL == "" || item.SignURL != "" && !validSiteURL(item.SignURL) || item.RSSURL != "" && !validSiteURL(item.RSSURL) {
		writeAPIError(response, http.StatusBadRequest, 400, "valid site address is required")
		return
	}
	if item.Priority != "" {
		priority, err := strconv.Atoi(item.Priority)
		if err != nil || priority < 0 || priority > 50 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid site priority")
			return
		}
	}
	if request.Form.Has("site_note") {
		changes := map[string]any{}
		if raw := strings.TrimSpace(request.Form.Get("site_note")); raw != "" && json.Unmarshal([]byte(raw), &changes) != nil {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid site attributes")
			return
		}
		stored := map[string]any{}
		if item.Note != "" && item.Note != "null" && json.Unmarshal([]byte(item.Note), &stored) != nil {
			writeAPIError(response, http.StatusInternalServerError, 500, "stored site attributes are invalid")
			return
		}
		for key, value := range changes {
			stored[key] = value
		}
		encoded, err := json.Marshal(stored)
		if err != nil {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid site attributes")
			return
		}
		item.Note = string(encoded)
	}
	if _, err := service.store.Upsert(ctx, item); err != nil {
		writeNativeSiteError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": "200", "success": true})
}

func (service siteService) serveCompatDelete(response http.ResponseWriter, request *http.Request) {
	if service.store == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
		return
	}
	if !parseSiteForm(response, request) {
		return
	}
	id, err := strconv.ParseInt(request.Form.Get("id"), 10, 64)
	if err != nil || id < 1 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if err := service.store.Delete(ctx, id); err != nil && !errors.Is(err, siteconfig.ErrNotFound) {
		writeNativeSiteError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": true, "success": true})
}

func (service siteService) serveCompatTest(response http.ResponseWriter, request *http.Request) {
	if service.store == nil || service.configStore == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site configuration is unavailable")
		return
	}
	if !parseSiteForm(response, request) {
		return
	}
	id, err := strconv.ParseInt(request.Form.Get("id"), 10, 64)
	if err != nil || id < 1 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	item, err := service.store.Get(ctx, id)
	if errors.Is(err, siteconfig.ErrNotFound) {
		writeJSON(response, http.StatusOK, map[string]any{"code": -1, "msg": "站点不存在", "time": 0})
		return
	}
	if err != nil {
		writeNativeSiteError(response, err)
		return
	}
	configuration, err := service.configStore.Snapshot()
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "site configuration could not be loaded")
		return
	}
	started := time.Now()
	ok, message, unsupported := checkSiteConnection(ctx, item, objectValue(configuration["app"]), service.client.Transport)
	if unsupported {
		writeAPIError(response, http.StatusNotImplemented, 501, message)
		return
	}
	code := -1
	if ok {
		code = 0
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": code, "msg": message, "time": time.Since(started).Milliseconds()})
}

func (service siteService) serveCompatCookieUpdate(response http.ResponseWriter, request *http.Request) {
	if service.store == nil || service.configStore == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site configuration is unavailable")
		return
	}
	if !service.requireSiteAPIKey(response, request) {
		return
	}
	if !parseSiteForm(response, request) {
		return
	}
	id, err := strconv.ParseInt(request.Form.Get("site_id"), 10, 64)
	if err != nil || id < 1 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if err := service.store.UpdateCookieAndAgent(ctx, id, request.Form.Get("site_cookie"), strings.TrimSpace(request.Form.Get("site_ua"))); err != nil {
		writeNativeSiteError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "messages": "请求发送成功"})
}

func (service siteService) requireSiteAPIKey(response http.ResponseWriter, request *http.Request) bool {
	configuration, err := service.configStore.Snapshot()
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "site configuration could not be loaded")
		return false
	}
	secret := strings.TrimSpace(text(objectValue(configuration["security"])["api_key"]))
	provided := strings.TrimSpace(request.Header.Get("Authorization"))
	if parts := strings.Fields(provided); len(parts) != 0 {
		provided = parts[len(parts)-1]
	}
	queryKey := request.URL.Query().Get("apikey")
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(provided)) != 1 && subtle.ConstantTimeCompare([]byte(secret), []byte(queryKey)) != 1 {
		writeAPIError(response, http.StatusUnauthorized, 401, "API Key 无效")
		return false
	}
	return true
}

func (service siteService) serveCompatAPISites(response http.ResponseWriter, request *http.Request) {
	if service.store == nil || service.configStore == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site configuration is unavailable")
		return
	}
	if !service.requireSiteAPIKey(response, request) {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	items, err := service.nativeSiteList(ctx, false)
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "site list request failed")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"user_sites": items}})
}
