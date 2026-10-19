package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type searchService struct {
	native *nativeExternalResourceSearch
}

type searchRequest struct {
	Keyword  string   `json:"keyword"`
	Quick    bool     `json:"quick"`
	Indexers []string `json:"indexers,omitempty"`
}

type searchData struct {
	Keyword  string        `json:"keyword"`
	Total    int           `json:"total"`
	Items    []searchMedia `json:"items"`
	Warnings []string      `json:"warnings,omitempty"`
}

type searchMedia struct {
	Key         string           `json:"key"`
	Title       string           `json:"title"`
	Year        string           `json:"year"`
	Type        string           `json:"type"`
	Vote        string           `json:"vote"`
	TMDBID      string           `json:"tmdbId"`
	Poster      string           `json:"poster"`
	Overview    string           `json:"overview"`
	Exists      bool             `json:"exists"`
	ExistsKnown *bool            `json:"existsKnown,omitempty"`
	Resources   []searchResource `json:"resources"`
}

type searchResource struct {
	ID              string   `json:"id"`
	Season          string   `json:"season"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Site            string   `json:"site"`
	PageURL         string   `json:"pageUrl"`
	Size            string   `json:"size"`
	Seeders         int      `json:"seeders"`
	Resolution      string   `json:"resolution"`
	Medium          string   `json:"medium"`
	Effect          string   `json:"effect"`
	ReleaseGroup    string   `json:"releaseGroup"`
	VideoCodec      string   `json:"videoCodec"`
	Labels          []string `json:"labels"`
	UploadFactor    float64  `json:"uploadFactor"`
	DownloadFactor  float64  `json:"downloadFactor"`
	PromotionKnown  *bool    `json:"promotionKnown,omitempty"`
	SeedersKnown    *bool    `json:"seedersKnown,omitempty"`
	MinimumSeedTime *int64   `json:"minimumSeedTime,omitempty"`
	MinimumRatio    *float64 `json:"minimumRatio,omitempty"`
	Exists          *bool    `json:"exists,omitempty"`
	ExistsKnown     *bool    `json:"existsKnown,omitempty"`
}

func (service searchService) serveHTTP(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeJSON(response, http.StatusUnauthorized, map[string]any{
			"code": 401, "success": false, "message": "missing authorization token",
		})
		return
	}

	request.Body = http.MaxBytesReader(response, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input searchRequest
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeJSON(response, http.StatusBadRequest, map[string]any{
			"code": 400, "success": false, "message": "invalid search request",
		})
		return
	}
	input.Keyword = strings.TrimSpace(input.Keyword)
	if input.Keyword == "" || len([]rune(input.Keyword)) > 120 {
		writeJSON(response, http.StatusBadRequest, map[string]any{
			"code": 400, "success": false, "message": "search keyword is required and must not exceed 120 characters",
		})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 90*time.Second)
	defer cancel()
	if service.native != nil && service.native.auth != nil && service.native.resources != nil && service.native.system != nil {
		data, handled, err := service.native.searchSelected(ctx, token, input.Keyword, input.Quick, input.Indexers)
		if handled {
			if err != nil {
				if errors.Is(err, errNativeSearchSelection) {
					writeAPIError(response, 400, 400, "requested search indexer is not enabled")
					return
				}
				if errors.Is(err, errNativeSearchPermission) {
					writeAPIError(response, 403, 403, "resource search permission is required")
					return
				}
				writeAPIError(response, 502, 502, "native indexer search could not be completed")
				return
			}
			writeJSON(response, 200, map[string]any{"code": 0, "success": true, "data": data})
			return
		}
	}
	writeAPIError(response, http.StatusServiceUnavailable, 503, "native search configuration is unavailable; no legacy request attempted")
}

type valueEntry struct {
	key   string
	value any
}

func entries(value any) []valueEntry {
	result := make([]valueEntry, 0)
	switch collection := value.(type) {
	case map[string]any:
		for key, item := range collection {
			result = append(result, valueEntry{key: key, value: item})
		}
	case []any:
		for _, rawEntry := range collection {
			pair, ok := rawEntry.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			result = append(result, valueEntry{key: text(pair[0]), value: pair[1]})
		}
	}
	return result
}

func slice(value any) []any {
	items, _ := value.([]any)
	return items
}

func stringsOf(value any) []string {
	items := slice(value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, text(item))
	}
	return result
}
