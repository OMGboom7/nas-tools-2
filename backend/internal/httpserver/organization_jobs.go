package httpserver

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func (api organizationAPI) jobsAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if !api.authorize(w, r) {
		return false
	}
	if !api.pureGo || api.jobs == nil {
		writeAPIError(w, 501, 501, "native copy execution requires the Python backend to be disabled")
		return false
	}
	return true
}
func writeOrganizationError(w http.ResponseWriter, err error) {
	status, message := 503, "organization journal is unavailable"
	if errors.Is(err, organization.ErrNotFound) {
		status, message = 404, "organization job not found"
	}
	if errors.Is(err, organization.ErrState) || errors.Is(err, organization.ErrClaimed) || errors.Is(err, organization.ErrPath) {
		status, message = 409, "organization state or configuration changed; review the job before any retry"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status, message = 504, "organization operation was interrupted; inspect its saved state"
	}
	writeAPIError(w, status, status, message)
}
func (api organizationAPI) serveJobs(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	jobs, err := api.jobs.List(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": jobs})
}
func (api organizationAPI) serveJob(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	job, err := api.jobs.Get(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": job})
}
func (api organizationAPI) serveCreateJob(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input struct {
		organizationPlanInput
		Fingerprint string `json:"fingerprint"`
	}
	if !decodeServiceRequest(w, r, &input, "invalid organization job request") {
		return
	}
	if input.Mode != "copy" || len(input.Fingerprint) != 64 {
		writeAPIError(w, 400, 400, "copy mode and the reviewed preview fingerprint are required")
		return
	}
	if input.Path == "" {
		input.Path = "."
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	plan, failure := api.plan(ctx, input.organizationPlanInput)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	if plan.Fingerprint != input.Fingerprint {
		writeAPIError(w, 409, 409, "preview changed; generate and review a fresh preview")
		return
	}
	if len(plan.definition.Entries) == 0 {
		writeAPIError(w, 422, 422, "preview contains no available files")
		return
	}
	id, err := api.jobs.Create(ctx, plan.Fingerprint, plan.definition)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	job, err := api.jobs.Get(ctx, id)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"code": 0, "success": true, "data": job})
}

// Revalidate admin configuration before any write, not just the opaque root IDs
// supplied by the browser. Stored destinations never come from client payloads.
func (api organizationAPI) validateJob(ctx context.Context, job organization.Job) error {
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return err
	}
	if organization.Digest(snapshot) != job.Definition.ConfigDigest {
		return organization.ErrState
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return err
	}
	find := func(list []organizationRoot, id, path string) bool {
		for _, root := range list {
			if root.ID == id {
				canonical, err := filepath.EvalSymlinks(root.Path)
				return err == nil && canonical == path
			}
		}
		return false
	}
	if !find(roots.Sources, job.Definition.SourceID, job.Definition.SourceRoot) || !find(roots.Targets, job.Definition.TargetID, job.Definition.TargetRoot) {
		return organization.ErrState
	}
	return nil
}

func (api organizationAPI) serveJobAction(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	action := r.PathValue("action")
	if action != "execute" && action != "reconcile" && action != "cancel" {
		writeAPIError(w, 404, 404, "organization action not found")
		return
	}
	// Execution requires an explicit confirmation of stable, completed source
	// files. This is a manual action, not proof from the downloader API.
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeServiceRequest(w, r, &input, "invalid organization confirmation") {
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, 400, "explicit confirmation is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	job, err := api.jobs.Get(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if action == "cancel" {
		err = api.jobs.Cancel(ctx, job.ID)
	} else {
		err = api.validateJob(ctx, job)
		if err == nil {
			err = api.runJob(ctx, job, action == "reconcile")
		}
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	job, err = api.jobs.Get(ctx, job.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": job})
}

func (api organizationAPI) runJob(ctx context.Context, job organization.Job, reconcile bool) error {
	if job.Definition.Mode != "copy" || job.State == "cancelled" {
		return organization.ErrState
	}
	for _, item := range job.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if item.State == "completed" {
			continue
		}
		if reconcile {
			if item.State != "prepared" {
				continue
			}
			if err := organization.VerifyPublished(ctx, job.Definition, item.Entry, item.Proof); err != nil {
				continue
			}
			if err := api.jobs.Complete(ctx, job, item); err != nil {
				return err
			}
			_ = organization.Cleanup(job.Definition, item.Entry, item.Proof)
			continue
		}
		// An uncertain item is held forever, never re-copied because a request
		// disconnected or a process restarted. Only positive proof completes it.
		if item.State != "planned" {
			return organization.ErrState
		}
		claimed, err := api.jobs.Claim(ctx, job.ID, item.Index)
		if err != nil {
			return err
		}
		if !claimed {
			return organization.ErrState
		}
		proof, err := organization.Copy(ctx, job.Definition, item.Entry, organization.TempName(job.ID, item.Index), func(p organization.Proof) error {
			if err := api.validateJob(ctx, job); err != nil {
				return err
			}
			return api.jobs.Prepare(ctx, job.ID, item.Index, p)
		})
		if err == nil {
			item.Proof = proof
			err = organization.VerifyPublished(ctx, job.Definition, item.Entry, proof)
		}
		if err == nil {
			err = api.jobs.Complete(ctx, job, item)
		}
		if err != nil {
			persist, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = api.jobs.RecordError(persist, job.ID, item.Index)
			persistCancel()
			return err
		}
		_ = organization.Cleanup(job.Definition, item.Entry, proof)
	}
	return nil
}
