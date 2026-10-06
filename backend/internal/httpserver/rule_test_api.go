package httpserver

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
)

type ruleTestAPI struct {
	filters *filterconfig.Store
	words   *wordconfig.Store
}

func (api ruleTestAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if api.filters == nil || api.words == nil {
		writeAPIError(response, 503, 503, "native rule test is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 256<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, 400, 400, "invalid rule test request")
		return
	}
	title, subtitle := request.Form.Get("title"), request.Form.Get("subtitle")
	if strings.TrimSpace(title) == "" {
		writeJSON(response, http.StatusOK, map[string]any{"code": -1})
		return
	}
	if len(title) > 64<<10 || len(subtitle) > 64<<10 {
		writeAPIError(response, 400, 400, "rule test input too large")
		return
	}
	size := 0.0
	var err error
	if raw := request.Form.Get("size"); raw != "" {
		size, err = strconv.ParseFloat(raw, 64)
		if err != nil || size < 0 || math.IsNaN(size) || math.IsInf(size, 0) || size > 1e9 {
			writeAPIError(response, 400, 400, "invalid size")
			return
		}
	}
	group := int64(0)
	if raw := strings.TrimSpace(request.Form.Get("rulegroup")); raw != "" {
		group, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || group < -1 {
			writeAPIError(response, 400, 400, "invalid rule group")
			return
		}
	}
	_, words, err := api.words.List(request.Context())
	if err != nil {
		writeAPIError(response, 500, 500, "custom words are unavailable")
		return
	}
	processed, err := wordconfig.ProcessWords(request.Context(), title, words)
	if err != nil {
		writeAPIError(response, 500, 500, "custom words are unavailable")
		return
	}
	processedSubtitle, err := wordconfig.ProcessWords(request.Context(), subtitle, words)
	if err != nil {
		writeAPIError(response, 500, 500, "custom words are unavailable")
		return
	}
	episodes, err := mediameta.Episodes(processed.Title, processedSubtitle.Title)
	if err != nil {
		writeAPIError(response, 400, 400, "invalid episode metadata")
		return
	}
	result, err := api.filters.Match(request.Context(), group, filterconfig.TorrentMetadata{Title: processed.Title, Subtitle: processedSubtitle.Title, SizeBytes: size * (1 << 30), Movie: !episodes.TV, Episodes: episodes.Count})
	if err != nil {
		status := 500
		if errors.Is(err, filterconfig.ErrNotFound) {
			status = 404
		}
		if errors.Is(err, filterconfig.ErrInvalidRule) {
			status = 422
		}
		writeAPIError(response, status, status, "rule test failed")
		return
	}
	message, order := "未匹配", 0
	if result.Matched {
		message = "匹配"
		if result.Order != 0 {
			order = 100 - result.Order
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "flag": result.Matched, "text": message, "order": order})
}
