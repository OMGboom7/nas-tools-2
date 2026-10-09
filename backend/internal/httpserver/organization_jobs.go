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
		writeAPIError(w, 501, 501, "native organization execution requires the Python backend to be disabled")
		return false
	}
	return true
}
func writeOrganizationError(w http.ResponseWriter, err error) {
	status, message := 503, "organization operation failed; inspect the saved job state"
	if errors.Is(err, organization.ErrNotFound) {
		status, message = 404, "organization job not found"
	}
	if errors.Is(err, organization.ErrState) || errors.Is(err, organization.ErrClaimed) || errors.Is(err, organization.ErrPath) {
		status, message = 409, "organization state or configuration changed; review the job before any retry"
	}
	if errors.Is(err, organization.ErrMode) {
		status, message = 422, "selected organization mode is unavailable on this filesystem or with current permissions; no mode fallback was performed"
	}
	if errors.Is(err, organization.ErrBusy) {
		status, message = 409, "organization operation is already active; refresh its saved state instead of retrying execution"
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
	if !organization.SupportedMode(input.Mode) || len(input.Fingerprint) != 64 {
		writeAPIError(w, 400, 400, "a supported organization mode and the reviewed preview fingerprint are required")
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
	if action != "execute" && action != "reconcile" && action != "cancel" && action != "resume-move" && action != "resume-publication" {
		writeAPIError(w, 404, 404, "organization action not found")
		return
	}
	// Execution requires an explicit confirmation of stable, completed source
	// files. This is a manual action, not proof from the downloader API.
	var input struct {
		Confirm              bool `json:"confirm"`
		ConfirmSourceRemoval bool `json:"confirmSourceRemoval"`
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
	if action == "resume-move" && job.Definition.Mode != "move" {
		writeOrganizationError(w, organization.ErrState)
		return
	}
	if job.Definition.Mode == "move" && (action == "execute" || action == "resume-move" || action == "resume-publication") && !input.ConfirmSourceRemoval {
		writeAPIError(w, 400, 400, "separate source-removal confirmation is required for moving files")
		return
	}
	guard, err := api.jobs.AcquireJob(ctx, job.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	defer guard.Close()
	// The first read only established consent requirements. Reload under the
	// guard so a competing completed/cancelled operation cannot leave stale state.
	job, err = api.jobs.Get(ctx, job.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if action == "cancel" {
		err = api.jobs.Cancel(ctx, job.ID)
	} else {
		err = api.validateJob(ctx, job)
		if err == nil {
			if action == "resume-move" {
				err = api.resumeMove(ctx, job)
			} else if action == "resume-publication" {
				err = api.resumePublication(ctx, job)
			} else {
				err = api.runJob(ctx, job, action == "reconcile")
			}
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
	if !organization.SupportedMode(job.Definition.Mode) || job.State == "cancelled" {
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
			// A published target alone does not prove source disposition. This
			// action must never rename or remove any source/recovery objects.
			if job.Definition.Mode == "move" {
				continue
			}
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
		prepare := func(p organization.Proof) error {
			if err := api.validateJob(ctx, job); err != nil {
				return err
			}
			return api.jobs.Prepare(ctx, job.ID, item.Index, p)
		}
		transfer := organization.Transfer
		if job.Definition.Mode == "move" {
			transfer = organization.PrepareMoveTarget
		}
		proof, err := transfer(ctx, job.Definition, item.Entry, organization.TempName(job.ID, item.Index), prepare)
		if err == nil {
			item.Proof = proof
			if job.Definition.Mode == "move" {
				item.State = "prepared"
				err = api.finishMove(ctx, job, item)
			} else {
				err = organization.VerifyPublished(ctx, job.Definition, item.Entry, proof)
			}
		}
		if err == nil && job.Definition.Mode != "move" {
			err = api.jobs.Complete(ctx, job, item)
		}
		if err != nil {
			persist, persistCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = api.jobs.RecordError(persist, job.ID, item.Index)
			persistCancel()
			return err
		}
		if job.Definition.Mode != "move" {
			_ = organization.Cleanup(job.Definition, item.Entry, proof)
		}
	}
	return nil
}

func (api organizationAPI) finishMove(ctx context.Context, job organization.Job, item organization.JobItem) error {
	if err := api.validateJob(ctx, job); err != nil {
		return err
	}
	if item.State == "prepared" {
		p, err := organization.PrepareMove(ctx, job.Definition, item.Entry, item.Proof, organization.MoveHoldName(job.ID, item.Index))
		if err != nil {
			return err
		}
		if err = api.jobs.BeginMove(ctx, job.ID, item.Index, p); err != nil {
			return err
		}
		item.Proof, item.State = p, "moving"
	}
	if item.State != "moving" && item.State != "quarantined" {
		return organization.ErrState
	}
	if err := organization.ContinueMove(ctx, job.Definition, item.Entry, item.Proof, item.State == "quarantined", func() error {
		if err := api.validateJob(ctx, job); err != nil {
			return err
		}
		return api.jobs.MarkQuarantined(ctx, job.ID, item.Index, item.Proof)
	}); err != nil {
		return err
	}
	if err := api.validateJob(ctx, job); err != nil {
		return err
	}
	if err := api.jobs.Complete(ctx, job, item); err != nil {
		return err
	}
	// Reload the receipt, including when another confirmed request committed
	// history first. Never infer cleanup from a stale in-memory item.
	current, err := api.jobs.Get(ctx, job.ID)
	if err != nil {
		return err
	}
	return api.cleanupMove(ctx, current, current.Items[item.Index])
}

func (api organizationAPI) cleanupMove(ctx context.Context, job organization.Job, item organization.JobItem) error {
	if item.State != "completed" {
		return organization.ErrState
	}
	if item.Reason == "" {
		return nil
	}
	if err := api.validateJob(ctx, job); err != nil {
		return err
	}
	if item.Reason == organization.MoveSourceCleanupPending {
		if err := organization.CleanupMovedSource(ctx, job.Definition, item.Entry, item.Proof); err != nil {
			return err
		}
		if err := api.jobs.AdvanceMoveCleanup(ctx, job.ID, item.Index, item.Proof, true); err != nil {
			return err
		}
		item.Reason = organization.MoveTargetCleanupPending
	}
	if item.Reason != organization.MoveTargetCleanupPending {
		return organization.ErrState
	}
	if err := organization.CleanupMoveTarget(ctx, job.Definition, item.Entry, item.Proof); err != nil {
		return err
	}
	return api.jobs.AdvanceMoveCleanup(ctx, job.ID, item.Index, item.Proof, false)
}

func (api organizationAPI) resumeMove(ctx context.Context, job organization.Job) error {
	if job.Definition.Mode != "move" || job.State == "cancelled" {
		return organization.ErrState
	}
	for _, item := range job.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.State == "planned" {
			// Never start new items from recovery.
			continue
		}
		var err error
		if item.State == "completed" {
			err = api.cleanupMove(ctx, job, item)
		} else {
			err = api.finishMove(ctx, job, item)
		}
		if err != nil {
			persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = api.jobs.RecordError(persist, job.ID, item.Index)
			cancel()
			return err
		}
	}
	return nil
}

func (api organizationAPI) resumePublication(ctx context.Context, job organization.Job) error {
	if !organization.SupportedMode(job.Definition.Mode) || job.State == "cancelled" {
		return organization.ErrState
	}
	for _, item := range job.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.State == "planned" || item.State == "completed" {
			continue
		}
		if item.State != "prepared" {
			return organization.ErrState
		}
		err := organization.PublishPrepared(ctx, job.Definition, item.Entry, item.Proof, func() error {
			if err := api.validateJob(ctx, job); err != nil {
				return err
			}
			return api.jobs.VerifyPrepared(ctx, job.ID, item.Index, item.Proof)
		})
		if err == nil {
			if job.Definition.Mode == "move" {
				var current organization.Job
				current, err = api.jobs.Get(ctx, job.ID)
				if err == nil {
					latest := current.Items[item.Index]
					if latest.State == "completed" {
						err = api.cleanupMove(ctx, current, latest)
					} else {
						err = api.finishMove(ctx, current, latest)
					}
				}
			} else {
				if err = api.validateJob(ctx, job); err == nil {
					err = api.jobs.Complete(ctx, job, item)
				}
				if err == nil {
					_ = organization.Cleanup(job.Definition, item.Entry, item.Proof)
				}
			}
		}
		if err != nil {
			persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = api.jobs.RecordError(persist, job.ID, item.Index)
			cancel()
			return err
		}
	}
	return nil
}
