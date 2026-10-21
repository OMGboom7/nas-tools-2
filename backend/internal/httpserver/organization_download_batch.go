package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type downloadBatchRequest struct {
	ConfirmRemoteExclusive    bool `json:"confirmRemoteExclusive,omitempty"`
	ConfirmRemoteRecoveryCopy bool `json:"confirmRemoteRecoveryCopy,omitempty"`
	actor                     string
	downloadCompletionInput
	Fingerprint                string `json:"fingerprint,omitempty"`
	Reason                     string `json:"reason,omitempty"`
	Confirm                    bool   `json:"confirm,omitempty"`
	ConfirmOmitted             bool   `json:"confirmOmitted,omitempty"`
	ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped,omitempty"`
	ConfirmSourceRemoval       bool   `json:"confirmSourceRemoval,omitempty"`
	ConfirmTaskStopped         bool   `json:"confirmTaskStopped,omitempty"`
	ConfirmTaskRemoval         bool   `json:"confirmTaskRemoval,omitempty"`
	ConfirmExclusiveLabels     bool   `json:"confirmExclusiveLabels,omitempty"`
}

func downloadBatchGuard(b organization.DownloadBatch) string {
	return "download-completion:" + organization.DownloadTaskKey(b.Origin.DownloaderID, b.Origin.Task.ID)
}
func (api organizationAPI) serveDownloadPlan(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input downloadCompletionInput
	if !decodeOrganizationRequest(w, r, &input, "invalid download organization review") {
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	review, failure := api.planDownload(ctx, input, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
}
func (api organizationAPI) serveDownloadBatchSave(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input downloadBatchRequest
	if !decodeOrganizationRequest(w, r, &input, "invalid download organization reservation") {
		return
	}
	if !input.Confirm || !input.ConfirmOldExecutorsStopped || len(input.Fingerprint) != 64 {
		writeAPIError(w, 400, 400, "review the scope and confirm old executors are actually stopped; saving does not execute")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	guard, err := api.jobs.AcquireJob(ctx, "download-completion:"+organization.DownloadTaskKey(input.DownloaderID, strings.ToLower(input.TaskID)))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	existing, err := api.jobs.FindDownloadBatch(ctx, input.DownloaderID, strings.ToLower(input.TaskID))
	if err == nil {
		if existing.Fingerprint != input.Fingerprint {
			writeAPIError(w, 409, 409, "this downloader/task already has a different immutable batch; read its receipt instead of reserving again")
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": existing})
		return
	}
	if !errors.Is(err, organization.ErrNotFound) {
		writeOrganizationError(w, err)
		return
	}
	review, failure := api.planDownload(ctx, input.downloadCompletionInput, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	if review.Fingerprint != input.Fingerprint || len(review.Omitted) > 0 && !input.ConfirmOmitted {
		writeAPIError(w, 409, 409, "fresh exact plan and independent omitted-file acknowledgement required")
		return
	}
	definitions := []organization.Definition{}
	for _, p := range review.Plans {
		definitions = append(definitions, p.definition)
	}
	batch, definitions, err := api.jobs.BeginDownloadBatch(ctx, review.Fingerprint, review.origin, definitions, review.Omitted)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	for _, d := range definitions {
		if _, err = api.jobs.Create(ctx, organization.Digest(d), d); err != nil {
			api.failDownloadBatch(batch, "reservation interrupted; inspect linked jobs, never repeat saving")
			writeOrganizationError(w, err)
			return
		}
	}
	batch, err = api.jobs.GetDownloadBatch(ctx, batch.ID)
	if err == nil && len(batch.Jobs) != batch.Expected {
		err = organization.ErrState
	}
	if err == nil {
		err = api.jobs.SetDownloadBatch(ctx, batch.ID, "reserving", "ready", "", "")
	}
	if err != nil {
		api.failDownloadBatch(batch, "reservation receipt interrupted; inspect saved batch and all linked jobs")
		writeOrganizationError(w, err)
		return
	}
	batch, err = api.jobs.GetDownloadBatch(ctx, batch.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"code": 0, "success": true, "data": batch})
}
func (api organizationAPI) failDownloadBatch(b organization.DownloadBatch, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := api.jobs.GetDownloadBatch(ctx, b.ID)
	if err == nil && current.State != "completed" && current.State != "result_pending" {
		_ = api.jobs.SetDownloadBatch(ctx, b.ID, current.State, "needs_review", reason, current.ResultProof)
	}
}
func (api organizationAPI) serveDownloadBatchFind(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	id, err := syncConfigID(r.URL.Query().Get("downloaderId"))
	if err != nil || id < 1 || r.URL.Query().Get("taskId") == "" {
		writeAPIError(w, 400, 400, "exact downloader and task IDs required")
		return
	}
	b, err := api.jobs.FindDownloadBatch(r.Context(), r.URL.Query().Get("downloaderId"), strings.ToLower(r.URL.Query().Get("taskId")))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": b})
}
func (api organizationAPI) serveDownloadBatchGet(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	b, err := api.jobs.GetDownloadBatch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": b})
}

func (api organizationAPI) verifyDownloadBatchFiles(ctx context.Context, b organization.DownloadBatch) error {
	if len(b.Jobs) == 0 || len(b.Jobs) != b.Expected {
		return organization.ErrState
	}
	if err := api.jobs.VerifyDownloadConfig(ctx, b.Origin); err != nil {
		return err
	}
	for _, id := range b.Jobs {
		job, err := api.jobs.Get(ctx, id)
		if err != nil {
			return err
		}
		if job.State != "completed" || job.Download == nil || job.Download.BatchID != b.ID || organization.Digest(*job.Download) != organization.Digest(b.Origin) || job.Mode != b.Mode {
			return organization.ErrState
		}
		if err := api.validateJobConfiguration(ctx, job); err != nil {
			return err
		}
		jobCtx, closeRemote, err := api.remoteContext(ctx, job)
		if err != nil {
			return err
		}
		for _, item := range job.Items {
			if err := organization.VerifyCompletedTarget(jobCtx, job, item); err != nil {
				closeRemote()
				return err
			}
		}
		closeRemote()
	}
	return nil
}
func (api organizationAPI) serveDownloadBatchAction(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	action := r.PathValue("action")
	if action != "execute" && action != "reconcile" && action != "result-review" && action != "finalize" && action != "reconcile-result" && action != "reservation-review" && action != "recover-reservation" {
		writeAPIError(w, 404, 404, "download batch action not found")
		return
	}
	var input downloadBatchRequest
	if !decodeOrganizationRequest(w, r, &input, "invalid download batch action") {
		return
	}
	if action != "result-review" && action != "reservation-review" && !input.Confirm {
		writeAPIError(w, 400, 400, "explicit action confirmation required")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	input.actor = claims.Username
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	b, err := api.jobs.GetDownloadBatch(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	guard, err := api.jobs.AcquireJob(ctx, downloadBatchGuard(b))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	b, err = api.jobs.GetDownloadBatch(ctx, b.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	switch action {
	case "reservation-review":
		var review downloadReservationReview
		review, err = api.reviewDownloadReservation(ctx, b, input.actor, input.Reason)
		if err == nil {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
			return
		}
	case "recover-reservation":
		err = api.recoverDownloadReservation(ctx, b, input)
	case "execute":
		err = api.executeDownloadBatch(ctx, b, input)
	case "reconcile":
		if b.State == "result_pending" || b.State == "completed" {
			err = organization.ErrState
		} else {
			err = api.verifyDownloadBatchFiles(ctx, b)
			if err == nil {
				err = api.jobs.SetDownloadBatch(ctx, b.ID, b.State, "files_done", "", "")
			} else {
				api.failDownloadBatch(b, "saved jobs or published proofs still require explicit individual review")
			}
		}
	case "result-review":
		var review downloadResultReview
		review, err = api.reviewDownloadResult(ctx, b, input.actor)
		if err == nil {
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
			return
		}
	case "finalize":
		err = api.finalizeDownloadBatch(ctx, b, input)
	case "reconcile-result":
		err = api.reconcileDownloadResult(ctx, b)
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	b, err = api.jobs.GetDownloadBatch(ctx, b.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": b})
}
func (api organizationAPI) executeDownloadBatch(ctx context.Context, b organization.DownloadBatch, input downloadBatchRequest) error {
	if b.State != "ready" || len(b.Jobs) != b.Expected {
		return organization.ErrState
	}
	mode := ""
	for _, id := range b.Jobs {
		job, err := api.jobs.Get(ctx, id)
		if err != nil {
			return err
		}
		if job.State != "ready" && job.State != "completed" {
			return organization.ErrState
		}
		if mode != "" && mode != job.Mode {
			return organization.ErrState
		}
		mode = job.Mode
		if job.State != "completed" {
			if err := api.validateJob(ctx, job); err != nil {
				return err
			}
		}
	}
	if organization.MoveMode(mode) && (!input.ConfirmSourceRemoval || !input.ConfirmTaskStopped) {
		return organization.ErrState
	}
	if organization.RemoteMode(mode) && api.monitorGeneration == "" && api.downloadGeneration == "" {
		if input.actor == "" || !input.ConfirmOldExecutorsStopped || !input.ConfirmRemoteExclusive || !input.ConfirmRemoteRecoveryCopy {
			return organization.ErrState
		}
		api.remoteApproval = organization.RemoteApproval{Actor: input.actor, Created: time.Now().UTC().Format(time.RFC3339Nano), Exclusive: true, RecoveryCopy: true, Origin: "download-batch"}
	}
	if err := api.setDownloadBatch(ctx, b.ID, "ready", "executing", "", ""); err != nil {
		return err
	}
	for _, id := range b.Jobs {
		guard, err := api.jobs.AcquireJob(ctx, id)
		if err != nil {
			api.failDownloadBatch(b, "a linked job is busy; inspect saved jobs before continuing")
			return err
		}
		job, err := api.jobs.Get(ctx, id)
		job.DownloadGeneration = api.downloadGeneration
		if err == nil && job.State != "completed" {
			err = api.validateJob(ctx, job)
			if err == nil {
				err = api.runJob(ctx, job, false)
			}
		}
		guard.Close()
		if err != nil {
			api.failDownloadBatch(b, "file execution interrupted; use explicit individual job recovery, never automatically repeat the batch")
			return err
		}
	}
	current, err := api.jobs.GetDownloadBatch(ctx, b.ID)
	if err == nil {
		err = api.verifyDownloadBatchFiles(ctx, current)
	}
	if err != nil {
		api.failDownloadBatch(b, "completion proofs require review; downloader result was not changed")
		return err
	}
	return api.setDownloadBatch(ctx, b.ID, "executing", "files_done", "", "")
}

type downloadResultReview = organization.DownloadResultIntent

func (api organizationAPI) reviewDownloadResult(ctx context.Context, b organization.DownloadBatch, actor string) (downloadResultReview, error) {
	if err := api.downloadMonitorAuthorized(ctx); err != nil {
		return downloadResultReview{}, err
	}
	if b.State != "files_done" {
		return downloadResultReview{}, organization.ErrState
	}
	if err := api.verifyDownloadBatchFiles(ctx, b); err != nil {
		return downloadResultReview{}, err
	}
	c, err := api.completionOrganizationClient(ctx, b.Origin)
	if err != nil {
		return downloadResultReview{}, err
	}
	task, err := c.read(ctx, b.Origin.Task.ID)
	if err != nil || !organization.DownloadTaskMatches(b.Origin.Task, task, false) {
		return downloadResultReview{}, organization.ErrState
	}
	mode := ""
	for _, id := range b.Jobs {
		job, err := api.jobs.Get(ctx, id)
		if err != nil {
			return downloadResultReview{}, err
		}
		mode = job.Mode
	}
	review := downloadResultReview{BatchID: b.ID, Actor: actor, Operation: "add-organized-label", BeforeLabels: task.Labels, AfterLabels: append([]string{}, task.Labels...)}
	if organization.MoveMode(mode) {
		if !downloadTaskStopped(b.Origin.DownloaderType, task.State) {
			return review, organization.ErrState
		}
		review.Operation = "remove-task-keep-all-local-data"
	} else if b.Origin.DownloaderType == "aria2" {
		review.Operation = "remove-completed-result-keep-all-local-data"
	} else {
		review.AfterLabels = append(review.AfterLabels, "已整理")
		sort.Strings(review.AfterLabels)
		review.NeedsExclusiveLabels = b.Origin.DownloaderType == "transmission"
	}
	review.Fingerprint = organization.Digest(struct {
		Review downloadResultReview
		Origin organization.DownloadOrigin
		Jobs   []string
	}{review, b.Origin, b.Jobs})
	return review, nil
}
func (api organizationAPI) finalizeDownloadBatch(ctx context.Context, b organization.DownloadBatch, input downloadBatchRequest) error {
	review, err := api.reviewDownloadResult(ctx, b, input.actor)
	if err != nil {
		return err
	}
	if input.Fingerprint != review.Fingerprint || review.NeedsExclusiveLabels && !input.ConfirmExclusiveLabels {
		return organization.ErrState
	}
	if strings.HasPrefix(review.Operation, "remove-") && !input.ConfirmTaskRemoval {
		return organization.ErrState
	}
	if strings.HasPrefix(review.Operation, "remove-task") && !input.ConfirmTaskStopped {
		return organization.ErrState
	}
	c, err := api.completionOrganizationClient(ctx, b.Origin)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(review)
	if err != nil {
		return err
	}
	proof := string(raw)
	if err := api.setDownloadBatch(ctx, b.ID, "files_done", "result_pending", "result write intent persisted; ambiguous response requires readonly reconciliation", proof); err != nil {
		return err
	}
	if strings.HasPrefix(review.Operation, "remove-") {
		err = c.remove(ctx, b.Origin.Task.ID)
	} else {
		err = c.mark(ctx, b.Origin.Task.ID, review.AfterLabels)
	}
	if err != nil {
		return err
	}
	fresh, err := api.jobs.GetDownloadBatch(ctx, b.ID)
	if err != nil {
		return err
	}
	return api.reconcileDownloadResult(ctx, fresh)
}
func (api organizationAPI) reconcileDownloadResult(ctx context.Context, b organization.DownloadBatch) error {
	if b.State == "completed" {
		return nil
	}
	if b.State != "result_pending" {
		return organization.ErrState
	}
	var intent downloadResultReview
	if json.Unmarshal([]byte(b.ResultProof), &intent) != nil || intent.BatchID != b.ID || intent.Actor == "" {
		return organization.ErrState
	}
	fp := intent.Fingerprint
	intent.Fingerprint = ""
	if fp != organization.Digest(struct {
		Review downloadResultReview
		Origin organization.DownloadOrigin
		Jobs   []string
	}{intent, b.Origin, b.Jobs}) {
		return organization.ErrState
	}
	if err := api.verifyDownloadBatchFiles(ctx, b); err != nil {
		return err
	}
	c, err := api.completionOrganizationClient(ctx, b.Origin)
	if err != nil {
		return err
	}
	remove := b.Origin.DownloaderType == "aria2"
	for _, id := range b.Jobs {
		job, err := api.jobs.Get(ctx, id)
		if err != nil {
			return err
		}
		remove = remove || organization.MoveMode(job.Mode)
	}
	if remove {
		if !strings.HasPrefix(intent.Operation, "remove-") {
			return organization.ErrState
		}
		present, err := c.has(ctx, b.Origin.Task.ID)
		if err != nil {
			return err
		}
		if present {
			return organization.ErrState
		}
	} else {
		if intent.Operation != "add-organized-label" {
			return organization.ErrState
		}
		task, err := c.read(ctx, b.Origin.Task.ID)
		if err != nil {
			return err
		}
		if !organization.DownloadTaskMatches(b.Origin.Task, task, true) || !task.HasLabel("已整理") {
			return organization.ErrState
		}
		for _, label := range intent.AfterLabels {
			if !task.HasLabel(label) {
				return organization.ErrState
			}
		}
	}
	return api.setDownloadBatch(ctx, b.ID, "result_pending", "completed", "", b.ResultProof)
}

func (api organizationAPI) setDownloadBatch(ctx context.Context, id, from, to, reason, proof string) error {
	if err := api.downloadMonitorAuthorized(ctx); err != nil {
		return err
	}
	return api.jobs.SetDownloadBatchAuthorized(ctx, id, from, to, reason, proof, api.downloadGeneration)
}
