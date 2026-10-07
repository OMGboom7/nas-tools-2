package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (service subscriptionService) serveCompatSubscriptionUpsert(response http.ResponseWriter, request *http.Request) {
	_, ok := requireServiceToken(response, request)
	if !ok {
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 32<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription form")
		return
	}
	form := request.PostForm
	if form == nil {
		form = request.Form
	}
	kind := strings.ToUpper(strings.TrimSpace(form.Get("type")))
	if kind == "电影" {
		kind = "MOV"
	} else if kind != "MOV" && kind != "" {
		kind = "TV"
	}
	if kind == "" {
		writeAPIError(response, http.StatusBadRequest, 400, "subscription type is required")
		return
	}
	fuzzy, err := parseCompatSubscriptionFlag(form.Get("fuzzy_match"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid fuzzy matching flag")
		return
	}
	overEdition, err := parseCompatSubscriptionFlag(form.Get("over_edition"))
	if err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid edition flag")
		return
	}
	input := subscriptionUpsertRequest{
		ID: form.Get("rssid"), Name: form.Get("name"), Year: form.Get("year"), Type: kind,
		Season: form.Get("season"), MediaID: form.Get("mediaid"), Keyword: form.Get("keyword"),
		FuzzyMatch: fuzzy, OverEdition: overEdition,
		RSSSites: splitCompatSubscriptionSites(form.Get("rss_sites")), SearchSites: splitCompatSubscriptionSites(form.Get("search_sites")),
		Quality: form.Get("filter_restype"), Resolution: form.Get("filter_pix"), ReleaseGroup: form.Get("filter_team"),
		FilterRule: form.Get("filter_rule"), Include: form.Get("filter_include"), Exclude: form.Get("filter_exclude"),
		SavePath: form.Get("save_path"), DownloadSetting: form.Get("download_setting"),
	}
	if strings.HasPrefix(strings.TrimSpace(input.MediaID), "BG:") {
		input.Type = "TV"
	}
	if input.TotalEpisodes, err = parseCompatSubscriptionNumber(form.Get("total_ep")); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid episode total")
		return
	}
	if input.CurrentEpisode, err = parseCompatSubscriptionNumber(form.Get("current_ep")); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid current episode")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	if form.Get("in_form") != "manual" {
		if err := service.applyCompatSubscriptionDefaults(ctx, &input); err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "default subscription settings are unavailable")
			return
		}
	}
	if message := validateSubscription(&input); message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	var metadata *nativeSubscriptionMetadata
	if !input.FuzzyMatch {
		if input.MediaID == "" || strings.HasPrefix(input.MediaID, "BG:") || strings.HasPrefix(input.MediaID, "DB:") {
			input, err = service.resolveNativeSubscriptionInput(ctx, input)
			if err != nil {
				if errors.Is(err, errInvalidSubscriptionSelector) {
					writeAPIError(response, http.StatusBadRequest, 400, "invalid media ID")
					return
				}
				writeAPIError(response, http.StatusBadGateway, 502, "TMDB subscription search is unavailable")
				return
			}
			if input.MediaID == "" {
				writeAPIError(response, http.StatusNotFound, 404, "matching media was not found")
				return
			}
		}
		metadata, err = service.fetchNativeSubscriptionMetadata(ctx, input)
		if err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "TMDB subscription metadata is unavailable")
			return
		}
	}
	id, err := service.upsertNativeSubscription(ctx, input, metadata)
	if err != nil {
		switch {
		case errors.Is(err, errSubscriptionAlreadyExists):
			writeJSON(response, http.StatusConflict, map[string]any{"code": 9, "success": false, "msg": "订阅已存在"})
		case errors.Is(err, sql.ErrNoRows):
			writeAPIError(response, http.StatusNotFound, 404, "subscription not found")
		case errors.Is(err, errInvalidSubscriptionSelector):
			writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription ID")
		default:
			writeAPIError(response, http.StatusBadGateway, 502, "subscription storage is unavailable")
		}
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "msg": "添加订阅成功", "page": form.Get("page"), "name": input.Name, "rssid": id})
}

func parseCompatSubscriptionFlag(value string) (bool, error) {
	if value == "" || value == "0" {
		return false, nil
	}
	if value == "1" {
		return true, nil
	}
	return false, errInvalidSubscriptionSelector
}

func parseCompatSubscriptionNumber(value string) (*int, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	return &number, nil
}

func splitCompatSubscriptionSites(value string) []string {
	result := []string{}
	for _, site := range strings.Split(value, ",") {
		if site = strings.TrimSpace(site); site != "" {
			result = append(result, site)
		}
	}
	return result
}

func (service subscriptionService) applyCompatSubscriptionDefaults(ctx context.Context, input *subscriptionUpsertRequest) error {
	if service.systemConfig == nil {
		return errors.New("system settings are unavailable")
	}
	key := "DefaultRssSettingMOV"
	if input.Type == "TV" {
		key = "DefaultRssSettingTV"
	}
	stored, err := service.systemConfig.Get(ctx, key)
	if err != nil || stored == "" {
		return err
	}
	var defaults map[string]any
	if err := json.Unmarshal([]byte(stored), &defaults); err != nil {
		return err
	}
	if input.Quality == "" {
		input.Quality = text(defaults["restype"])
	}
	if input.Resolution == "" {
		input.Resolution = text(defaults["pix"])
	}
	if input.ReleaseGroup == "" {
		input.ReleaseGroup = text(defaults["team"])
	}
	if input.FilterRule == "" {
		input.FilterRule = text(defaults["rule"])
	}
	if input.Include == "" {
		input.Include = text(defaults["include"])
	}
	if input.Exclude == "" {
		input.Exclude = text(defaults["exclude"])
	}
	if input.DownloadSetting == "" {
		input.DownloadSetting = text(defaults["download_setting"])
	}
	if !input.OverEdition {
		input.OverEdition = text(defaults["over_edition"]) == "1"
	}
	if len(input.RSSSites) == 0 {
		input.RSSSites = stringsOf(defaults["rss_sites"])
	}
	if len(input.SearchSites) == 0 {
		input.SearchSites = stringsOf(defaults["search_sites"])
	}
	return nil
}
