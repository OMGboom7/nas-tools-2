package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/pan115"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

// serveCompatNow preserves the old API envelope while keeping the request in Go.
func (service downloadService) serveCompatNow(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid download query")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	items, handled, err := service.nativeActiveFor(ctx, strings.TrimSpace(request.Form.Get("id")))
	if !handled {
		writeAPIError(response, http.StatusNotImplemented, 501, "downloader type is not migrated")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "download tasks are unavailable")
		return
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{
			"id": item.ID, "title": item.Title, "name": item.Title, "progress": item.Progress,
			"speed": item.Speed, "state": item.State, "site_url": item.SiteURL, "image": item.Image,
		})
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "", "data": map[string]any{"result": result}})
}

func (service downloadService) serveCompatHistory(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid history query")
		return
	}
	page, err := strconv.Atoi(request.Form.Get("page"))
	if err != nil || page < 1 || page > 1000 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid history page")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	items, err := service.nativeHistory(ctx, page)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "download history is unavailable")
		return
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		mediaType := "TV"
		if item.Type == "电影" {
			mediaType = "MOV"
		}
		result = append(result, map[string]any{
			"id": item.ID, "orgid": item.ID, "tmdbid": item.ID, "title": item.Title,
			"type": mediaType, "media_type": item.Type, "year": item.Year, "image": item.Image,
			"overview": item.Torrent, "date": item.Date, "site": item.Site,
		})
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "", "data": map[string]any{"Items": result}})
}

func (service downloadService) serveCompatControl(response http.ResponseWriter, request *http.Request, action string) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid download action")
		return
	}
	id := strings.TrimSpace(request.Form.Get("id"))
	if id == "" || len(id) > 256 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid download task id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	handled, err := service.nativeControl(ctx, id, action)
	if !handled {
		writeAPIError(response, http.StatusNotImplemented, 501, "downloader type is not migrated")
		return
	}
	if errors.Is(err, errDownloaderControlUnsupported) {
		writeAPIError(response, http.StatusNotImplemented, 501, "downloader task control is not migrated")
		return
	}
	if errors.Is(err, qbittorrent.ErrConfiguration) || errors.Is(err, transmission.ErrConfiguration) || errors.Is(err, aria2.ErrConfiguration) || errors.Is(err, pan115.ErrConfiguration) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid download task id")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "download task action failed")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "", "data": map[string]any{"id": id}})
}
