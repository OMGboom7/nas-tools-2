package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type siteService struct {
	client      *http.Client
	downloaders *downloaderconfig.Store
	store       *siteconfig.Store
	filters     *filterconfig.Store
	configStore *config.Store
	catalogPath string
}

type sitesData struct {
	Items []siteSummary `json:"items"`
}

type siteSummary struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Priority         int      `json:"priority"`
	Host             string   `json:"host"`
	Capabilities     []string `json:"capabilities"`
	RSSEnabled       bool     `json:"rssEnabled"`
	BrushEnabled     bool     `json:"brushEnabled"`
	StatisticEnabled bool     `json:"statisticEnabled"`
	ParseEnabled     bool     `json:"parseEnabled"`
	MessageEnabled   bool     `json:"messageEnabled"`
	BrowserEnabled   bool     `json:"browserEnabled"`
	ProxyEnabled     bool     `json:"proxyEnabled"`
}

type siteTestData struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Duration int    `json:"duration"`
	Message  string `json:"message"`
}

func (service siteService) serveList(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if service.store == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
		return
	}
	sites, err := service.nativeSiteList(ctx, false)
	result := map[string]any{"code": 0, "data": map[string]any{"sites": sites}}
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "site list request failed")
		return
	}
	if number(result["code"]) == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if number(result["code"]) != 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "site list request failed")
		return
	}

	payload := legacyPayload(result)
	items := make([]siteSummary, 0)
	for _, raw := range slice(payload["sites"]) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		rss := truthy(item["rss_enable"])
		brush := truthy(item["brush_enable"])
		statistic := truthy(item["statistic_enable"])
		parse := truthy(item["parse"])
		message := truthy(item["unread_msg_notify"])
		browser := truthy(item["chrome"])
		proxy := truthy(item["proxy"])
		capabilities := make([]string, 0, 7)
		for _, capability := range []struct {
			label   string
			enabled bool
		}{
			{label: "订阅", enabled: rss}, {label: "刷流", enabled: brush},
			{label: "统计", enabled: statistic}, {label: "解析", enabled: parse},
			{label: "消息", enabled: message}, {label: "仿真", enabled: browser},
			{label: "代理", enabled: proxy},
		} {
			if capability.enabled {
				capabilities = append(capabilities, capability.label)
			}
		}
		items = append(items, siteSummary{
			ID: text(item["id"]), Name: text(item["name"]), Priority: int(number(item["pri"])),
			Host: safeSiteHost(text(item["strict_url"])), Capabilities: capabilities,
			RSSEnabled: rss, BrushEnabled: brush, StatisticEnabled: statistic, ParseEnabled: parse,
			MessageEnabled: message, BrowserEnabled: browser, ProxyEnabled: proxy,
		})
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": sitesData{Items: items}})
}

func (service siteService) testConnection(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	parsedID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || parsedID < 1 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}

	if service.store == nil || service.configStore == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site configuration is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	item, err := service.store.Get(ctx, parsedID)
	if err != nil {
		if errors.Is(err, siteconfig.ErrNotFound) {
			writeAPIError(response, http.StatusNotFound, 404, "site not found")
		} else {
			writeAPIError(response, http.StatusInternalServerError, 500, "site configuration could not be loaded")
		}
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
	data := siteTestData{ID: id, OK: ok, Duration: int(time.Since(started).Milliseconds()), Message: message}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func safeSiteHost(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
