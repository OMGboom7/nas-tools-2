package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

type downloaderConfigDetail struct {
	ID                    string              `json:"id"`
	Name                  string              `json:"name"`
	Type                  string              `json:"type"`
	Host                  string              `json:"host"`
	Port                  string              `json:"port"`
	Proxy                 string              `json:"proxy"`
	TorrentManagement     string              `json:"torrentManagement"`
	RmtMode               string              `json:"rmtMode"`
	Enabled               bool                `json:"enabled"`
	Transfer              bool                `json:"transfer"`
	OnlyNastool           bool                `json:"onlyNastool"`
	MatchPath             bool                `json:"matchPath"`
	Default               bool                `json:"default"`
	UsernameConfigured    bool                `json:"usernameConfigured"`
	PasswordConfigured    bool                `json:"passwordConfigured"`
	SecretConfigured      bool                `json:"secretConfigured"`
	CookieConfigured      bool                `json:"cookieConfigured"`
	DirectoriesConfigured bool                `json:"directoriesConfigured"`
	Directories           []downloadDirectory `json:"directories"`
}

type downloadDirectory struct {
	Type          string `json:"type"`
	Category      string `json:"category"`
	SavePath      string `json:"savePath"`
	ContainerPath string `json:"containerPath"`
	Label         string `json:"label"`
}

type downloaderOptions struct {
	Categories map[string][]string `json:"categories"`
	Warnings   []string            `json:"warnings"`
}

type downloaderConfigRequest struct {
	Name              string              `json:"name"`
	Type              string              `json:"type"`
	Host              string              `json:"host"`
	Port              string              `json:"port"`
	Username          string              `json:"username"`
	Password          string              `json:"password"`
	Secret            string              `json:"secret"`
	Cookie            string              `json:"cookie"`
	Proxy             string              `json:"proxy"`
	TorrentManagement string              `json:"torrentManagement"`
	RmtMode           string              `json:"rmtMode"`
	Enabled           bool                `json:"enabled"`
	Transfer          bool                `json:"transfer"`
	OnlyNastool       bool                `json:"onlyNastool"`
	MatchPath         bool                `json:"matchPath"`
	ClearUsername     bool                `json:"clearUsername"`
	ClearPassword     bool                `json:"clearPassword"`
	ClearSecret       bool                `json:"clearSecret"`
	ClearCookie       bool                `json:"clearCookie"`
	Directories       []downloadDirectory `json:"directories"`
}

type mediaConfigDetail struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Host               string `json:"host"`
	PlayHost           string `json:"playHost"`
	ServerName         string `json:"serverName"`
	Active             bool   `json:"active"`
	APIKeyConfigured   bool   `json:"apiKeyConfigured"`
	TokenConfigured    bool   `json:"tokenConfigured"`
	UsernameConfigured bool   `json:"usernameConfigured"`
	PasswordConfigured bool   `json:"passwordConfigured"`
}

type mediaConfigRequest struct {
	Host          string `json:"host"`
	PlayHost      string `json:"playHost"`
	ServerName    string `json:"serverName"`
	APIKey        string `json:"apiKey"`
	Token         string `json:"token"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	Activate      bool   `json:"activate"`
	ClearAPIKey   bool   `json:"clearApiKey"`
	ClearToken    bool   `json:"clearToken"`
	ClearUsername bool   `json:"clearUsername"`
	ClearPassword bool   `json:"clearPassword"`
}

func (service serviceOverviewService) serveDownloaderConfig(response http.ResponseWriter, request *http.Request) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	detail, result, err := service.downloaderDetail(ctx, id)
	if !service.handleConfigResult(response, result, err, "downloader not found") {
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": safeDownloaderConfig(detail)})
}

func (service serviceOverviewService) createDownloader(response http.ResponseWriter, request *http.Request) {
	service.upsertDownloader(response, request, "")
}

func (service serviceOverviewService) serveDownloaderOptions(response http.ResponseWriter, request *http.Request) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	data := downloaderOptions{Categories: map[string][]string{"电影": {}, "电视剧": {}, "动漫": {}}, Warnings: []string{}}
	if service.downloaders == nil {
		data.Warnings = append(data.Warnings, "download directory categories unavailable")
	} else {
		items, err := service.downloaders.List(ctx)
		if err != nil {
			writeAPIError(response, http.StatusInternalServerError, 500, "download directory categories could not be loaded")
			return
		}
		for _, item := range items {
			for _, directory := range normalizeDownloadDirectories(sliceOrJSON(item.DownloadDir)) {
				if _, exists := data.Categories[directory.Type]; exists && directory.Category != "" {
					data.Categories[directory.Type] = append(data.Categories[directory.Type], directory.Category)
				}
			}
		}
		for mediaType, categories := range data.Categories {
			data.Categories[mediaType] = uniqueStrings(categories)
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service serviceOverviewService) updateDownloader(response http.ResponseWriter, request *http.Request) {
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader id")
		return
	}
	service.upsertDownloader(response, request, id)
}

func (service serviceOverviewService) upsertDownloader(response http.ResponseWriter, request *http.Request, id string) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	var input downloaderConfigRequest
	if !decodeServiceRequest(response, request, &input, "invalid downloader request") {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Type = strings.ToLower(strings.TrimSpace(input.Type))
	input.Host = strings.TrimSpace(input.Host)
	input.Port = strings.TrimSpace(input.Port)
	input.Username = strings.TrimSpace(input.Username)
	input.Proxy = strings.TrimSpace(input.Proxy)
	input.RmtMode = strings.TrimSpace(input.RmtMode)
	if input.RmtMode == "" {
		input.RmtMode = "link"
	}

	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	current := map[string]any{}
	if id != "" {
		var result map[string]any
		var err error
		current, result, err = service.downloaderDetail(ctx, id)
		if !service.handleConfigResult(response, result, err, "downloader not found") {
			return
		}
	}
	config, message := buildDownloaderConfig(input, current)
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	configJSON, _ := json.Marshal(config)
	directories := "[]"
	if input.Directories == nil && id != "" {
		directories = rawJSONValue(current["download_dir"], "[]")
	} else {
		normalized, message := validateDownloadDirectories(input.Directories, input.Type)
		if message != "" {
			writeAPIError(response, http.StatusBadRequest, 400, message)
			return
		}
		encoded, _ := json.Marshal(normalized)
		directories = string(encoded)
	}
	if service.downloaders == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native downloader store is unavailable")
		return
	}
	numericID := int64(0)
	if id != "" {
		numericID, _ = strconv.ParseInt(id, 10, 64)
	}
	_, err := service.downloaders.Upsert(ctx, downloaderconfig.Downloader{
		ID: numericID, Name: input.Name, Type: input.Type,
		Enabled: boolInt(input.Enabled), Transfer: boolInt(input.Transfer),
		OnlyNastool: boolInt(input.OnlyNastool), MatchPath: boolInt(input.MatchPath),
		RmtMode: input.RmtMode, Config: string(configJSON), DownloadDir: directories,
	})
	if err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "downloader could not be saved")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "下载器已保存"})
}

func (service serviceOverviewService) deleteDownloader(response http.ResponseWriter, request *http.Request) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if service.downloaders == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native downloader store is unavailable")
		return
	}
	numericID, _ := strconv.ParseInt(id, 10, 64)
	if err := service.downloaders.Delete(ctx, numericID); err != nil {
		writeAPIError(response, http.StatusNotFound, 404, "downloader could not be deleted")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "下载器已删除"})
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (service serviceOverviewService) setDefaultDownloader(response http.ResponseWriter, request *http.Request) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, ok := validServiceID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if service.systemConfig == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native system configuration is unavailable")
		return
	}
	if err := service.systemConfig.Set(ctx, "DefaultDownloader", id); err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "default downloader could not be updated")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "默认下载器已更新"})
}

func (service serviceOverviewService) serveMediaConfig(response http.ResponseWriter, request *http.Request) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, name, ok := validMediaType(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "unsupported media server")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	config, status, err := service.configInfo(ctx)
	if err != nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native configuration store is unavailable")
		return
	}
	if status == 401 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if status != 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "configuration could not be loaded")
		return
	}
	server, _ := config[id].(map[string]any)
	active := "emby"
	if media, ok := config["media"].(map[string]any); ok && text(media["media_server"]) != "" {
		active = strings.ToLower(text(media["media_server"]))
	}
	detail := safeMediaConfig(id, name, active == id, server)
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": detail})
}

func (service serviceOverviewService) updateMediaConfig(response http.ResponseWriter, request *http.Request) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	id, _, ok := validMediaType(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "unsupported media server")
		return
	}
	var input mediaConfigRequest
	if !decodeServiceRequest(response, request, &input, "invalid media server request") {
		return
	}
	input.Host = strings.TrimSpace(input.Host)
	input.PlayHost = strings.TrimSpace(input.PlayHost)
	input.ServerName = strings.TrimSpace(input.ServerName)
	input.Username = strings.TrimSpace(input.Username)
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	config, status, err := service.configInfo(ctx)
	if err != nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native configuration store is unavailable")
		return
	}
	if status == 401 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if status != 0 {
		writeAPIError(response, http.StatusBadGateway, 502, "configuration could not be loaded")
		return
	}
	current, _ := config[id].(map[string]any)
	items, message := buildMediaConfig(id, input, current)
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	if input.Activate {
		items["media.media_server"] = id
	}
	if service.configStore == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native configuration store is unavailable")
		return
	}
	if err := service.configStore.Update(items); err != nil {
		writeAPIError(response, http.StatusInternalServerError, 500, "media server could not be saved")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "媒体服务器已保存"})
}

func safeDownloaderConfig(item map[string]any) downloaderConfigDetail {
	config := objectValue(item["config"])
	directories := normalizeDownloadDirectories(sliceOrJSON(item["download_dir"]))
	return downloaderConfigDetail{
		ID: text(item["id"]), Name: text(item["name"]), Type: strings.ToLower(text(item["type"])),
		Host: safeEditableURL(text(config["host"])), Port: text(config["port"]), Proxy: safeProxyAddress(text(config["proxy"])),
		TorrentManagement: text(config["torrent_management"]), RmtMode: text(item["rmt_mode"]),
		Enabled: truthy(item["enabled"]), Transfer: truthy(item["transfer"]), OnlyNastool: truthy(item["only_nastool"]),
		MatchPath: truthy(item["match_path"]), Default: truthy(item["is_default"]),
		UsernameConfigured: text(config["username"]) != "", PasswordConfigured: text(config["password"]) != "",
		SecretConfigured: text(config["secret"]) != "", CookieConfigured: text(config["cookie"]) != "",
		DirectoriesConfigured: len(directories) > 0, Directories: directories,
	}
}

func safeMediaConfig(id, name string, active bool, config map[string]any) mediaConfigDetail {
	return mediaConfigDetail{
		ID: id, Name: name, Host: safeEditableURL(text(config["host"])), PlayHost: safeEditableURL(text(config["play_host"])),
		ServerName: text(config["servername"]), Active: active, APIKeyConfigured: text(config["api_key"]) != "",
		TokenConfigured: text(config["token"]) != "", UsernameConfigured: text(config["username"]) != "",
		PasswordConfigured: text(config["password"]) != "",
	}
}

func buildDownloaderConfig(input downloaderConfigRequest, current map[string]any) (map[string]any, string) {
	if input.Name == "" || len([]rune(input.Name)) > 80 {
		return nil, "downloader name is required"
	}
	allowed := map[string]bool{"qbittorrent": true, "transmission": true, "aria2": true, "pan115": true, "pikpak": true}
	if !allowed[input.Type] {
		return nil, "unsupported downloader type"
	}
	if !map[string]bool{"copy": true, "link": true, "softlink": true, "move": true, "rclone": true, "rclonecopy": true, "minio": true, "miniocopy": true}[input.RmtMode] {
		return nil, "unsupported transfer mode"
	}
	currentConfig := map[string]any{}
	if strings.ToLower(text(current["type"])) == input.Type {
		currentConfig = objectValue(current["config"])
	}
	config := map[string]any{}
	username := mergeSecret(text(currentConfig["username"]), input.Username, input.ClearUsername)
	password := mergeSecret(text(currentConfig["password"]), input.Password, input.ClearPassword)
	secret := mergeSecret(text(currentConfig["secret"]), input.Secret, input.ClearSecret)
	cookie := mergeSecret(text(currentConfig["cookie"]), input.Cookie, input.ClearCookie)
	switch input.Type {
	case "qbittorrent", "transmission", "aria2":
		if !validDownloaderHost(input.Host) {
			return nil, "valid downloader address is required"
		}
		port, err := strconv.Atoi(input.Port)
		if err != nil || port < 1 || port > 65535 {
			return nil, "downloader port must be between 1 and 65535"
		}
		config["host"] = preserveSafeURL(text(currentConfig["host"]), input.Host)
		config["port"] = input.Port
	}
	switch input.Type {
	case "qbittorrent":
		if username == "" {
			return nil, "downloader username is required"
		}
		management := input.TorrentManagement
		if management == "" {
			management = text(currentConfig["torrent_management"])
		}
		if management == "" {
			management = "manual"
		}
		if !map[string]bool{"default": true, "manual": true, "auto": true}[management] {
			return nil, "unsupported torrent management mode"
		}
		config["username"], config["password"], config["torrent_management"] = username, password, management
	case "transmission":
		if username == "" {
			return nil, "downloader username is required"
		}
		config["username"], config["password"] = username, password
	case "aria2":
		if secret == "" {
			return nil, "Aria2 token is required"
		}
		config["secret"] = secret
	case "pan115":
		if cookie == "" {
			return nil, "115 cookie is required"
		}
		config["cookie"] = cookie
	case "pikpak":
		if username == "" || password == "" {
			return nil, "PikPak account and password are required"
		}
		config["username"], config["password"], config["proxy"] = username, password, input.Proxy
	}
	return config, ""
}

func buildMediaConfig(id string, input mediaConfigRequest, current map[string]any) (map[string]any, string) {
	if !validHTTPURL(input.Host) {
		return nil, "valid media server address is required"
	}
	if input.PlayHost != "" && !validHTTPURL(input.PlayHost) {
		return nil, "invalid media playback address"
	}
	host := preserveSafeURL(text(current["host"]), input.Host)
	playHost := preserveSafeURL(text(current["play_host"]), input.PlayHost)
	items := map[string]any{id + ".host": host, id + ".play_host": playHost}
	switch id {
	case "emby", "jellyfin":
		apiKey := mergeSecret(text(current["api_key"]), input.APIKey, input.ClearAPIKey)
		if apiKey == "" {
			return nil, "media server API key is required"
		}
		items[id+".api_key"] = apiKey
	case "plex":
		token := mergeSecret(text(current["token"]), input.Token, input.ClearToken)
		username := mergeSecret(text(current["username"]), input.Username, input.ClearUsername)
		password := mergeSecret(text(current["password"]), input.Password, input.ClearPassword)
		if token == "" && (input.ServerName == "" || username == "" || password == "") {
			return nil, "Plex token or complete account credentials are required"
		}
		items["plex.token"], items["plex.servername"] = token, input.ServerName
		items["plex.username"], items["plex.password"] = username, password
	}
	return items, ""
}

func (service serviceOverviewService) configInfo(ctx context.Context) (map[string]any, int, error) {
	if service.configStore != nil {
		config, err := service.configStore.Snapshot()
		return config, 0, err
	}
	return nil, 0, errors.New("native configuration store unavailable")
}

func (service serviceOverviewService) handleConfigResult(response http.ResponseWriter, result map[string]any, err error, notFound string) bool {
	if err != nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native configuration store is unavailable")
		return false
	}
	if result != nil && number(result["code"]) == 403 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return false
	}
	if result != nil {
		writeAPIError(response, http.StatusNotFound, 404, notFound)
		return false
	}
	return true
}

func requireServiceToken(response http.ResponseWriter, request *http.Request) (string, bool) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return "", false
	}
	return token, true
}

func decodeServiceRequest(response http.ResponseWriter, request *http.Request, target any, message string) bool {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return false
	}
	return true
}

func validMediaType(raw string) (string, string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	name, ok := map[string]string{"emby": "Emby", "jellyfin": "Jellyfin", "plex": "Plex"}[id]
	return id, name, ok
}

func validDownloaderHost(raw string) bool {
	value := strings.TrimSpace(raw)
	if value == "" || strings.ContainsAny(value, "\r\n\t") {
		return false
	}
	if strings.Contains(value, "://") {
		return validHTTPURL(value)
	}
	parsed, err := url.Parse("http://" + value)
	return err == nil && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validHTTPURL(raw string) bool {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil
}

func safeEditableURL(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	withScheme := value
	if !strings.Contains(withScheme, "://") {
		withScheme = "http://" + withScheme
	}
	parsed, err := url.Parse(withScheme)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	parsed.User, parsed.RawQuery, parsed.Fragment, parsed.RawFragment = nil, "", "", ""
	result := parsed.String()
	if !strings.Contains(value, "://") {
		result = strings.TrimPrefix(result, "http://")
	}
	return strings.TrimRight(result, "/")
}

func safeProxyAddress(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.Contains(value, "@") {
		return ""
	}
	return safeEditableURL(value)
}

func preserveSafeURL(current, input string) string {
	if input != "" && safeEditableURL(current) == strings.TrimRight(input, "/") {
		return current
	}
	return input
}

func objectValue(value any) map[string]any {
	if item, ok := value.(map[string]any); ok {
		return item
	}
	if raw, ok := value.(string); ok {
		var item map[string]any
		if json.Unmarshal([]byte(raw), &item) == nil {
			return item
		}
	}
	return map[string]any{}
}

func sliceOrJSON(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	if raw, ok := value.(string); ok {
		var items []any
		if json.Unmarshal([]byte(raw), &items) == nil {
			return items
		}
	}
	return nil
}

func rawJSONValue(value any, fallback string) string {
	if raw, ok := value.(string); ok && json.Valid([]byte(raw)) {
		return raw
	}
	if value != nil {
		if encoded, err := json.Marshal(value); err == nil {
			return string(encoded)
		}
	}
	return fallback
}

func normalizeDownloadDirectories(items []any) []downloadDirectory {
	result := make([]downloadDirectory, 0, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		directory := downloadDirectory{
			Type: strings.TrimSpace(text(item["type"])), Category: strings.TrimSpace(text(item["category"])),
			SavePath: strings.TrimSpace(text(item["save_path"])), ContainerPath: strings.TrimSpace(text(item["container_path"])),
			Label: strings.TrimSpace(text(item["label"])),
		}
		if directory.Type != "" || directory.Category != "" || directory.SavePath != "" || directory.ContainerPath != "" || directory.Label != "" {
			result = append(result, directory)
		}
	}
	return result
}

func validateDownloadDirectories(items []downloadDirectory, downloaderType string) ([]map[string]string, string) {
	if len(items) > 50 {
		return nil, "too many download directories"
	}
	result := make([]map[string]string, 0, len(items))
	seenRules, seenLabels := map[string]bool{}, map[string]bool{}
	for _, raw := range items {
		item := downloadDirectory{
			Type: strings.TrimSpace(raw.Type), Category: strings.TrimSpace(raw.Category),
			SavePath: strings.TrimSpace(raw.SavePath), ContainerPath: strings.TrimSpace(raw.ContainerPath), Label: strings.TrimSpace(raw.Label),
		}
		if item.Type == "" && item.Category == "" && item.SavePath == "" && item.ContainerPath == "" && item.Label == "" {
			continue
		}
		if !map[string]bool{"": true, "电影": true, "电视剧": true, "动漫": true}[item.Type] {
			return nil, "unsupported download directory media type"
		}
		if item.Type == "" && item.Category != "" {
			return nil, "download directory category requires a media type"
		}
		if len([]rune(item.Category)) > 100 || len([]rune(item.Label)) > 100 || len(item.SavePath) > 2048 || len(item.ContainerPath) > 2048 {
			return nil, "download directory field is too long"
		}
		for _, value := range []string{item.Category, item.Label, item.SavePath, item.ContainerPath} {
			if strings.ContainsAny(value, "\r\n\x00") {
				return nil, "download directory contains invalid characters"
			}
		}
		ruleKey := item.Type + "\x00" + item.Category
		if seenRules[ruleKey] {
			return nil, "download directory rules must be unique"
		}
		seenRules[ruleKey] = true
		if downloaderType != "qbittorrent" {
			item.Label = ""
		} else if item.Label != "" {
			if seenLabels[item.Label] {
				return nil, "qBittorrent directory labels must be unique"
			}
			seenLabels[item.Label] = true
		}
		result = append(result, map[string]string{
			"type": item.Type, "category": item.Category, "save_path": item.SavePath,
			"container_path": item.ContainerPath, "label": item.Label,
		})
	}
	return result, ""
}

func uniqueStrings(items []string) []string {
	result := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, raw := range items {
		item := strings.TrimSpace(raw)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		result = append(result, item)
	}
	return result
}

func zeroOne(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
