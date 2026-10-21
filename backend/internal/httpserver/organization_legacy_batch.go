package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func (api organizationAPI) reserveRunLegacyPlan(ctx context.Context, p organizationPlan, actor string, sourceRemoval bool) (organization.Job, *organization.LegacyBatch, error) {
	if len(p.partitions) == 0 {
		id, err := api.jobs.Create(ctx, p.Fingerprint, p.definition)
		if err != nil {
			return organization.Job{}, nil, err
		}
		j, err := api.runLegacyRedo(ctx, id, sourceRemoval)
		return j, nil, err
	}
	b, err := api.jobs.CreateLegacyBatch(ctx, p.Fingerprint, actor, planDefinitions(p))
	if err != nil {
		return organization.Job{}, nil, err
	}
	err = api.runLegacyBatch(ctx, b, sourceRemoval)
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if current, e := api.jobs.GetLegacyBatch(read, b.ID); e == nil {
		b = current
	}
	var job organization.Job
	if len(b.Jobs) > 0 {
		job, _ = api.jobs.Get(read, b.Jobs[0])
	}
	return job, &b, err
}
func (api organizationAPI) runLegacyBatch(ctx context.Context, b organization.LegacyBatch, sourceRemoval bool) error {
	guard, err := api.jobs.AcquireJob(ctx, "legacy-batch:"+b.ID)
	if err != nil {
		return err
	}
	defer guard.Close()
	b, err = api.jobs.GetLegacyBatch(ctx, b.ID)
	if err != nil {
		return err
	}
	if b.State != "ready" || organization.MoveMode(b.Mode) && !sourceRemoval {
		return organization.ErrState
	}
	jobs, err := api.jobs.LegacyBatchJobs(ctx, b)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		for _, item := range j.Items {
			if item.State != "planned" {
				return organization.ErrState
			}
		}
		if err := api.validateJob(ctx, j); err != nil {
			return err
		}
	}
	if err = api.jobs.SetLegacyBatch(ctx, b.ID, "ready", "executing", "", b.ReviewFingerprint); err != nil {
		return err
	}
	for _, j := range jobs {
		jobGuard, e := api.jobs.AcquireJob(ctx, j.ID)
		if e == nil {
			e = api.validateJob(ctx, j)
			if e == nil {
				e = api.runJob(ctx, j, false)
			}
			jobGuard.Close()
		}
		if e != nil {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = api.jobs.SetLegacyBatch(persist, b.ID, "executing", "needs_review", "execution interrupted; inspect every original child, never replay the batch", "")
			cancel()
			return e
		}
	}
	err = api.reconcileLegacyBatch(ctx, b.ID)
	if err != nil {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = api.jobs.SetLegacyBatch(persist, b.ID, "executing", "needs_review", "complete-scope verification requires review; no automatic replay", "")
		cancel()
	}
	return err
}
func (api organizationAPI) reconcileLegacyBatch(ctx context.Context, id string) error {
	b, err := api.jobs.GetLegacyBatch(ctx, id)
	if err != nil {
		return err
	}
	if b.State == "completed" {
		return nil
	}
	if b.State != "ready" && b.State != "executing" && b.State != "needs_review" {
		return organization.ErrState
	}
	jobs, err := api.jobs.LegacyBatchJobs(ctx, b)
	if err != nil {
		return err
	}
	for n, j := range jobs {
		if !organization.LegacyFilesDone(j) {
			return fmt.Errorf("legacy child %d is unfinished: %w", n, organization.ErrState)
		}
		operation, closeRemote, e := api.remoteContext(ctx, j)
		if e != nil {
			return e
		}
		for _, item := range j.Items {
			if e = organization.VerifyCompletedTarget(operation, j, item); e != nil {
				closeRemote()
				return fmt.Errorf("legacy child %d target proof: %w", n, e)
			}
		}
		closeRemote()
	}
	if err = api.jobs.VerifyLegacyBatchScope(ctx, b, jobs); err != nil {
		return fmt.Errorf("legacy complete source scope: %w", err)
	}
	if err := api.jobs.FinishLegacyBatch(ctx, b, b.ReviewFingerprint, false); err != nil {
		return fmt.Errorf("legacy complete receipt: %w", err)
	}
	return nil
}
func (api organizationAPI) serveLegacyBatch(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	b, err := api.jobs.GetLegacyBatch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": b})
}
func (api organizationAPI) serveLegacyBatchAction(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input struct {
		Confirm                    bool   `json:"confirm"`
		ConfirmSourceRemoval       bool   `json:"confirmSourceRemoval"`
		ConfirmOldExecutorsStopped bool   `json:"confirmOldExecutorsStopped"`
		ConfirmRemoteExclusive     bool   `json:"confirmRemoteExclusive"`
		ConfirmRemoteRecoveryCopy  bool   `json:"confirmRemoteRecoveryCopy"`
		ReviewFingerprint          string `json:"reviewFingerprint"`
		Reason                     string `json:"reason"`
	}
	if !decodeOrganizationRequest(w, r, &input, "invalid multi-target batch action") {
		return
	}
	if !input.Confirm {
		writeAPIError(w, 400, 400, "separate explicit confirmation required")
		return
	}
	action := r.PathValue("action")
	if action != "execute" && action != "reconcile" && action != "stop" && action != "resume" && action != "close" {
		writeAPIError(w, 404, 404, "batch action not found")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	b, err := api.jobs.GetLegacyBatch(ctx, r.PathValue("id"))
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	if action == "stop" {
		if input.Reason == "" || len(input.Reason) > 512 {
			writeAPIError(w, 400, 400, "a stop reason is required; this revokes future writes, not an atomic rollback")
			return
		}
		if b.State != "closing" {
			err = api.jobs.SetLegacyBatch(ctx, b.ID, b.State, "closing", claims.Username+": "+input.Reason, "")
		}
	} else if action == "execute" {
		if input.ReviewFingerprint != b.ReviewFingerprint || !input.ConfirmOldExecutorsStopped {
			writeAPIError(w, 400, 400, "fresh full batch review and stopped-writer confirmation required")
			return
		}
		if organization.RemoteMode(b.Mode) {
			if err = api.approveLegacyRemote(claims.Username, "legacy-batch", input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy, input.ConfirmOldExecutorsStopped); err != nil {
				writeAPIError(w, 400, 400, "separate remote consent required")
				return
			}
		}
		err = api.runLegacyBatch(ctx, b, input.ConfirmSourceRemoval)
	} else {
		guard, e := api.jobs.AcquireJob(ctx, "legacy-batch:"+b.ID)
		if e != nil {
			writeOrganizationError(w, e)
			return
		}
		defer guard.Close()
		b, err = api.jobs.GetLegacyBatch(ctx, b.ID)
		if err != nil {
			writeOrganizationError(w, err)
			return
		}
		if action == "reconcile" {
			err = api.reconcileLegacyBatch(ctx, b.ID)
		} else {
			if input.ReviewFingerprint != b.ReviewFingerprint || !input.ConfirmOldExecutorsStopped || input.Reason == "" || len(input.Reason) > 512 {
				writeAPIError(w, 400, 400, "fresh batch review, stopped writers and a reason required")
				return
			}
			if b.State != "closing" {
				writeAPIError(w, 409, 409, "stop the whole batch before disposition or reauthorization")
				return
			}
			jobs, e := api.jobs.LegacyBatchJobs(ctx, b)
			if e != nil {
				writeOrganizationError(w, e)
				return
			}
			if action == "resume" {
				for _, j := range jobs {
					if j.State == "cancelled" || j.State == "abandoned" || j.State == "abandoning" || j.State == organization.SourceRestoring || j.State == organization.SourceRestored || j.State == "remote_closed" {
						err = organization.ErrState
						break
					}
					if e := api.validateJobConfiguration(ctx, j); e != nil {
						err = e
						break
					}
				}
				if err == nil && organization.RemoteMode(b.Mode) {
					err = api.approveLegacyRemote(claims.Username, "legacy-batch", input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy, input.ConfirmOldExecutorsStopped)
				}
				if err == nil {
					err = api.jobs.SetLegacyBatch(ctx, b.ID, "closing", "needs_review", claims.Username+": "+input.Reason, b.ReviewFingerprint)
				}
			} else {
				// Closing cancels only never-started children. Other unfinished
				// objects require their existing proof-based disposition flows.
				for _, j := range jobs {
					planned := len(j.Items) > 0
					for _, item := range j.Items {
						planned = planned && item.State == "planned"
					}
					if planned {
						g, e := api.jobs.AcquireJob(ctx, j.ID)
						if e != nil {
							err = e
							break
						}
						e = api.jobs.Cancel(ctx, j.ID)
						g.Close()
						if e != nil {
							err = e
							break
						}
					}
				}
				if err == nil {
					b, err = api.jobs.GetLegacyBatch(ctx, b.ID)
				}
				if err == nil {
					err = api.jobs.FinishLegacyBatch(ctx, b, b.ReviewFingerprint, true)
				}
			}
		}
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	b, err = api.jobs.GetLegacyBatch(ctx, b.ID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": b})
}
