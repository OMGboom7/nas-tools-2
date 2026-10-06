package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
)

type subscriptionService struct {
	legacyURL    string
	client       *http.Client
	images       *mediaImageProxy
	downloaders  *downloaderconfig.Store
	sites        *siteconfig.Store
	filters      *filterconfig.Store
	systemConfig *systemconfig.Store
	configStore  *config.Store
	catalogPath  string
	databasePath string
	words        *wordconfig.Store
}

type subscriptionsData struct {
	Items    []subscriptionItem    `json:"items"`
	History  []subscriptionHistory `json:"history"`
	Warnings []string              `json:"warnings"`
}

type subscriptionItem struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Year            string   `json:"year"`
	Type            string   `json:"type"`
	Season          string   `json:"season"`
	TMDBID          string   `json:"tmdbId"`
	Image           string   `json:"image"`
	Overview        string   `json:"overview"`
	State           string   `json:"state"`
	StateLabel      string   `json:"stateLabel"`
	Total           int      `json:"total"`
	Remaining       int      `json:"remaining"`
	Progress        float64  `json:"progress"`
	RSSSites        []string `json:"rssSites"`
	SearchSites     []string `json:"searchSites"`
	OverEdition     bool     `json:"overEdition"`
	Quality         string   `json:"quality"`
	Resolution      string   `json:"resolution"`
	ReleaseGroup    string   `json:"releaseGroup"`
	Include         string   `json:"include"`
	Exclude         string   `json:"exclude"`
	Keyword         string   `json:"keyword"`
	FuzzyMatch      bool     `json:"fuzzyMatch"`
	FilterRule      string   `json:"filterRule"`
	SavePath        string   `json:"savePath"`
	DownloadSetting string   `json:"downloadSetting"`
	TotalEpisodes   int      `json:"totalEpisodes"`
	CurrentEpisode  int      `json:"currentEpisode"`
}

type subscriptionHistory struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Year       string `json:"year"`
	Type       string `json:"type"`
	Season     string `json:"season"`
	TMDBID     string `json:"tmdbId"`
	Image      string `json:"image"`
	Overview   string `json:"overview"`
	FinishTime string `json:"finishTime"`
	Total      int    `json:"total"`
	Start      int    `json:"start"`
}

type subscriptionUpsertRequest struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Year            string   `json:"year"`
	Type            string   `json:"type"`
	Season          string   `json:"season"`
	MediaID         string   `json:"mediaId"`
	Keyword         string   `json:"keyword"`
	FuzzyMatch      bool     `json:"fuzzyMatch"`
	OverEdition     bool     `json:"overEdition"`
	RSSSites        []string `json:"rssSites"`
	SearchSites     []string `json:"searchSites"`
	Quality         string   `json:"quality"`
	Resolution      string   `json:"resolution"`
	ReleaseGroup    string   `json:"releaseGroup"`
	FilterRule      string   `json:"filterRule"`
	Include         string   `json:"include"`
	Exclude         string   `json:"exclude"`
	SavePath        string   `json:"savePath"`
	DownloadSetting string   `json:"downloadSetting"`
	TotalEpisodes   *int     `json:"totalEpisodes"`
	CurrentEpisode  *int     `json:"currentEpisode"`
}

func (service subscriptionService) serveList(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	if service.databasePath == "" {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native subscription storage is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 20*time.Second)
	defer cancel()
	data, err := service.nativeSubscriptionList(ctx)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "subscription service is unavailable")
		return
	}
	sort.SliceStable(data.Items, func(left, right int) bool { return data.Items[left].Name < data.Items[right].Name })
	sort.SliceStable(data.History, func(left, right int) bool { return data.History[left].FinishTime > data.History[right].FinishTime })
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": data})
}

func (service subscriptionService) upsert(response http.ResponseWriter, request *http.Request) {
	token := request.Header.Get("Authorization")
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	var input subscriptionUpsertRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription request")
		return
	}
	if strings.HasPrefix(strings.TrimSpace(input.MediaID), "BG:") {
		input.Type = "TV"
	}
	if message := validateSubscription(&input); message != "" {
		writeAPIError(response, http.StatusBadRequest, 400, message)
		return
	}
	if service.databasePath != "" && (input.FuzzyMatch || numericTMDBID(input.MediaID) || input.MediaID == "" || strings.HasPrefix(input.MediaID, "BG:") || strings.HasPrefix(input.MediaID, "DB:")) {
		service.serveNativeSubscriptionUpsert(response, request, input)
		return
	}
	form := url.Values{
		"name": {input.Name}, "type": {input.Type}, "fuzzy_match": {boolNumber(input.FuzzyMatch)},
		"over_edition": {boolNumber(input.OverEdition)}, "in_form": {"manual"},
	}
	optionalForm(form, "rssid", input.ID)
	optionalForm(form, "year", input.Year)
	optionalForm(form, "season", input.Season)
	optionalForm(form, "mediaid", input.MediaID)
	optionalForm(form, "keyword", input.Keyword)
	optionalForm(form, "rss_sites", strings.Join(input.RSSSites, ","))
	optionalForm(form, "search_sites", strings.Join(input.SearchSites, ","))
	optionalForm(form, "filter_restype", input.Quality)
	optionalForm(form, "filter_pix", input.Resolution)
	optionalForm(form, "filter_team", input.ReleaseGroup)
	optionalForm(form, "filter_rule", input.FilterRule)
	optionalForm(form, "filter_include", input.Include)
	optionalForm(form, "filter_exclude", input.Exclude)
	optionalForm(form, "save_path", input.SavePath)
	optionalForm(form, "download_setting", input.DownloadSetting)
	if input.TotalEpisodes != nil {
		form.Set("total_ep", strconv.Itoa(*input.TotalEpisodes))
	}
	if input.CurrentEpisode != nil {
		form.Set("current_ep", strconv.Itoa(*input.CurrentEpisode))
	}
	service.forwardMutation(response, request, token, "/api/v1/subscribe/add", form)
}

func validateSubscription(input *subscriptionUpsertRequest) string {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.Year = strings.TrimSpace(input.Year)
	input.Type = strings.ToUpper(strings.TrimSpace(input.Type))
	input.MediaID = strings.TrimSpace(input.MediaID)
	if id, err := strconv.ParseInt(input.MediaID, 10, 64); err == nil && id > 0 {
		input.MediaID = strconv.FormatInt(id, 10)
	}
	input.Keyword = strings.TrimSpace(input.Keyword)
	if input.Name == "" || len([]rune(input.Name)) > 200 {
		return "subscription name is required and must not exceed 200 characters"
	}
	if input.Type != "MOV" && input.Type != "TV" {
		return "subscription type must be MOV or TV"
	}
	if input.Year != "" {
		year, err := strconv.Atoi(input.Year)
		if err != nil || year < 1800 || year > 2200 {
			return "invalid release year"
		}
	}
	if input.Type == "TV" && input.Season != "" {
		season := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(input.Season)), "S")
		number, err := strconv.Atoi(season)
		if err != nil || number < 0 || number > 999 {
			return "invalid season"
		}
		input.Season = strconv.Itoa(number)
	} else if input.Type == "MOV" {
		input.Season = ""
	}
	if input.TotalEpisodes != nil && *input.TotalEpisodes < 0 || input.CurrentEpisode != nil && *input.CurrentEpisode < 0 {
		return "episode counts cannot be negative"
	}
	if input.TotalEpisodes != nil && input.CurrentEpisode != nil && *input.CurrentEpisode > *input.TotalEpisodes {
		return "current episode cannot exceed total episodes"
	}
	if len([]rune(input.Keyword)) > 200 || len([]rune(input.Include)) > 500 || len([]rune(input.Exclude)) > 500 {
		return "subscription filter text is too long"
	}
	return ""
}

func optionalForm(form url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		form.Set(key, value)
	}
}

func boolNumber(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func (service subscriptionService) control(response http.ResponseWriter, request *http.Request) {
	token, kind, id, action, ok := subscriptionAction(request)
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription action")
		return
	}
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	var path string
	form := url.Values{"type": {kind}, "rssid": {id}}
	switch action {
	case "refresh":
		path = "/api/v1/subscribe/search"
	case "remove":
		if service.databasePath != "" {
			service.serveNativeSubscriptionRemove(response, request, kind, id, false)
			return
		}
		path = "/api/v1/subscribe/delete"
	default:
		writeAPIError(response, http.StatusBadRequest, 400, "unsupported subscription action")
		return
	}
	service.forwardMutation(response, request, token, path, form)
}

func (service subscriptionService) controlHistory(response http.ResponseWriter, request *http.Request) {
	token, kind, id, action, ok := subscriptionAction(request)
	if !ok {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid subscription history action")
		return
	}
	if token == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	var path string
	form := url.Values{"rssid": {id}}
	switch action {
	case "redo":
		if service.databasePath != "" {
			service.serveNativeSubscriptionHistoryRedo(response, request, kind, id, false)
			return
		}
		path = "/api/v1/subscribe/redo"
		form.Set("type", kind)
	case "remove":
		if service.databasePath != "" {
			service.serveNativeSubscriptionRemove(response, request, kind, id, true)
			return
		}
		path = "/api/v1/subscribe/history/delete"
	default:
		writeAPIError(response, http.StatusBadRequest, 400, "unsupported subscription history action")
		return
	}
	service.forwardMutation(response, request, token, path, form)
}

func subscriptionAction(request *http.Request) (token, kind, id, action string, ok bool) {
	token = request.Header.Get("Authorization")
	kind = strings.ToUpper(request.PathValue("type"))
	id = strings.TrimSpace(request.PathValue("id"))
	action = request.PathValue("action")
	ok = (kind == "MOV" || kind == "TV") && id != "" && len(id) <= 64
	return
}

func (service subscriptionService) forwardMutation(response http.ResponseWriter, request *http.Request, token, path string, form url.Values) {
	ctx, cancel := context.WithTimeout(request.Context(), 90*time.Second)
	defer cancel()
	result, err := postLegacy(ctx, service.client, service.legacyURL, path, token, form)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "legacy subscription service is unavailable")
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
			message = "subscription action failed"
		}
		writeJSON(response, http.StatusBadRequest, map[string]any{"code": code, "success": false, "message": message})
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "message": text(result["message"])})
}

func (service subscriptionService) normalizeItems(value any, kind string) []subscriptionItem {
	items := make([]subscriptionItem, 0)
	for _, entry := range entries(value) {
		item, ok := entry.value.(map[string]any)
		if !ok {
			continue
		}
		total := int(number(item["total"]))
		remaining := int(number(item["lack"]))
		progress := float64(0)
		if total > 0 {
			progress = float64(total-remaining) * 100 / float64(total)
			if progress < 0 {
				progress = 0
			} else if progress > 100 {
				progress = 100
			}
		}
		image := text(item["image"])
		if image == "" {
			image = text(item["poster"])
		}
		if service.images != nil {
			image = service.images.rewrite(image)
		}
		id := text(item["id"])
		if id == "" {
			id = entry.key
		}
		state := text(item["state"])
		items = append(items, subscriptionItem{
			ID: id, Name: text(item["name"]), Year: text(item["year"]), Type: kind,
			Season: text(item["season"]), TMDBID: text(item["tmdbid"]), Image: image,
			Overview: text(item["overview"]), State: state, StateLabel: subscriptionStateLabel(state),
			Total: total, Remaining: remaining, Progress: progress, RSSSites: stringsOf(item["rss_sites"]),
			SearchSites: stringsOf(item["search_sites"]), OverEdition: truthy(item["over_edition"]),
			Quality: text(item["filter_restype"]), Resolution: text(item["filter_pix"]),
			ReleaseGroup: text(item["filter_team"]), Include: text(item["filter_include"]),
			Exclude: text(item["filter_exclude"]), Keyword: text(item["keyword"]), FuzzyMatch: truthy(item["fuzzy_match"]),
			FilterRule: text(item["filter_rule"]), SavePath: text(item["save_path"]),
			DownloadSetting: text(item["download_setting"]), TotalEpisodes: int(number(item["total_ep"])),
			CurrentEpisode: int(number(item["current_ep"])),
		})
	}
	return items
}

func (service subscriptionService) normalizeHistory(value any) []subscriptionHistory {
	items := make([]subscriptionHistory, 0)
	for _, raw := range slice(value) {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		image := text(item["IMAGE"])
		if service.images != nil {
			image = service.images.rewrite(image)
		}
		items = append(items, subscriptionHistory{
			ID: text(item["ID"]), Name: text(item["NAME"]), Year: text(item["YEAR"]),
			Type: text(item["TYPE"]), Season: text(item["SEASON"]), TMDBID: text(item["TMDBID"]),
			Image: image, Overview: text(item["DESC"]), FinishTime: text(item["FINISH_TIME"]),
			Total: int(number(item["TOTAL"])), Start: int(number(item["START"])),
		})
	}
	return items
}

func subscriptionStateLabel(state string) string {
	switch state {
	case "D":
		return "队列中"
	case "S":
		return "正在搜索"
	case "R":
		return "正在订阅"
	default:
		return "完成"
	}
}
