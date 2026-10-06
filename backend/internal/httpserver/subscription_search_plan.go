package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

type subscriptionSearchPlanner struct {
	runner *rssRunAPI
	search *nativeExternalResourceSearch
}

type subscriptionResourceProvider func(context.Context, map[string]any, string, string) ([]identifiedSubscriptionResource, *recognitionFailure)

type subscriptionSearchPlan struct {
	Candidates      []subscriptionCandidate `json:"candidates"`
	Remaining       []int                   `json:"remaining"`
	LibraryComplete bool                    `json:"libraryComplete"`
	Fetched         int                     `json:"fetched"`
	raw             map[string]any
	mediaID         string
	season, total   int
	storedMissing   []int
	needed          []int
}

func (api subscriptionSearchPlanner) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if api.runner == nil || api.search == nil || api.runner.download.service.auth == nil {
		writeAPIError(w, 503, 503, "native subscription planning is unavailable")
		return
	}
	auth := api.runner.download.service.auth
	claims, err := auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return
	}
	if !auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission is required")
		return
	}
	if !api.runner.pureGo {
		writeAPIError(w, 501, 501, "native subscription planning requires disabled legacy backend")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil {
		writeAPIError(w, 400, 400, "invalid subscription plan request")
		return
	}
	kind := r.Form.Get("type")
	id, err := strconv.ParseInt(r.Form.Get("id"), 10, 64)
	if err != nil || id <= 0 || kind != "MOV" && kind != "TV" {
		writeAPIError(w, 400, 400, "invalid subscription selector")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	plan, failure := api.plan(ctx, kind, id)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": plan})
}

func (api subscriptionSearchPlanner) plan(ctx context.Context, kind string, id int64) (subscriptionSearchPlan, *recognitionFailure) {
	return api.planSelected(ctx, kind, id, "", false)
}

func (api subscriptionSearchPlanner) planSelected(ctx context.Context, kind string, id int64, expectedState string, executing bool) (subscriptionSearchPlan, *recognitionFailure) {
	return api.planResources(ctx, kind, id, expectedState, executing, nil)
}

func (api subscriptionSearchPlanner) planResources(ctx context.Context, kind string, id int64, expectedState string, executing bool, provider subscriptionResourceProvider) (subscriptionSearchPlan, *recognitionFailure) {
	plan := subscriptionSearchPlan{Candidates: []subscriptionCandidate{}, Remaining: []int{}}
	fail := func(status int, message string) (subscriptionSearchPlan, *recognitionFailure) {
		return plan, &recognitionFailure{status, message}
	}
	service := api.runner.recognition.service
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return fail(503, "subscription storage is unavailable")
	}
	defer db.Close()
	table := "RSS_MOVIES"
	if kind == "TV" {
		table = "RSS_TVS"
	}
	raw, err := readNativeSubscriptionRow(ctx, db, table, id)
	if err != nil {
		return fail(503, "subscription configuration is unavailable")
	}
	if raw == nil {
		return fail(404, "subscription is unavailable")
	}
	item := normalizeNativeSubscriptionRow(raw)
	plan.raw = raw
	if expectedState != "" && text(raw["STATE"]) != expectedState {
		return fail(409, "subscription scheduled state changed")
	}
	if executing {
		pending, err := readPendingSubscriptionSubmission(ctx, db, kind, id)
		if err != nil {
			return fail(503, "subscription submission state is unavailable")
		}
		if pending {
			return fail(409, "subscription submission requires verification before retrying")
		}
	}
	if truthy(item["fuzzy_match"]) || truthy(item["over_edition"]) {
		return fail(501, "fuzzy/edition-upgrade subscription planning is not migrated")
	}
	input := subscriptionUpsertRequest{Name: text(item["name"]), Year: text(item["year"]), Type: kind, MediaID: text(item["tmdbid"]), Season: text(item["season"])}
	if kind == "TV" && input.Season == "" {
		input.Season = "1"
	}
	if validateSubscription(&input) != "" {
		return fail(422, "invalid subscription identity")
	}
	if !numericTMDBID(input.MediaID) {
		input, err = service.resolveNativeSubscriptionInput(ctx, input)
		if err != nil || input.MediaID == "" {
			return fail(502, "subscription identity could not be verified")
		}
	}
	mediaKind := "movie"
	if kind == "TV" {
		mediaKind = "tv"
	}
	detail, err := fetchNativeTMDBDetails(ctx, service.configStore, service.client.Transport, mediaKind, input.MediaID)
	if err != nil {
		return fail(502, "subscription metadata is unavailable")
	}
	meta := mediameta.Metadata{Title: detail.Title, Year: input.Year}
	plan.mediaID = input.MediaID
	selection := subscriptionCandidateInput{TV: kind == "TV", Quality: text(item["filter_restype"]), Resolution: text(item["filter_pix"]), Team: text(item["filter_team"]), Include: text(item["filter_include"]), Exclude: text(item["filter_exclude"])}
	if group := text(item["filter_rule"]); group != "" {
		selection.Group, err = strconv.ParseInt(group, 10, 64)
		if err != nil || selection.Group < -1 {
			return fail(422, "invalid subscription filter group")
		}
	}
	if kind == "TV" {
		selection.Season, _ = strconv.Atoi(input.Season)
		meta.Title, meta.Episodes.TV, meta.Episodes.Season = detail.Name, true, &selection.Season
		for _, season := range detail.Seasons {
			if season.Number == selection.Season && season.Episodes != nil {
				selection.Total = *season.Episodes
			}
		}
		if total := text(item["total_ep"]); total != "" && total != "0" {
			selection.Total, err = strconv.Atoi(total)
			if err != nil {
				return fail(422, "invalid subscription total override")
			}
		}
		for i := range detail.Seasons {
			if detail.Seasons[i].Number == selection.Season {
				detail.Seasons[i].Episodes = &selection.Total
			}
		}
		oldTotal, e1 := strconv.Atoi(text(raw["TOTAL"]))
		lack, e2 := strconv.Atoi(text(raw["LACK"]))
		if e1 != nil || e2 != nil || oldTotal < 0 || oldTotal > 10000 || lack < 0 || lack > oldTotal || selection.Total <= 0 || selection.Total > 10000 {
			return fail(422, "invalid subscription episode progress")
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fail(503, "subscription progress is unavailable")
		}
		selection.Missing, err = refreshMissingEpisodes(ctx, tx, strconv.FormatInt(id, 10), oldTotal, lack, selection.Total)
		tx.Rollback()
		if err != nil {
			return fail(422, "subscription progress could not be verified")
		}
		plan.season, plan.total = selection.Season, selection.Total
		plan.storedMissing = append([]int{}, selection.Missing...)
	}
	coverage, err := service.nativeMediaExistence(ctx, meta, detail, mediaKind)
	if err != nil {
		return fail(503, "subscription inventory could not be verified")
	}
	plan.LibraryComplete = coverage.Complete
	if coverage.Complete {
		return plan, nil
	}
	if kind == "TV" {
		libraryMissing := map[int]bool{}
		for _, n := range coverage.Missing[selection.Season] {
			libraryMissing[n] = true
		}
		intersection := []int{}
		for _, n := range selection.Missing {
			if libraryMissing[n] {
				intersection = append(intersection, n)
			}
		}
		selection.Missing = intersection
		plan.needed = append([]int{}, intersection...)
		if len(selection.Missing) == 0 {
			return plan, nil
		}
	}
	groups, err := api.runner.filters.List(ctx)
	if err != nil {
		return fail(503, "subscription filters are unavailable")
	}
	if _, _, err := planSubscriptionCandidates(ctx, selection, groups, nil); err != nil {
		return fail(422, "invalid subscription filter configuration")
	}
	if provider != nil {
		identified, failure := provider(ctx, raw, mediaKind, input.MediaID)
		if failure != nil {
			return plan, failure
		}
		plan.Fetched = len(identified)
		plan.Candidates, plan.Remaining, err = planSubscriptionCandidates(ctx, selection, groups, identified)
		if err != nil {
			return fail(422, "subscription RSS candidate planning failed")
		}
		return plan, nil
	}
	// Do not normalize malformed saved JSON into an empty/global selection.
	encoded := json.RawMessage(text(raw["SEARCH_SITES"]))
	var old map[string]any
	if json.Unmarshal([]byte(text(raw["DESC"])), &old) == nil && old != nil {
		if value, exists := old["search_sites"]; exists {
			encoded, err = json.Marshal(value)
			if err != nil {
				return fail(422, "invalid subscription search sites")
			}
		}
	}
	sites, err := rssSubscriptionSiteList(encoded)
	if err != nil {
		return fail(422, "invalid subscription search sites")
	}
	keyword := strings.TrimSpace(text(item["keyword"]))
	if keyword == "" {
		keyword = meta.Title
	}
	resources, err := api.search.fetchResources(ctx, keyword, sites)
	if err != nil {
		return fail(502, "subscription indexer search could not be completed")
	}
	plan.Fetched = len(resources)
	identified := []identifiedSubscriptionResource{}
	for _, resource := range resources {
		result, failure := api.runner.recognition.recognizeKind(ctx, resource.Title, resource.Description, mediaKind)
		if failure != nil {
			return plan, failure
		}
		if result == nil || strconv.FormatInt(result.detail.ID, 10) != input.MediaID {
			continue
		}
		identified = append(identified, identifiedSubscriptionResource{resource: resource, meta: result.meta, revised: text(result.data["rev_string"]), subtitle: result.subtitle})
	}
	plan.Candidates, plan.Remaining, err = planSubscriptionCandidates(ctx, selection, groups, identified)
	if err != nil {
		return fail(422, "subscription candidate planning failed")
	}
	return plan, nil
}
