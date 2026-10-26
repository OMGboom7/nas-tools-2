package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type fuzzyRequest struct {
	Fingerprint                string `json:"fingerprint"`
	RequestID                  string `json:"requestId"`
	Revision                   string `json:"revision"`
	ConfirmRead                bool   `json:"confirmRead"`
	Confirm                    bool   `json:"confirm"`
	ConfirmWhole               bool   `json:"confirmWhole"`
	ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
	Index                      int    `json:"index"`
}

func (r fuzzyRequest) approved() bool {
	return r.Confirm && r.ConfirmWhole && r.ConfirmOldExecutorsStopped
}

type fuzzyReview struct {
	ID             string               `json:"id"`
	RequestID      string               `json:"requestId"`
	Fingerprint    string               `json:"fingerprint"`
	Kind           string               `json:"kind"`
	SubscriptionID int64                `json:"subscriptionId,string"`
	Name           string               `json:"name"`
	Actor          string               `json:"actor"`
	Candidates     []fuzzyCandidateView `json:"candidates"`
	OverEdition    bool                 `json:"overEdition"`
	definition     fuzzyBatchDefinition
}
type fuzzyContinuationReview struct {
	Batch       fuzzyBatch `json:"batch"`
	Fingerprint string     `json:"fingerprint"`
	Actor       string     `json:"actor"`
	CanResume   bool       `json:"canResume"`
	grant       fuzzyGrant
}

func (api *fuzzySubscriptionAPI) grantReview(ctx context.Context, kind string, id int64, actor string) (fuzzyGrantReview, fuzzySourceSnapshot, error) {
	v := fuzzyGrantReview{Sites: []string{}, Targets: []fuzzyGrantTarget{}}
	var s fuzzySourceSnapshot
	err := fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error { var err error; s, err = api.source(ctx, tx, kind, id, actor); return err })
	if err != nil {
		return v, s, err
	}
	groups, err := api.service().filters.List(ctx)
	if err != nil {
		return v, s, err
	}
	snapshot, err := api.service().configStore.Snapshot()
	if err != nil {
		return v, s, err
	}
	if _, err := newFuzzyMatcher(s.raw, kind, text(objectValue(snapshot["pt"])["download_order"]), groups); err != nil {
		return v, s, err
	}
	scope, err := subscriptionRSSScope(s.raw)
	if err != nil {
		return v, s, err
	}
	sites, err := api.service().sites.List(ctx)
	if err != nil {
		return v, s, err
	}
	selected, err := selectedSubscriptionRSSSites(sites, scope)
	if err != nil || len(selected) > 100 {
		return v, s, errFuzzyScope
	}
	item := normalizeNativeSubscriptionRow(s.raw)
	for _, site := range selected {
		v.Sites = append(v.Sites, site.Name)
		downloader, options, err := api.runner.planner.runner.download.service.searchDownloadSettings(ctx, site.Name, addResourceRequest{Setting: text(item["download_setting"]), Directory: text(item["save_path"])})
		if err != nil {
			return v, s, err
		}
		v.Targets = append(v.Targets, fuzzyGrantTarget{Site: site.Name, DownloaderID: strconv.FormatInt(downloader.ID, 10), Downloader: downloader.Name, Type: downloader.Type, Directory: options.SavePath, Paused: options.Paused})
	}
	v.Kind, v.ID, v.Name, v.Year, v.Season, v.OverEdition = kind, id, text(item["name"]), text(item["year"]), text(item["season"]), truthy(item["over_edition"])
	v.Actor, v.Active, v.Fingerprint, v.WorkerRunning = actor, fuzzyGrantCurrent(s, s.grant), s.fingerprint, api.workers.Load() > 0
	return v, s, nil
}
func (api *fuzzySubscriptionAPI) sourceHTTP(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	kind, id, err := fuzzySelector(r)
	if err != nil {
		fuzzyError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if r.Method == "GET" {
		view, _, err := api.grantReview(ctx, kind, id, actor)
		if err != nil {
			fuzzyError(w, err)
			return
		}
		jobs, err := api.list(ctx, kind, id, false)
		if err != nil {
			fuzzyError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"review": view, "jobs": jobs}})
		return
	}
	var in fuzzyRequest
	if !decodeOrganizationRequest(w, r, &in, "invalid fuzzy subscription request") {
		return
	}
	action := r.PathValue("action")
	if action == "authorize" || action == "revoke" {
		if !fuzzyHex(in.Fingerprint, 64) || !in.Confirm || action == "authorize" && !in.approved() {
			writeAPIError(w, 400, 400, "independent source, whole-torrent and old-executor confirmations required")
			return
		}
		_, s, err := api.grantReview(ctx, kind, id, actor)
		if err != nil || s.fingerprint != in.Fingerprint {
			fuzzyError(w, errFuzzyScope)
			return
		}
		if action == "authorize" && text(s.raw["STATE"]) != "R" {
			fuzzyError(w, errFuzzyScope)
			return
		}
		epoch, err := refreshNonce()
		if err != nil {
			fuzzyError(w, err)
			return
		}
		grant := fuzzyGrant{Version: 1, Kind: kind, ID: id, Epoch: epoch, Actor: actor, Authority: s.authority, Source: s.source, Scope: s.scope, Created: time.Now().UTC().Format(time.RFC3339Nano), Authorized: action == "authorize"}
		err = api.write(ctx, func(tx *sql.Tx) error {
			current, err := api.source(ctx, tx, kind, id, actor)
			if err != nil {
				return err
			}
			if current.fingerprint != in.Fingerprint {
				return errFuzzyScope
			}
			if err := fuzzySchema(ctx, tx); err != nil {
				return err
			}
			if err := archiveFuzzyGrant(ctx, tx, kind, id, current.grantRaw); err != nil {
				return err
			}
			payload, err := json.Marshal(grant)
			if err != nil {
				return err
			}
			return fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_SUBSCRIPTION_GRANTS(KIND,SUB_ID,DOCUMENT) VALUES(?,?,?) ON CONFLICT(KIND,SUB_ID) DO UPDATE SET DOCUMENT=excluded.DOCUMENT`, kind, id, string(payload)))
		})
		if err != nil {
			fuzzyError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"active": grant.Authorized}})
		return
	}
	if action != "preview" && action != "save" {
		writeAPIError(w, 405, 405, "unsupported native fuzzy action")
		return
	}
	if !in.ConfirmRead || action == "save" && (!in.approved() || !fuzzyHex(in.RequestID, 32) || !fuzzyHex(in.Fingerprint, 64)) {
		writeAPIError(w, 400, 400, "fresh readonly review and independent whole-torrent submission confirmations required")
		return
	}
	if action == "save" {
		id := fuzzyBatchID(fuzzyBatchDefinition{Kind: kind, SubscriptionID: id, Actor: actor, RequestID: in.RequestID})
		if j, err := api.get(ctx, id); err == nil {
			if j.Fingerprint != in.Fingerprint {
				fuzzyError(w, errFuzzyScope)
				return
			}
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
			return
		} else if !errors.Is(err, errFuzzyMissing) {
			fuzzyError(w, err)
			return
		}
	}
	view, err := api.plan(ctx, kind, id, actor, in.RequestID, nil)
	if err != nil {
		fuzzyError(w, err)
		return
	}
	if action == "preview" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": view})
		return
	}
	if view.Fingerprint != in.Fingerprint {
		fuzzyError(w, errFuzzyScope)
		return
	}
	j, err := api.create(ctx, view.definition)
	if err != nil {
		fuzzyError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
}
func (api *fuzzySubscriptionAPI) plan(ctx context.Context, kind string, id int64, actor, requestID string, provider subscriptionResourceProvider) (fuzzyReview, error) {
	v := fuzzyReview{Candidates: []fuzzyCandidateView{}}
	if requestID == "" {
		var err error
		requestID, err = refreshNonce()
		if err != nil {
			return v, err
		}
	}
	if !fuzzyHex(requestID, 32) {
		return v, errFuzzyScope
	}
	var s fuzzySourceSnapshot
	err := fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error {
		var err error
		s, err = api.source(ctx, tx, kind, id, actor)
		if err != nil {
			return err
		}
		if !fuzzyGrantCurrent(s, s.grant) {
			return errFuzzyScope
		}
		pending, err := readPendingSubscriptionSubmission(ctx, tx, kind, id)
		if err != nil {
			return err
		}
		if pending {
			return errFuzzyScope
		}
		return nil
	})
	if err != nil {
		return v, err
	}
	groups, err := api.service().filters.List(ctx)
	if err != nil {
		return v, err
	}
	configuration, err := api.service().configStore.Snapshot()
	if err != nil {
		return v, err
	}
	matcher, err := newFuzzyMatcher(s.raw, kind, text(objectValue(configuration["pt"])["download_order"]), groups)
	if err != nil {
		return v, err
	}
	if provider == nil {
		sites, err := api.service().sites.List(ctx)
		if err != nil || len(sites) > 100 {
			return v, errFuzzyScope
		}
		batch := &subscriptionRSSBatch{api: api.runner.feeds, sites: sites, feeds: map[int64]*subscriptionFeedCache{}}
		provider = batch.resources
	}
	mediaKind := "movie"
	if kind == "TV" {
		mediaKind = "tv"
	}
	resources, failure := provider(ctx, s.raw, mediaKind, "")
	if failure != nil {
		return v, errors.New("fuzzy RSS feed or recognition could not be verified")
	}
	candidates, err := matcher.selectCandidates(ctx, resources)
	if err != nil {
		return v, err
	}
	d := fuzzyBatchDefinition{Version: 1, Kind: kind, SubscriptionID: id, Actor: actor, RequestID: requestID, Grant: s.grant, Raw: s.raw, Candidates: candidates}
	if !validFuzzyDefinition(d) {
		return v, errFuzzyScope
	}
	err = fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error { return api.current(ctx, tx, d.Grant) })
	if err != nil {
		return v, err
	}
	v.ID, v.RequestID, v.Fingerprint, v.Kind, v.SubscriptionID, v.Name, v.Actor = fuzzyBatchID(d), requestID, fuzzyDigest(d), kind, id, text(s.raw["NAME"]), actor
	v.OverEdition = truthy(normalizeNativeSubscriptionRow(s.raw)["over_edition"])
	for _, c := range candidates {
		v.Candidates = append(v.Candidates, c.View)
	}
	v.definition = d
	return v, nil
}
func (api *fuzzySubscriptionAPI) automatic(ctx context.Context, kind string, id int64, provider subscriptionResourceProvider) (subscriptionSearchRunResult, *recognitionFailure) {
	result := subscriptionSearchRunResult{Remaining: []int{}}
	var grant fuzzyGrant
	err := fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error { var err error; grant, _, err = readFuzzyGrant(ctx, tx, kind, id); return err })
	if err != nil {
		return result, &recognitionFailure{409, "fuzzy rule authority could not be established"}
	}
	if !grant.Authorized {
		result.Skipped = true
		return result, nil
	}
	jobs, err := api.list(ctx, kind, id, false)
	if err != nil {
		return result, &recognitionFailure{503, "fuzzy receipts unavailable"}
	}
	for _, j := range jobs {
		switch j.State {
		case "pending", "running":
			result.Queued, result.BatchID = true, j.ID
			return result, nil
		case "blocked", "uncertain":
			result.Uncertain, result.BatchID = true, j.ID
			return result, &recognitionFailure{409, "original fuzzy batch requires explicit review; no new scan or submission"}
		}
	}
	view, err := api.plan(ctx, kind, id, grant.Actor, "", provider)
	if err != nil {
		return result, &recognitionFailure{409, "fuzzy scope or RSS candidates could not be verified; no submission"}
	}
	if len(view.Candidates) == 0 {
		return result, nil
	}
	j, err := api.create(ctx, view.definition)
	if err != nil {
		return result, &recognitionFailure{409, "fuzzy source changed or already has a batch; readonly review before retrying"}
	}
	result.Queued, result.BatchID = true, j.ID
	return result, nil
}
func (api *fuzzySubscriptionAPI) statusHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	jobs, err := api.list(r.Context(), "", 0, false)
	if err != nil {
		fuzzyError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"workerRunning": api.workers.Load() > 0, "jobs": jobs}})
}
func (api *fuzzySubscriptionAPI) batchHTTP(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorize(w, r)
	if !ok {
		return
	}
	id := r.PathValue("batch")
	j, err := api.get(r.Context(), id)
	if err != nil {
		fuzzyError(w, err)
		return
	}
	if r.Method == "GET" {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
		return
	}
	var in fuzzyRequest
	if !decodeOrganizationRequest(w, r, &in, "invalid fuzzy batch action") {
		return
	}
	action := r.PathValue("action")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if action == "restore" {
		if !in.Confirm || in.Revision != j.Revision || !fuzzyHex(in.RequestID, 32) {
			writeAPIError(w, 400, 400, "fresh receipt and stable request ID required to restore a disabled rule copy")
			return
		}
		created, err := restoreFuzzyRule(ctx, api.service(), id, in.RequestID)
		if err != nil {
			fuzzyError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"id": strconv.FormatInt(created, 10), "batchId": id, "requestId": in.RequestID, "executionAuthorized": false}})
		return
	} else if action == "stop" {
		if !in.Confirm {
			writeAPIError(w, 400, 400, "explicit batch and future-rule pause confirmation required")
			return
		}
		if err := api.finishState(ctx, id, "stopped", "administrator stopped this batch and paused its matching original rule; accepted submissions cannot be recalled"); err != nil {
			fuzzyError(w, err)
			return
		}
	} else if action == "review" || action == "resume" {
		if !in.ConfirmRead {
			writeAPIError(w, 400, 400, "explicit readonly receipt review required")
			return
		}
		var s fuzzySourceSnapshot
		err = fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error {
			var err error
			s, err = api.source(ctx, tx, j.Kind, j.SubscriptionID, actor)
			return err
		})
		if err != nil || !fuzzyGrantCurrent(s, s.grant) || !validFuzzyBatchAuthorization(j.definition, s.grant) {
			fuzzyError(w, errFuzzyScope)
			return
		}
		review := fuzzyContinuationReview{Batch: j, Actor: actor, Fingerprint: fuzzyDigest([]any{j.Revision, s.fingerprint}), CanResume: j.State == "running" || j.State == "blocked" || j.State == "uncertain", grant: s.grant}
		for _, item := range j.Items {
			if item.State == "intent" || item.State == "uncertain" {
				review.CanResume = false
			}
		}
		if action == "review" {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
			return
		}
		if !in.approved() || !review.CanResume || in.Revision != j.Revision || in.Fingerprint != review.Fingerprint {
			writeAPIError(w, 400, 400, "fresh exact-batch review and independent remaining-target confirmations required")
			return
		}
		guard, err := api.records.AcquireJob(ctx, "native-fuzzy-"+id)
		if err != nil {
			fuzzyError(w, err)
			return
		}
		err = api.resume(ctx, id, in.Revision, s.grant)
		guard.Close()
		if err != nil {
			fuzzyError(w, err)
			return
		}
	} else if action == "verify" {
		if !in.ConfirmRead || !in.Confirm || in.Revision != j.Revision {
			writeAPIError(w, 400, 400, "fresh receipt revision and explicit adoption of readonly positive evidence required")
			return
		}
		if err := api.verify(ctx, id, in.Index); err != nil {
			fuzzyError(w, err)
			return
		}
	} else if action == "execute" {
		if !in.approved() || in.Revision != j.Revision || j.State != "pending" {
			writeAPIError(w, 400, 400, "fresh pending batch and independent confirmations required")
			return
		}
		if err := api.run(ctx, id); err != nil {
			fuzzyError(w, err)
			return
		}
	} else {
		writeAPIError(w, 405, 405, "unsupported native fuzzy batch action")
		return
	}
	j, err = api.get(ctx, id)
	if err != nil {
		fuzzyError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
}
func (api *fuzzySubscriptionAPI) fallback(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.authorize(w, r); !ok {
		return
	}
	writeAPIError(w, 405, 405, "unsupported native fuzzy subscription method or action")
}
