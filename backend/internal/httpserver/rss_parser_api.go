package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/rssparserconfig"
)

type rssParserAPI struct{ store *rssparserconfig.Store }

func (api rssParserAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Second)
	defer cancel()
	switch request.URL.Path {
	case "/api/v1/rss/parser/list":
		items, err := api.store.List(ctx)
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS parser list is unavailable")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"parsers": items}})
		return
	case "/api/v1/rss/parser/info", "/api/v1/rss/parser/delete", "/api/v1/rss/parser/update":
	default:
		writeAPIError(response, http.StatusNotFound, 404, "RSS parser endpoint not found")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 256<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS parser request")
		return
	}
	rawID := strings.TrimSpace(request.Form.Get("id"))
	id := int64(0)
	if rawID != "" {
		var err error
		id, err = strconv.ParseInt(rawID, 10, 64)
		if err != nil || id <= 0 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS parser id")
			return
		}
	}
	if request.URL.Path != "/api/v1/rss/parser/update" && id == 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "RSS parser id is required")
		return
	}
	switch request.URL.Path {
	case "/api/v1/rss/parser/info":
		item, err := api.store.Get(ctx, id)
		if errors.Is(err, rssparserconfig.ErrNotFound) {
			writeJSON(response, http.StatusOK, map[string]any{"code": 0, "detail": map[string]any{}})
			return
		}
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS parser is unavailable")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "detail": item})
	case "/api/v1/rss/parser/delete":
		if err := api.store.Delete(ctx, id); err != nil {
			if errors.Is(err, rssparserconfig.ErrNotFound) {
				writeJSON(response, http.StatusOK, map[string]any{"code": 1})
				return
			}
			writeAPIError(response, http.StatusBadGateway, 502, "RSS parser could not be deleted")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0})
	case "/api/v1/rss/parser/update":
		item := rssparserconfig.Parser{ID: id, Name: strings.TrimSpace(request.Form.Get("name")), Type: strings.TrimSpace(request.Form.Get("type")), Format: request.Form.Get("format"), Params: request.Form.Get("params")}
		if item.Name == "" || item.Type == "" || item.Format == "" || len(item.Name) > 200 || len(item.Type) > 32 || len(item.Format) > 128<<10 || len(item.Params) > 64<<10 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS parser configuration")
			return
		}
		if _, err := api.store.Upsert(ctx, item); err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS parser could not be saved")
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0})
	}
}
