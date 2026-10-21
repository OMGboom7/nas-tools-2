package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type legacyRedoInput struct {
	Flag                       string          `json:"flag"`
	IDs                        json.RawMessage `json:"ids"`
	ConfirmSourceRemoval       bool            `json:"confirmSourceRemoval"`
	ReviewFingerprint          string          `json:"reviewFingerprint,omitempty"`
	ConfirmRemoteExclusive     bool            `json:"confirmRemoteExclusive,omitempty"`
	ConfirmRemoteRecoveryCopy  bool            `json:"confirmRemoteRecoveryCopy,omitempty"`
	ConfirmOldExecutorsStopped bool            `json:"confirmOldExecutorsStopped,omitempty"`
}
type legacyRedoResult struct {
	ID      string `json:"id"`
	JobID   string `json:"jobId,omitempty"`
	State   string `json:"state"`
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

func (api organizationAPI) legacyRequestMode(mode string) (string, error) {
	if mode != "" {
		return mode, nil
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	mode = text(objectValue(snapshot["pt"])["rmt_mode"])
	if mode == "" {
		mode = "copy"
	}
	if named := transferModes[mode]; named != "" {
		mode = named
	}
	return mode, nil
}

func legacyRedoFailure(err error, message string) *recognitionFailure {
	status := 503
	if errors.Is(err, organization.ErrState) || errors.Is(err, organization.ErrClaimed) || errors.Is(err, organization.ErrBusy) || errors.Is(err, organization.ErrPath) {
		status = 409
	}
	if errors.Is(err, organization.ErrNotFound) {
		status = 404
	}
	if errors.Is(err, organization.ErrMode) {
		status = 422
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = 504
	}
	return &recognitionFailure{status, message}
}

func decodeLegacyRedo(w http.ResponseWriter, r *http.Request) (legacyRedoInput, bool) {
	input := legacyRedoInput{}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media == "application/json" {
		if !decodeOrganizationRequest(w, r, &input, "invalid legacy re-identification request") {
			return input, false
		}
	} else if media == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if r.ParseForm() != nil {
			writeAPIError(w, 400, 400, "invalid legacy re-identification form")
			return input, false
		}
		for key := range r.PostForm {
			if key != "flag" && key != "ids" && key != "confirmSourceRemoval" && key != "reviewFingerprint" && key != "confirmRemoteExclusive" && key != "confirmRemoteRecoveryCopy" && key != "confirmOldExecutorsStopped" {
				writeAPIError(w, 400, 400, "unsupported legacy re-identification field")
				return input, false
			}
		}
		if len(r.PostForm["flag"]) != 1 || len(r.PostForm["ids"]) < 1 || len(r.PostForm["confirmSourceRemoval"]) > 1 {
			writeAPIError(w, 400, 400, "invalid legacy re-identification form")
			return input, false
		}
		input.Flag = r.PostForm.Get("flag")
		input.ReviewFingerprint = r.PostForm.Get("reviewFingerprint")
		for _, key := range []string{"reviewFingerprint", "confirmRemoteExclusive", "confirmRemoteRecoveryCopy", "confirmOldExecutorsStopped"} {
			if len(r.PostForm[key]) > 1 {
				writeAPIError(w, 400, 400, "duplicate remote consent or reviewed fingerprint")
				return input, false
			}
		}
		for key, dest := range map[string]*bool{"confirmRemoteExclusive": &input.ConfirmRemoteExclusive, "confirmRemoteRecoveryCopy": &input.ConfirmRemoteRecoveryCopy, "confirmOldExecutorsStopped": &input.ConfirmOldExecutorsStopped} {
			if value := r.PostForm.Get(key); value != "" {
				if value != "true" && value != "false" {
					writeAPIError(w, 400, 400, "invalid remote consent")
					return input, false
				}
				*dest = value == "true"
			}
		}
		ids := r.PostForm["ids"]
		if len(ids) == 1 && len(ids[0]) > 0 && ids[0][0] == '[' {
			input.IDs = json.RawMessage(ids[0])
		} else {
			input.IDs, _ = json.Marshal(ids)
		}
		if value := r.PostForm.Get("confirmSourceRemoval"); value != "" {
			if value != "true" && value != "false" {
				writeAPIError(w, 400, 400, "invalid source-removal consent")
				return input, false
			}
			input.ConfirmSourceRemoval = value == "true"
		}
	} else {
		writeAPIError(w, 400, 400, "unsupported re-identification content type")
		return input, false
	}
	if input.Flag == "unknow" {
		input.Flag = "unidentification"
	} // Old REST help spelling.
	if input.Flag != "unidentification" && input.Flag != "history" {
		writeAPIError(w, 400, 400, "select unknown or historical re-identification")
		return input, false
	}
	return input, true
}

func (api organizationAPI) legacyRedoPlan(ctx context.Context, kind string, id int64, actor string) (organizationPlan, *recognitionFailure) {
	fail := func(status int, message string) (organizationPlan, *recognitionFailure) {
		return organizationPlan{}, &recognitionFailure{status, message}
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return fail(503, "configured organization directories are unavailable")
	}
	input := organizationPlanInput{AllowProcessedUnknown: kind == "unidentification"}
	var dest string
	modeFor := func(stored string) (string, error) {
		if stored != "" {
			return transferModes[stored], nil
		}
		return api.legacyRequestMode("")
	}
	if kind == "unidentification" {
		record, err := api.jobs.GetUnknown(ctx, id)
		if err != nil {
			return fail(409, "unknown record unavailable; inspect its saved state")
		}
		if record.State != "N" && record.State != "Y" {
			return fail(422, "unknown record has no supported state")
		}
		input.Mode, err = modeFor(record.Mode)
		if err != nil {
			return fail(503, "organization configuration unavailable")
		}
		projection := projectUnknown(record, roots, input.Mode)
		if projection.Reason != "" {
			return fail(422, "unknown source, destination or mode is outside supported configured scope")
		}
		input.SourceID, input.Path = projection.SourceID, projection.Relative
		input.UnknownRecord = &organizationUnknownSelector{ID: id, Fingerprint: organization.Digest(record)}
		dest = record.Dest
	} else {
		record, err := api.jobs.GetHistory(ctx, id)
		if err != nil {
			return fail(409, "historical record unavailable; inspect its saved state")
		}
		input.Mode, err = modeFor(record.Mode)
		if err != nil {
			return fail(503, "organization configuration unavailable")
		}
		projection := projectHistory(record, roots, input.Mode)
		if projection.Reason != "" {
			return fail(422, "historical original source, destination or mode is outside supported configured scope")
		}
		input.SourceID, input.Path = projection.SourceID, projection.Relative
		input.HistoryRecord = &organizationHistorySelector{ID: id, Fingerprint: organization.Digest(record)}
		dest = record.Dest
	}
	input.LegacyTargetOverride = dest != ""
	if !organization.SupportedMode(input.Mode) {
		return fail(501, "selected transfer mode is not migrated; no local fallback")
	}
	var best organizationPlan
	var last *recognitionFailure
	for _, target := range roots.Targets {
		if dest != "" && !organizationTargetMatches(target.Path, ".", dest, input.Mode) {
			continue
		}
		if dest == "" && target.Type == "" {
			continue
		} // Default means configured media library, not an unrelated sync destination.
		input.TargetID = target.ID
		plan, failure := api.plan(ctx, input)
		if failure != nil {
			last = failure
			continue
		}
		if len(plan.definition.Entries) == 0 || plan.Skipped != 0 {
			best = plan
			continue
		}
		complete := true
		for _, item := range plan.Items {
			if item.Status != "available" {
				complete = false
				break
			}
		}
		if !complete {
			best = plan
			continue
		}
		plan.definition.LegacyOrigin = &organization.LegacyOrganizationOrigin{Action: "redo", Kind: kind, RecordID: strconv.FormatInt(id, 10), Actor: actor}
		plan.Fingerprint = organization.Digest(struct {
			Definition organization.Definition
			Items      []organizationPlanItem
		}{plan.definition, plan.Items})
		return plan, nil
	}
	if last != nil && len(best.Items) == 0 {
		return organizationPlan{}, last
	}
	return fail(422, "the full original scope is not available in one supported configured destination; review conflicts, skipped files or mixed destinations")
}

func (api organizationAPI) runLegacyRedo(ctx context.Context, id string, sourceRemoval bool) (organization.Job, error) {
	job, err := api.jobs.Get(ctx, id)
	if err != nil {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if current, readErr := api.jobs.Get(readCtx, id); readErr == nil {
			job = current
		}
		return job, err
	}
	if organization.MoveMode(job.Definition.Mode) && !sourceRemoval {
		return job, organization.ErrState
	}
	if organization.RemoteMode(job.Mode) && !api.remoteApproval.Valid() {
		return job, organization.ErrState
	}
	guard, err := api.jobs.AcquireJob(ctx, id)
	if err != nil {
		return job, err
	}
	defer guard.Close()
	job, err = api.jobs.Get(ctx, id)
	if err != nil {
		return job, err
	}
	if job.State != "ready" && job.State != "completed" {
		return job, organization.ErrState
	}
	if err = api.validateJob(ctx, job); err == nil {
		err = api.runJob(ctx, job, false)
	}
	if err == nil {
		err = api.resolveUnknown(ctx, job)
	}
	if err != nil {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if current, readErr := api.jobs.Get(readCtx, id); readErr == nil {
			job = current
		}
		return job, err
	}
	job, err = api.jobs.Get(ctx, id)
	if err == nil && job.State != "completed" {
		err = organization.ErrState
	}
	return job, err
}

func (api organizationAPI) serveLegacyRedo(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	input, ok := decodeLegacyRedo(w, r)
	if !ok {
		return
	}
	selectors, err := legacyUnknownIDs(input.IDs)
	if err != nil {
		writeAPIError(w, 400, 400, "invalid re-identification IDs")
		return
	}
	seen := map[int64]bool{}
	for _, selector := range selectors {
		if seen[selector.ID] {
			writeAPIError(w, 400, 400, "duplicate re-identification ID")
			return
		}
		seen[selector.ID] = true
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid or expired authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	plans := make([]organizationPlan, len(selectors))
	failures := make([]*recognitionFailure, len(selectors))
	for i, selector := range selectors {
		plans[i], failures[i] = api.legacyRedoPlan(ctx, input.Flag, selector.ID, claims.Username)
		if failures[i] == nil && organization.MoveMode(plans[i].definition.Mode) && !input.ConfirmSourceRemoval {
			writeAPIError(w, 400, 400, "separate source-removal consent is required before any job is created or executed")
			return
		}
	}
	review := makeLegacyRedoReview(input.Flag, selectors, plans, failures)
	remote := false
	for i := range plans {
		remote = remote || failures[i] == nil && organization.RemoteMode(plans[i].Mode)
	}
	if input.ReviewFingerprint != "" && input.ReviewFingerprint != review.Fingerprint {
		writeAPIError(w, 409, 409, "reviewed re-identification scope or account changed; review again")
		return
	}
	if remote && (input.ReviewFingerprint != review.Fingerprint || api.approveLegacyRemote(claims.Username, "legacy-redo", input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy, input.ConfirmOldExecutorsStopped) != nil) {
		writeAPIError(w, 400, 400, "remote re-identification requires fresh scope review and independent remote/stopped-writer consent before reserving any job")
		return
	}
	results := []legacyRedoResult{}
	success := true
	for i, selector := range selectors {
		result := legacyRedoResult{ID: strconv.FormatInt(selector.ID, 10), State: "not_started"}
		failure := failures[i]
		if failure == nil {
			// Bind the source/configuration/TMDB/old-row snapshot a second time
			// immediately before reservation. Direct POST is the legacy transfer
			// request, not an automatic startup retry or a readonly-preview result.
			fresh, f := api.legacyRedoPlan(ctx, input.Flag, selector.ID, claims.Username)
			failure = f
			if failure == nil && fresh.Fingerprint != plans[i].Fingerprint {
				failure = &recognitionFailure{409, "source or plan changed; review a fresh request"}
			}
			if failure == nil {
				id, err := api.jobs.Create(ctx, fresh.Fingerprint, fresh.definition)
				if err != nil {
					failure = legacyRedoFailure(err, "job reservation failed; inspect saved jobs before retry")
				} else {
					result.JobID = id
					job, err := api.runLegacyRedo(ctx, id, input.ConfirmSourceRemoval)
					result.State = job.State
					if err != nil {
						failure = legacyRedoFailure(err, "execution or compatibility receipt requires review; do not automatically retry")
					}
				}
			}
		}
		if failure != nil {
			success = false
			result.Code, result.Message = failure.status, failure.message
		}
		results = append(results, result)
	}
	code, message := 0, "转移成功"
	if !success {
		code, message = 2, "部分记录未完成；检查逐记录结果和已保存任务，不自动重跑"
	}
	writeJSON(w, 200, map[string]any{"code": code, "success": success, "message": message, "data": map[string]any{"results": results}})
}
