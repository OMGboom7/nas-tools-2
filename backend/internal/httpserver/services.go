package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type serviceOverviewService struct {
	client       *http.Client
	configStore  *config.Store
	systemConfig *systemconfig.Store
	downloaders  *downloaderconfig.Store
	sites        *siteconfig.Store
	catalogPath  string
}

type serviceComponent struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Host        string `json:"host"`
	Summary     string `json:"summary"`
	Configured  bool   `json:"configured"`
	Enabled     bool   `json:"enabled"`
	Active      bool   `json:"active"`
	Default     bool   `json:"default"`
	Monitoring  bool   `json:"monitoring"`
	CanTest     bool   `json:"canTest"`
	SourceCount int    `json:"sourceCount"`
}

type servicesData struct {
	Downloaders  []serviceComponent `json:"downloaders"`
	MediaServers []serviceComponent `json:"mediaServers"`
	Indexers     []serviceComponent `json:"indexers"`
	Warnings     []string           `json:"warnings"`
}

type serviceTestData struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Duration int64  `json:"duration"`
	Message  string `json:"message"`
}

func (service serviceOverviewService) serveList(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	type result struct {
		name  string
		value map[string]any
		err   error
	}
	requests := []string{
		"config",
		"downloaders",
		"indexers",
	}
	results := make(chan result, len(requests))
	var wait sync.WaitGroup
	for _, name := range requests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if name == "config" {
				if service.configStore == nil {
					results <- result{name: name, err: errors.New("native configuration store unavailable")}
				} else {
					snapshot, err := service.configStore.Snapshot()
					results <- result{name: name, value: map[string]any{
						"code": 0, "success": true, "data": snapshot,
					}, err: err}
				}
				return
			}
			if name == "downloaders" {
				if service.downloaders == nil {
					results <- result{name: name, err: errors.New("native downloader store unavailable")}
				} else {
					payload, err := service.downloaderPayload(ctx, 0)
					results <- result{name: name, value: map[string]any{
						"code": 0, "success": true, "data": payload,
					}, err: err}
				}
				return
			}
			if name == "indexers" {
				payload, handled, err := (subscriptionService{
					catalogPath: service.catalogPath, configStore: service.configStore,
					systemConfig: service.systemConfig, sites: service.sites,
				}).nativeIndexerOptions(ctx)
				if !handled && err == nil {
					err = errors.New("native indexer catalog unavailable")
				}
				results <- result{name: name, value: payload, err: err}
				return
			}
		}()
	}
	wait.Wait()
	close(results)

	data := servicesData{Downloaders: []serviceComponent{}, MediaServers: []serviceComponent{}, Indexers: []serviceComponent{}, Warnings: []string{}}
	values := map[string]map[string]any{}
	unauthorized, successful := 0, 0
	for result := range results {
		if result.err != nil || number(result.value["code"]) != 0 {
			if result.err == nil && number(result.value["code"]) == 403 {
				unauthorized++
			}
			data.Warnings = append(data.Warnings, result.name+" unavailable")
			continue
		}
		successful++
		values[result.name] = legacyPayload(result.value)
	}
	if successful == 0 {
		if unauthorized == len(requests) {
			writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
			return
		}
		writeAPIError(response, http.StatusBadGateway, 502, "service overview is unavailable")
		return
	}
	data.Downloaders = normalizeDownloaders(values["downloaders"])
	data.MediaServers = normalizeMediaServers(values["config"], nil)
	data.Indexers = normalizeIndexer(values["config"], values["indexers"])
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service serviceOverviewService) downloaderPayload(ctx context.Context, id int64) (map[string]any, error) {
	defaultID := ""
	if service.systemConfig != nil {
		value, err := service.systemConfig.Get(ctx, "DefaultDownloader")
		if err != nil {
			return nil, err
		}
		defaultID = value
	}
	if id != 0 {
		item, err := service.downloaders.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"detail": downloaderRecord(item, defaultID)}, nil
	}
	items, err := service.downloaders.List(ctx)
	if err != nil {
		return nil, err
	}
	detail := make(map[string]any, len(items))
	for _, item := range items {
		detail[strconv.FormatInt(item.ID, 10)] = downloaderRecord(item, defaultID)
	}
	return map[string]any{"detail": detail}, nil
}

func downloaderRecord(item downloaderconfig.Downloader, defaultID string) map[string]any {
	configuration := map[string]any{}
	if err := json.Unmarshal([]byte(item.Config), &configuration); err != nil {
		configuration = map[string]any{}
	}
	directories := []any{}
	if err := json.Unmarshal([]byte(item.DownloadDir), &directories); err != nil {
		directories = []any{}
	}
	return map[string]any{
		"id": item.ID, "name": item.Name, "type": item.Type,
		"enabled": item.Enabled, "transfer": item.Transfer,
		"only_nastool": item.OnlyNastool, "match_path": item.MatchPath,
		"rmt_mode": item.RmtMode, "config": configuration,
		"download_dir": directories, "is_default": strconv.FormatInt(item.ID, 10) == defaultID,
	}
}

func (service serviceOverviewService) test(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	kind, id := request.PathValue("kind"), strings.TrimSpace(request.PathValue("id"))
	if id == "" {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid service id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	var result map[string]any
	var err error
	message := ""

	switch kind {
	case "downloader":
		if _, ok := validServiceID(id); !ok {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader id")
			return
		}
		var detail map[string]any
		detail, result, err = service.downloaderDetail(ctx, id)
		if err == nil && result == nil {
			if service.downloaders != nil {
				configuration, _ := detail["config"].(map[string]any)
				handled, checkErr, clientName := nativeDownloaderCheck(ctx, text(detail["type"]), configuration, service.client.Transport)
				if handled {
					ok := checkErr == nil
					message := "连接正常"
					if !ok {
						message = clientName + " 连接或认证失败，请检查地址、凭据和 TLS 证书"
					}
					data := serviceTestData{Kind: kind, ID: id, OK: ok, Duration: time.Since(started).Milliseconds(), Message: message}
					writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
					return
				}
			}
			writeAPIError(response, http.StatusNotImplemented, 501, "downloader type is not migrated")
			return
		}
	case "media":
		_, mediaName, valid := validMediaType(id)
		if !valid {
			writeAPIError(response, http.StatusBadRequest, 400, "unsupported media server")
			return
		}
		if service.configStore == nil {
			writeAPIError(response, http.StatusServiceUnavailable, 503, "native configuration store is unavailable")
			return
		}
		snapshot, snapshotErr := service.configStore.Snapshot()
		if snapshotErr != nil {
			writeAPIError(response, http.StatusInternalServerError, 500, "media server configuration could not be loaded")
			return
		}
		configuration := objectValue(snapshot[id])
		credential := text(configuration["api_key"])
		if id == "plex" {
			credential = text(configuration["token"])
			if credential == "" && (text(configuration["username"]) != "" || text(configuration["password"]) != "") {
				writeAPIError(response, http.StatusNotImplemented, 501, "Plex account discovery is not migrated; configure a server token")
				return
			}
		}
		client, clientErr := mediaserver.New(id, text(configuration["host"]), credential, service.client.Transport)
		if clientErr == nil {
			clientErr = client.Check(ctx)
		}
		ok := clientErr == nil
		message := "连接正常"
		if !ok {
			message = mediaName + " 连接或认证失败，请检查地址、凭据和 TLS 证书"
		}
		data := serviceTestData{Kind: kind, ID: id, OK: ok, Duration: time.Since(started).Milliseconds(), Message: message}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
		return
	case "indexer":
		if id != "builtin" {
			writeAPIError(response, http.StatusBadRequest, 400, "unsupported indexer")
			return
		}
		result, handled, nativeErr := (subscriptionService{
			catalogPath: service.catalogPath, configStore: service.configStore,
			systemConfig: service.systemConfig, sites: service.sites,
		}).nativeIndexerOptions(ctx)
		if !handled {
			writeAPIError(response, http.StatusNotImplemented, 501, "plugin indexers are not migrated")
			return
		}
		err = nativeErr
		if err == nil && number(result["code"]) == 0 {
			message = "索引器已加载 " + text(len(slice(legacyPayload(result)["indexers"]))) + " 个来源"
		}
	default:
		writeAPIError(response, http.StatusBadRequest, 400, "unsupported service kind")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "legacy service is unavailable")
		return
	}
	if number(result["code"]) == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	ok := number(result["code"]) == 0 && result["success"] != false
	if message == "" {
		message = text(result["message"])
	}
	if message == "" {
		if ok {
			message = "连接正常"
		} else {
			message = "连接失败"
		}
	}
	data := serviceTestData{Kind: kind, ID: id, OK: ok, Duration: time.Since(started).Milliseconds(), Message: message}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service serviceOverviewService) downloaderDetail(ctx context.Context, id string) (map[string]any, map[string]any, error) {
	if service.downloaders != nil {
		numericID, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return nil, map[string]any{"code": 400, "success": false}, nil
		}
		payload, err := service.downloaderPayload(ctx, numericID)
		if errors.Is(err, downloaderconfig.ErrNotFound) {
			return nil, map[string]any{"code": 404, "success": false, "message": "downloader not found"}, nil
		}
		if err != nil {
			return nil, nil, err
		}
		detail, _ := payload["detail"].(map[string]any)
		return detail, nil, nil
	}
	return nil, nil, errors.New("native downloader store unavailable")
}

func normalizeDownloaders(payload map[string]any) []serviceComponent {
	items := make([]serviceComponent, 0)
	if payload == nil {
		return items
	}
	for _, entry := range entries(payload["detail"]) {
		item, ok := entry.value.(map[string]any)
		if !ok {
			continue
		}
		config, _ := item["config"].(map[string]any)
		dtype := text(item["type"])
		name := text(item["name"])
		if name == "" {
			name = serviceTypeLabel(dtype)
		}
		items = append(items, serviceComponent{
			ID: text(item["id"]), Kind: "downloader", Name: name, Type: dtype,
			Host:       safeServiceAddress(text(config["host"]), text(config["port"])),
			Configured: config != nil, Enabled: truthy(item["enabled"]), Active: truthy(item["enabled"]),
			Default: truthy(item["is_default"]), Monitoring: truthy(item["transfer"]), CanTest: config != nil,
		})
	}
	return items
}

func normalizeMediaServers(config, statistics map[string]any) []serviceComponent {
	items := make([]serviceComponent, 0, 3)
	active := "emby"
	if media, ok := config["media"].(map[string]any); ok && text(media["media_server"]) != "" {
		active = text(media["media_server"])
	}
	mediaSummary := ""
	if statistics != nil {
		parts := make([]string, 0, 3)
		for _, item := range []struct{ key, label string }{{"Movie", "电影"}, {"Series", "剧集"}, {"Episodes", "集"}} {
			if value := text(statistics[item.key]); value != "" {
				parts = append(parts, value+" "+item.label)
			}
		}
		mediaSummary = strings.Join(parts, " · ")
	}
	for _, item := range []struct{ id, name string }{{"emby", "Emby"}, {"jellyfin", "Jellyfin"}, {"plex", "Plex"}} {
		conf, _ := config[item.id].(map[string]any)
		host := safeServiceAddress(text(conf["host"]), "")
		isActive := item.id == active
		summary := ""
		if isActive {
			summary = mediaSummary
		}
		items = append(items, serviceComponent{ID: item.id, Kind: "media", Name: item.name, Type: item.id, Host: host, Summary: summary, Configured: host != "", Enabled: isActive, Active: isActive, CanTest: host != ""})
	}
	return items
}

func normalizeIndexer(config, payload map[string]any) []serviceComponent {
	items := slice(payload["indexers"])
	public := 0
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok && truthy(item["public"]) {
			public++
		}
	}
	active := "builtin"
	if pt, ok := config["pt"].(map[string]any); ok && text(pt["search_indexer"]) != "" {
		active = text(pt["search_indexer"])
	}
	summary := text(len(items)-public) + " 个私有来源 · " + text(public) + " 个公开来源"
	return []serviceComponent{{ID: "builtin", Kind: "indexer", Name: "内建索引器", Type: "builtin", Summary: summary, Configured: len(items) > 0, Enabled: active == "builtin", Active: active == "builtin", CanTest: true, SourceCount: len(items)}}
}

func safeServiceAddress(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	candidate := host
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	if parsed.Port() != "" {
		return parsed.Host
	}
	if port != "" {
		return net.JoinHostPort(parsed.Hostname(), port)
	}
	return parsed.Hostname()
}

func serviceTypeLabel(value string) string {
	return map[string]string{"qbittorrent": "qBittorrent", "transmission": "Transmission", "aria2": "Aria2"}[value]
}

func validServiceID(raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	if id == "" || len(id) > 64 {
		return "", false
	}
	for _, char := range id {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return "", false
		}
	}
	return id, true
}
