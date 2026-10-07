package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/pan115"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

type downloadService struct {
	legacyURL    string
	client       *http.Client
	images       *mediaImageProxy
	downloaders  *downloaderconfig.Store
	sites        *siteconfig.Store
	configStore  *config.Store
	systemConfig *systemconfig.Store
	resources    *searchcache.Store
	auth         *nativeAuthentication
	siteLimits   *siteRequestLimiter
}

type downloadsData struct {
	Active      []downloadTask    `json:"active"`
	History     []downloadHistory `json:"history"`
	HistoryPage int               `json:"historyPage"`
	Warnings    []string          `json:"warnings"`
}

type downloadTask struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Progress     float64 `json:"progress"`
	Speed        string  `json:"speed"`
	State        string  `json:"state"`
	SiteURL      string  `json:"siteUrl"`
	Image        string  `json:"image"`
	CanControl   bool    `json:"canControl"`
	CanRemove    bool    `json:"canRemove"`
	ShowProgress bool    `json:"showProgress"`
}

type downloadHistory struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Year    string `json:"year"`
	Image   string `json:"image"`
	Torrent string `json:"torrent"`
	Date    string `json:"date"`
	Site    string `json:"site"`
}

type addResourceRequest struct {
	ResourceID string `json:"resourceId"`
	Directory  string `json:"directory"`
	Setting    string `json:"setting"`
}

func (service downloadService) serveList(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	page := 1
	if raw := request.URL.Query().Get("historyPage"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid history page")
			return
		}
		page = parsed
	}

	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	type result struct {
		name    string
		value   map[string]any
		active  []downloadTask
		history []downloadHistory
		native  bool
		err     error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, item := range []struct {
		name string
		path string
		form url.Values
	}{
		{name: "active", path: "/api/v1/download/now", form: url.Values{"force_list": {"true"}}},
		{name: "history"},
	} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if item.name == "active" {
				items, handled, err := service.nativeActive(ctx)
				if handled {
					results <- result{name: item.name, active: items, native: true, err: err}
					return
				}
			} else {
				items, err := service.nativeHistory(ctx, page)
				results <- result{name: item.name, history: items, native: true, err: err}
				return
			}
			value, err := postLegacy(ctx, service.client, service.legacyURL, item.path, token, item.form)
			results <- result{name: item.name, value: value, err: err}
		}()
	}
	wait.Wait()
	close(results)

	data := downloadsData{Active: []downloadTask{}, History: []downloadHistory{}, HistoryPage: page, Warnings: []string{}}
	successful := 0
	unauthorized := 0
	for result := range results {
		if result.err != nil || !result.native && number(result.value["code"]) != 0 {
			if result.err == nil && !result.native && number(result.value["code"]) == 403 {
				unauthorized++
			}
			data.Warnings = append(data.Warnings, result.name+" unavailable")
			continue
		}
		successful++
		if result.native {
			if result.name == "active" {
				data.Active = result.active
			} else {
				data.History = result.history
			}
			continue
		}
		payload := legacyPayload(result.value)
		if result.name == "active" {
			data.Active = service.normalizeActive(payload["result"])
		} else {
			data.History = service.normalizeHistory(payload["Items"])
		}
	}
	if successful == 0 {
		if unauthorized == 2 {
			writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
			return
		}
		writeAPIError(response, http.StatusBadGateway, 502, "download service is unavailable")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service downloadService) addResource(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	var input addResourceRequest
	if !decodeServiceRequest(response, request, &input, "invalid download request") {
		return
	}
	input.ResourceID = strings.TrimSpace(input.ResourceID)
	if input.ResourceID == "" || len(input.ResourceID) > 256 {
		writeAPIError(response, http.StatusBadRequest, 400, "resource id is required")
		return
	}
	if strings.HasPrefix(input.ResourceID, searchcache.Prefix) {
		service.addNativeSearchResource(response, request, input)
		return
	}
	form := url.Values{"id": {input.ResourceID}}
	if input.Directory != "" {
		form.Set("dir", input.Directory)
	}
	if input.Setting != "" {
		form.Set("setting", input.Setting)
	}
	service.forwardMutation(response, request, token, "/api/v1/download/search", form, "download could not be started")
}

func (service downloadService) control(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	action := request.PathValue("action")
	legacyAction := map[string]string{"start": "start", "stop": "stop", "remove": "remove"}[action]
	if id == "" || len(id) > 256 || legacyAction == "" {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid download action")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	if handled, err := service.nativeControl(ctx, id, action); handled {
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
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": ""})
		return
	}
	service.forwardMutation(response, request, token, "/api/v1/download/"+legacyAction, url.Values{"id": {id}}, "download action failed")
}

func (service downloadService) forwardMutation(response http.ResponseWriter, request *http.Request, token, path string, form url.Values, fallback string) {
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	result, err := postLegacy(ctx, service.client, service.legacyURL, path, token, form)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "legacy download service is unavailable")
		return
	}
	code := int(number(result["code"]))
	if code == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if code != 0 || result["success"] == false {
		message := text(result["message"])
		if message == "" {
			message = fallback
		}
		writeJSON(response, http.StatusBadRequest, map[string]any{"code": code, "success": false, "message": message})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": text(result["message"])})
}

func (service downloadService) normalizeActive(value any) []downloadTask {
	items := make([]downloadTask, 0)
	for _, raw := range slice(value) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		progress := number(item["progress"])
		if progress < 0 {
			progress = 0
		} else if progress > 100 {
			progress = 100
		}
		title := text(item["title"])
		if title == "" {
			title = text(item["name"])
		}
		image := text(item["image"])
		if service.images != nil {
			image = service.images.rewrite(image)
		}
		items = append(items, downloadTask{
			ID: text(item["id"]), Title: title, Progress: progress, Speed: text(item["speed"]),
			State: text(item["state"]), SiteURL: text(item["site_url"]), Image: image,
			CanControl: !truthy(item["nomenu"]), CanRemove: !truthy(item["nomenu"]), ShowProgress: !truthy(item["noprogress"]),
		})
	}
	return items
}

func (service downloadService) normalizeHistory(value any) []downloadHistory {
	items := make([]downloadHistory, 0)
	for _, raw := range slice(value) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		image := text(item["image"])
		if service.images != nil {
			image = service.images.rewrite(image)
		}
		items = append(items, downloadHistory{
			ID: text(item["id"]), Title: text(item["title"]), Type: text(item["media_type"]),
			Year: text(item["year"]), Image: image, Torrent: text(item["overview"]),
			Date: text(item["date"]), Site: text(item["site"]),
		})
	}
	return items
}

func truthy(value any) bool {
	switch item := value.(type) {
	case bool:
		return item
	case string:
		return item == "1" || strings.EqualFold(item, "true")
	default:
		return number(value) != 0
	}
}

func writeAPIError(response http.ResponseWriter, status, code int, message string) {
	writeJSON(response, status, map[string]any{"code": code, "success": false, "message": message})
}
