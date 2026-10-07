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

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type siteDetail struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Priority            int    `json:"priority"`
	SiteURL             string `json:"siteUrl"`
	RSSEnabled          bool   `json:"rssEnabled"`
	BrushEnabled        bool   `json:"brushEnabled"`
	StatisticEnabled    bool   `json:"statisticEnabled"`
	ParseEnabled        bool   `json:"parseEnabled"`
	MessageEnabled      bool   `json:"messageEnabled"`
	BrowserEnabled      bool   `json:"browserEnabled"`
	ProxyEnabled        bool   `json:"proxyEnabled"`
	SubtitleEnabled     bool   `json:"subtitleEnabled"`
	Tags                string `json:"tags"`
	FilterRule          string `json:"filterRule"`
	DownloadSetting     string `json:"downloadSetting"`
	LimitInterval       string `json:"limitInterval"`
	LimitCount          string `json:"limitCount"`
	LimitSeconds        string `json:"limitSeconds"`
	RSSConfigured       bool   `json:"rssConfigured"`
	CookieConfigured    bool   `json:"cookieConfigured"`
	APIKeyConfigured    bool   `json:"apiKeyConfigured"`
	UserAgentConfigured bool   `json:"userAgentConfigured"`
}

type siteUpsertRequest struct {
	Name             string `json:"name"`
	Priority         int    `json:"priority"`
	SiteURL          string `json:"siteUrl"`
	RSSEnabled       bool   `json:"rssEnabled"`
	BrushEnabled     bool   `json:"brushEnabled"`
	StatisticEnabled bool   `json:"statisticEnabled"`
	ParseEnabled     bool   `json:"parseEnabled"`
	MessageEnabled   bool   `json:"messageEnabled"`
	BrowserEnabled   bool   `json:"browserEnabled"`
	ProxyEnabled     bool   `json:"proxyEnabled"`
	SubtitleEnabled  bool   `json:"subtitleEnabled"`
	Tags             string `json:"tags"`
	FilterRule       string `json:"filterRule"`
	DownloadSetting  string `json:"downloadSetting"`
	LimitInterval    string `json:"limitInterval"`
	LimitCount       string `json:"limitCount"`
	LimitSeconds     string `json:"limitSeconds"`
	RSSURL           string `json:"rssUrl"`
	Cookie           string `json:"cookie"`
	APIKey           string `json:"apiKey"`
	UserAgent        string `json:"userAgent"`
	ClearRSSURL      bool   `json:"clearRssUrl"`
	ClearCookie      bool   `json:"clearCookie"`
	ClearAPIKey      bool   `json:"clearApiKey"`
	ClearUserAgent   bool   `json:"clearUserAgent"`
}

func (service siteService) serveDetail(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	id, ok := validSiteID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	item, status, err := service.siteRecord(ctx, id)
	if err != nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
		return
	}
	if status == 401 {
		writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
		return
	}
	if status != 0 || text(item["id"]) == "" {
		writeAPIError(response, http.StatusNotFound, 404, "site not found")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": safeSiteDetail(item)})
}

func (service siteService) create(response http.ResponseWriter, request *http.Request) {
	service.upsert(response, request, "")
}

func (service siteService) update(response http.ResponseWriter, request *http.Request) {
	id, ok := validSiteID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}
	service.upsert(response, request, id)
}

func (service siteService) delete(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	id, ok := validSiteID(request.PathValue("id"))
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 15*time.Second)
	defer cancel()
	if service.store != nil {
		numericID, _ := strconv.ParseInt(id, 10, 64)
		if err := service.store.Delete(ctx, numericID); err != nil {
			writeNativeSiteError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "站点已删除"})
		return
	}
	writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
}

func (service siteService) upsert(response http.ResponseWriter, request *http.Request, id string) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	var input siteUpsertRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid site request")
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.SiteURL = strings.TrimSpace(input.SiteURL)
	input.RSSURL = strings.TrimSpace(input.RSSURL)
	if message := validateSiteInput(input, id == ""); message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	current := map[string]any{}
	if id != "" {
		var status int
		var err error
		current, status, err = service.siteRecord(ctx, id)
		if err != nil {
			writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
			return
		}
		if status == 401 {
			writeAPIError(response, http.StatusUnauthorized, 401, "authorization token is invalid or expired")
			return
		}
		if status != 0 || text(current["id"]) == "" {
			writeAPIError(response, http.StatusNotFound, 404, "site not found")
			return
		}
	}

	form, message := buildSiteForm(id, input, current)
	if message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	if service.store != nil {
		if err := service.saveNativeSite(ctx, form); err != nil {
			writeNativeSiteError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": "站点已保存"})
		return
	}
	writeAPIError(response, http.StatusServiceUnavailable, 503, "native site store is unavailable")
}

func (service siteService) siteRecord(ctx context.Context, id string) (map[string]any, int, error) {
	if service.store != nil {
		numericID, _ := strconv.ParseInt(id, 10, 64)
		item, err := service.store.Get(ctx, numericID)
		if errors.Is(err, siteconfig.ErrNotFound) {
			return nil, 404, nil
		}
		if err != nil {
			return nil, 0, err
		}
		record, err := nativeSiteRecord(item)
		return record, 0, err
	}
	return nil, 0, errors.New("native site store unavailable")
}

func safeSiteDetail(item map[string]any) siteDetail {
	return siteDetail{
		ID: text(item["id"]), Name: text(item["name"]), Priority: int(number(item["pri"])),
		SiteURL: safeSiteOrigin(text(item["signurl"])), RSSEnabled: truthy(item["rss_enable"]),
		BrushEnabled: truthy(item["brush_enable"]), StatisticEnabled: truthy(item["statistic_enable"]),
		ParseEnabled: truthy(item["parse"]), MessageEnabled: truthy(item["unread_msg_notify"]),
		BrowserEnabled: truthy(item["chrome"]), ProxyEnabled: truthy(item["proxy"]), SubtitleEnabled: truthy(item["subtitle"]),
		Tags: text(item["tags"]), FilterRule: text(item["rule"]), DownloadSetting: text(item["download_setting"]),
		LimitInterval: text(item["limit_interval"]), LimitCount: text(item["limit_count"]), LimitSeconds: text(item["limit_seconds"]),
		RSSConfigured: text(item["rssurl"]) != "", CookieConfigured: text(item["cookie"]) != "",
		APIKeyConfigured: text(item["api_key"]) != "", UserAgentConfigured: text(item["ua"]) != "",
	}
}

func buildSiteForm(id string, input siteUpsertRequest, current map[string]any) (url.Values, string) {
	siteURL := input.SiteURL
	if id != "" && (siteURL == "" || siteURL == safeSiteOrigin(text(current["signurl"]))) {
		siteURL = text(current["signurl"])
	}
	rssURL := mergeSecret(text(current["rssurl"]), input.RSSURL, input.ClearRSSURL)
	if (input.RSSEnabled || input.BrushEnabled) && rssURL == "" {
		return nil, "RSS address is required for subscription or brushing"
	}
	cookie := mergeSecret(text(current["cookie"]), input.Cookie, input.ClearCookie)
	apiKey := mergeSecret(text(current["api_key"]), input.APIKey, input.ClearAPIKey)
	userAgent := mergeSecret(text(current["ua"]), input.UserAgent, input.ClearUserAgent)

	note := map[string]any{
		"rule": input.FilterRule, "download_setting": input.DownloadSetting, "parse": yesNo(input.ParseEnabled),
		"ua": userAgent, "chrome": yesNo(input.BrowserEnabled), "proxy": yesNo(input.ProxyEnabled),
		"message": yesNo(input.MessageEnabled), "subtitle": yesNo(input.SubtitleEnabled), "tags": input.Tags,
		"limit_interval": input.LimitInterval, "limit_count": input.LimitCount, "limit_seconds": input.LimitSeconds,
	}
	noteJSON, _ := json.Marshal(note)
	uses := ""
	if input.RSSEnabled {
		uses += "D"
	}
	if input.BrushEnabled {
		uses += "S"
	}
	if input.StatisticEnabled {
		uses += "T"
	}
	form := url.Values{
		"site_name": {input.Name}, "site_pri": {strconv.Itoa(input.Priority)}, "site_signurl": {siteURL},
		"site_rssurl": {rssURL}, "site_cookie": {cookie}, "site_api_key": {apiKey},
		"site_include": {uses}, "site_note": {string(noteJSON)},
	}
	if id != "" {
		form.Set("site_id", id)
	}
	return form, ""
}

func validateSiteInput(input siteUpsertRequest, creating bool) string {
	if input.Name == "" || len([]rune(input.Name)) > 80 {
		return "site name is required"
	}
	if input.Priority < 1 || input.Priority > 50 {
		return "site priority must be between 1 and 50"
	}
	if creating && !validSiteURL(input.SiteURL) {
		return "valid site address is required"
	}
	if input.SiteURL != "" && !validSiteURL(input.SiteURL) {
		return "invalid site address"
	}
	if input.RSSURL != "" && !validSiteURL(input.RSSURL) {
		return "invalid RSS address"
	}
	if _, err := parseSiteRequestPolicy(map[string]any{"limit_interval": input.LimitInterval, "limit_count": input.LimitCount, "limit_seconds": input.LimitSeconds}); err != nil {
		return "rate limits require bounded non-negative integers and a paired interval/count"
	}
	return ""
}

func validSiteID(raw string) (string, bool) {
	id := strings.TrimSpace(raw)
	parsed, err := strconv.Atoi(id)
	return id, err == nil && parsed > 0
}

func validSiteURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func safeSiteOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

func mergeSecret(current, replacement string, clear bool) string {
	if clear {
		return ""
	}
	if replacement != "" {
		return replacement
	}
	return current
}

func yesNo(value bool) string {
	if value {
		return "Y"
	}
	return "N"
}
