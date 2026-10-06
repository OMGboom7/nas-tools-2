package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

func (service downloadService) addTorrent(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 9<<20)
	err := request.ParseMultipartForm(1 << 20)
	if request.MultipartForm != nil {
		defer request.MultipartForm.RemoveAll()
	}
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid torrent upload")
		return
	}
	if request.MultipartForm == nil || len(request.MultipartForm.File) != 1 || len(request.MultipartForm.File["torrent"]) != 1 || len(request.MultipartForm.Value) != 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "one torrent file is required")
		return
	}
	file, header, err := request.FormFile("torrent")
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "torrent file is required")
		return
	}
	defer file.Close()
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".torrent") || header.Size <= 0 || header.Size > 8<<20 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid torrent file")
		return
	}
	contents, err := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if err != nil || len(contents) == 0 || len(contents) > 8<<20 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid torrent file")
		return
	}
	if _, err := torrentmeta.Parse(contents); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid torrent file")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	id, handled, err := service.nativeAddTorrent(ctx, contents)
	if !handled {
		writeAPIError(response, http.StatusNotImplemented, 501, "downloader type is not migrated")
		return
	}
	if errors.Is(err, qbittorrent.ErrConfiguration) || errors.Is(err, transmission.ErrConfiguration) || errors.Is(err, aria2.ErrConfiguration) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader configuration")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "torrent task could not be added")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"id": id}})
}
