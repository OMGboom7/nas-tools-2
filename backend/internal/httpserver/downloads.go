package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/pan115"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

type downloadService struct {
	manualGuards **organization.Store
	pureGo       bool
	client       *http.Client
	images       *mediaImageProxy
	downloaders  *downloaderconfig.Store
	sites        *siteconfig.Store
	configStore  *config.Store
	systemConfig *systemconfig.Store
	resources    *searchcache.Store
	auth         *nativeAuthentication
	siteLimits   *siteRequestLimiter
	browser      browserruntime.Renderer
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
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, 401, 401, "missing authorization token")
		return
	}
	page := 1
	if raw := request.URL.Query().Get("historyPage"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeAPIError(response, 400, 400, "invalid history page")
			return
		}
		page = parsed
	}
	if service.downloaders == nil || service.systemConfig == nil {
		writeAPIError(response, 503, 503, "native download configuration is unavailable; no legacy request attempted")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	type result struct {
		name    string
		active  []downloadTask
		history []downloadHistory
		err     error
	}
	results := make(chan result, 2)
	go func() {
		items, handled, err := service.nativeActive(ctx)
		if !handled && err == nil {
			err = errors.New("native downloader is unavailable")
		}
		results <- result{name: "active", active: items, err: err}
	}()
	go func() {
		items, err := service.nativeHistory(ctx, page)
		results <- result{name: "history", history: items, err: err}
	}()
	data := downloadsData{Active: []downloadTask{}, History: []downloadHistory{}, HistoryPage: page, Warnings: []string{}}
	successful := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			data.Warnings = append(data.Warnings, r.name+" unavailable")
			continue
		}
		successful++
		if r.name == "active" {
			data.Active = r.active
		} else {
			data.History = r.history
		}
	}
	if successful == 0 {
		writeAPIError(response, 502, 502, "native download service is unavailable")
		return
	}
	writeJSON(response, 200, map[string]any{"code": 0, "success": true, "data": data})
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
	writeAPIError(response, 409, 409, "搜索结果来自旧缓存，请重新搜索后再下载；未提交任何任务")
}

func (service downloadService) control(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	action := request.PathValue("action")
	validAction := action == "start" || action == "stop" || action == "remove"
	if id == "" || len(id) > 256 || !validAction {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid download action")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	if handled, err := service.nativeControl(ctx, id, action); handled {
		if manualSubmissionError(response, err) {
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
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": ""})
		return
	}
	writeAPIError(response, 503, 503, "native downloader is unavailable; no legacy request attempted")
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
