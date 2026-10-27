package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type subscriptionSearchRunner struct {
	planner subscriptionSearchPlanner
	mu      sync.Mutex
	running map[string]bool
	feeds   *subscriptionRSSAPI
	fuzzy   *fuzzySubscriptionAPI
}

type subscriptionSearchRunResult struct {
	Seasons         []subscriptionSeasonResult `json:"seasons,omitempty"`
	FileSelectionID string                     `json:"fileSelectionId,omitempty"`
	Submitted       int                        `json:"submitted"`
	Remaining       []int                      `json:"remaining"`
	Completed       bool                       `json:"completed"`
	Uncertain       bool                       `json:"uncertain"`
	Queued          bool                       `json:"queued,omitempty"`
	Skipped         bool                       `json:"skipped,omitempty"`
	BatchID         string                     `json:"batchId,omitempty"`
	Upgrade         *subscriptionEditionView   `json:"upgrade,omitempty"`
}

func (api *subscriptionSearchRunner) serveCompatSearch(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil {
		writeAPIError(w, 400, 400, "invalid subscription search form")
		return
	}
	r.SetPathValue("action", "refresh")
	r.SetPathValue("type", r.PostForm.Get("type"))
	r.SetPathValue("id", r.PostForm.Get("rssid"))
	api.serveHTTP(w, r)
}

func (api *subscriptionSearchRunner) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if api.planner.runner == nil || api.planner.search == nil || api.planner.runner.download.service.auth == nil {
		writeAPIError(w, 503, 503, "native subscription execution is unavailable")
		return
	}
	auth := api.planner.runner.download.service.auth
	claims, err := auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return
	}
	if !auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil {
		writeAPIError(w, 400, 400, "invalid subscription execution request")
		return
	}
	rawID, kind := r.Form.Get("id"), r.Form.Get("type")
	if r.PathValue("action") == "refresh" {
		rawID, kind = r.PathValue("id"), r.PathValue("type")
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 || kind != "MOV" && kind != "TV" {
		writeAPIError(w, 400, 400, "invalid subscription selector")
		return
	}
	result, failure := api.execute(r.Context(), kind, id)
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"code": failure.status, "success": false, "message": failure.message, "data": result})
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": result})
}

func (api *subscriptionSearchRunner) execute(parent context.Context, kind string, id int64) (subscriptionSearchRunResult, *recognitionFailure) {
	return api.executeForState(parent, kind, id, "")
}

func (api *subscriptionSearchRunner) executeForState(parent context.Context, kind string, id int64, state string) (subscriptionSearchRunResult, *recognitionFailure) {
	return api.executeResources(parent, kind, id, state, nil)
}

func (api *subscriptionSearchRunner) executeResources(parent context.Context, kind string, id int64, state string, provider subscriptionResourceProvider) (subscriptionSearchRunResult, *recognitionFailure) {
	result := subscriptionSearchRunResult{Remaining: []int{}}
	fail := func(status int, message string) (subscriptionSearchRunResult, *recognitionFailure) {
		return result, &recognitionFailure{status, message}
	}
	if !api.planner.runner.pureGo {
		return fail(501, "native subscription execution requires disabled legacy backend")
	}
	key := kind + ":" + strconv.FormatInt(id, 10)
	api.mu.Lock()
	if api.running == nil {
		api.running = map[string]bool{}
	}
	if api.running[key] {
		api.mu.Unlock()
		return fail(409, "subscription search is already running")
	}
	api.running[key] = true
	api.mu.Unlock()
	defer func() { api.mu.Lock(); delete(api.running, key); api.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	if provider != nil && api.fuzzy != nil {
		service := api.planner.runner.recognition.service
		db, err := service.openNativeSubscriptionDatabase(ctx)
		if err != nil {
			return fail(503, "subscription source unavailable")
		}
		raw, err := readNativeSubscriptionRow(ctx, db, subscriptionTable(kind), id)
		db.Close()
		if err != nil {
			return fail(503, "subscription source unavailable")
		}
		if raw != nil && truthy(normalizeNativeSubscriptionRow(raw)["fuzzy_match"]) {
			if state != "" && text(raw["STATE"]) != state {
				return fail(409, "subscription scheduled state changed")
			}
			return api.fuzzy.automatic(ctx, kind, id, provider)
		}
	}
	plan, failure := api.planner.planResources(ctx, kind, id, state, true, provider)
	if failure != nil {
		return result, failure
	}
	result.Remaining = append([]int{}, plan.needed...)
	result.Upgrade = editionView(plan.edition)
	service := api.planner.runner.recognition.service
	if plan.LibraryComplete || kind == "TV" && len(plan.needed) == 0 || len(plan.Candidates) == 0 {
		if err := service.persistSubscriptionSubmission(ctx, kind, id, &plan, nil, ""); err != nil {
			return fail(409, "subscription search progress could not be saved; verify current subscription state")
		}
		result.Completed = subscriptionPlanCompleted(kind, plan, false)
		result.Upgrade = editionView(plan.edition)
		return result, nil
	}
	for _, candidate := range plan.Candidates {
		plan.files = nil
		plan.fileSelectionID = ""
		plan.cohort = nil
		if ctx.Err() != nil {
			return fail(504, "subscription execution was cancelled or timed out")
		}
		download := api.planner.runner.download.service
		item := normalizeNativeSubscriptionRow(plan.raw)
		downloader, options, err := download.searchDownloadSettings(ctx, candidate.resource.Indexer, addResourceRequest{Setting: text(item["download_setting"]), Directory: text(item["save_path"])})
		if errors.Is(err, errDownloadOptionsUnsupported) {
			return fail(501, "subscription downloader does not support these settings")
		}
		if errors.Is(err, errSearchDownloadSelection) {
			return fail(422, "invalid subscription download setting or directory selection")
		}
		if err != nil {
			return fail(503, "subscription download settings are unavailable or unsupported")
		}
		options, err = download.preparePTSource(ctx, downloader, candidate.resource, options)
		if err != nil {
			return fail(501, "PT subscription source requires a configuration-scoped qBittorrent/Transmission grant, no HR marker and zero automatic stop limits")
		}
		magnet, torrent, err := download.prepareSubscriptionTorrent(ctx, candidate)
		if err != nil {
			return fail(502, "subscription torrent could not be retrieved")
		}
		torrent, options, err = api.prepareSubscriptionFiles(ctx, kind, id, &plan, &candidate, downloader, options, magnet, torrent, provider)
		if err != nil {
			return fail(422, "complete torrent manifest, exact selectable episodes or original downloader unavailable; no download was added")
		}
		proof := newSubscriptionSubmissionProof(downloader, magnet, torrent)
		proof.ProfileDigest = downloadProfileFingerprint(downloader, proof.InfoHash, options)
		if plan.files != nil {
			proof.ProfileDigest = plan.files.ProfileFingerprint()
		}
		owner, err := service.reserveSubscriptionSubmission(ctx, kind, id, plan, candidate, proof)
		if err != nil {
			return fail(409, "subscription changed or resource is reserved; verify pending downloads before retrying")
		}
		if plan.edition != nil {
			plan.edition.Started = true
			result.Upgrade = editionView(plan.edition)
		}
		if plan.fileSelectionID != "" {
			result.FileSelectionID = plan.fileSelectionID
		}
		var handled bool
		if plan.files != nil {
			_, handled, err = api.submitSubscriptionFiles(ctx, downloader, torrent, options, *plan.files)
		} else {
			_, handled, err = download.nativeAddConfiguredDownload(ctx, downloader, magnet, torrent, options)
		}
		if err != nil || !handled {
			if downloadDefinitelyNotSubmitted(handled, err) {
				cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				releaseErr := service.releaseSubscriptionSubmission(cleanup, candidate.resource.DownloadURL, owner)
				stop()
				if releaseErr != nil {
					result.Uncertain = true
					return fail(503, "rejected submission reservation requires verification")
				}
				return fail(422, "downloader rejected subscription settings or authentication before submission")
			}
			result.Uncertain = true
			return fail(502, "subscription submission is uncertain; automatic retry is blocked")
		}
		if err := service.persistSubscriptionSubmission(ctx, kind, id, &plan, &candidate, owner); err != nil {
			result.Uncertain = true
			return fail(502, "downloader accepted resource but persistence requires verification; retry is blocked")
		}
		result.recordSeasons(plan.cohort)
		result.Submitted++
		result.Remaining = append([]int{}, plan.needed...)
		result.Completed = subscriptionPlanCompleted(kind, plan, true)
		result.Upgrade = editionView(plan.edition)
		for i := range result.Seasons {
			if result.Seasons[i].SubscriptionID == strconv.FormatInt(id, 10) {
				result.Seasons[i].Remaining = append([]int{}, result.Remaining...)
				result.Seasons[i].Completed = result.Completed
			}
		}
		if result.Completed || plan.edition != nil && plan.edition.Cycle.State == "completed" {
			break
		}
	}
	return result, nil
}
