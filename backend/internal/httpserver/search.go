package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type searchService struct {
	legacyURL string
	client    *http.Client
	images    *mediaImageProxy
	native    *nativeExternalResourceSearch
}

type searchRequest struct {
	Keyword string `json:"keyword"`
	Quick   bool   `json:"quick"`
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

	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var input searchRequest
	if err := decoder.Decode(&input); err != nil {
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
	if service.native != nil {
		data, handled, err := service.native.search(ctx, token, input.Keyword, input.Quick)
		if handled {
			if err != nil {
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
	form := url.Values{"search_word": {input.Keyword}}
	if input.Quick {
		form.Set("unident", "1")
	}
	started, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/search/keyword", token, form)
	if err != nil {
		writeJSON(response, http.StatusBadGateway, map[string]any{
			"code": 502, "success": false, "message": "legacy search service is unavailable",
		})
		return
	}
	if number(started["code"]) == 403 {
		writeJSON(response, http.StatusUnauthorized, map[string]any{
			"code": 401, "success": false, "message": "authorization token is invalid or expired",
		})
		return
	}
	if number(started["code"]) != 0 {
		writeJSON(response, http.StatusOK, map[string]any{
			"code": 0, "success": true,
			"data":    searchData{Keyword: input.Keyword, Items: []searchMedia{}},
			"message": text(started["msg"]),
		})
		return
	}

	result, err := postLegacy(ctx, service.client, service.legacyURL, "/api/v1/search/result", token, nil)
	if err != nil {
		writeJSON(response, http.StatusBadGateway, map[string]any{
			"code": 502, "success": false, "message": "search result is unavailable",
		})
		return
	}
	if number(result["code"]) == 403 {
		writeJSON(response, http.StatusUnauthorized, map[string]any{
			"code": 401, "success": false, "message": "authorization token is invalid or expired",
		})
		return
	}

	data := service.normalize(input.Keyword, legacyPayload(result))
	writeJSON(response, http.StatusOK, map[string]any{
		"code": 0, "success": true, "data": data,
	})
}

func (service searchService) normalize(keyword string, result map[string]any) searchData {
	data := searchData{
		Keyword: keyword,
		Total:   int(number(result["total"])),
		Items:   make([]searchMedia, 0),
	}
	mediaResults, _ := result["result"].(map[string]any)
	for _, value := range mediaResults {
		media, ok := value.(map[string]any)
		if !ok {
			continue
		}
		item := searchMedia{
			Key:       text(media["key"]),
			Title:     text(media["title"]),
			Year:      text(media["year"]),
			Type:      text(media["type"]),
			Vote:      text(media["vote"]),
			TMDBID:    text(media["tmdbid"]),
			Poster:    service.images.rewrite(text(media["poster"])),
			Overview:  text(media["overview"]),
			Exists:    text(media["fav"]) == "2",
			Resources: flattenResources(media["torrent_dict"]),
		}
		data.Items = append(data.Items, item)
	}
	sort.Slice(data.Items, func(left, right int) bool {
		return data.Items[left].Title < data.Items[right].Title
	})
	return data
}

func flattenResources(value any) []searchResource {
	resources := make([]searchResource, 0)
	for _, seasonEntry := range entries(value) {
		season := seasonEntry.key
		for _, groupEntry := range entries(seasonEntry.value) {
			group, _ := groupEntry.value.(map[string]any)
			for _, uniqueEntry := range entries(group["group_torrents"]) {
				unique, _ := uniqueEntry.value.(map[string]any)
				for _, torrentValue := range slice(unique["torrent_list"]) {
					torrent, ok := torrentValue.(map[string]any)
					if !ok {
						continue
					}
					resources = append(resources, searchResource{
						ID:             text(torrent["id"]),
						Season:         season,
						Name:           text(torrent["torrent_name"]),
						Description:    text(torrent["description"]),
						Site:           text(torrent["site"]),
						PageURL:        text(torrent["pageurl"]),
						Size:           text(torrent["size"]),
						Seeders:        int(number(torrent["seeders"])),
						Resolution:     text(torrent["respix"]),
						Medium:         text(torrent["restype"]),
						Effect:         text(torrent["reseffect"]),
						ReleaseGroup:   text(torrent["releasegroup"]),
						VideoCodec:     text(torrent["video_encode"]),
						Labels:         stringsOf(torrent["labels"]),
						UploadFactor:   number(torrent["uploadvalue"]),
						DownloadFactor: number(torrent["downloadvalue"]),
					})
				}
			}
		}
	}
	sort.SliceStable(resources, func(left, right int) bool {
		return resources[left].Seeders > resources[right].Seeders
	})
	return resources
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
