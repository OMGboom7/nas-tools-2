package httpserver

import (
	"context"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func (api organizationAPI) remoteRoot(ctx context.Context, root string) (*remotestore.Client, error) {
	if !api.pureGo {
		return nil, organization.ErrMode
	}
	c, err := api.rclone.Open(nil)
	if err != nil {
		return nil, err
	}
	if err := c.Probe(ctx, root); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func organizationTargetStatus(ctx context.Context, root, relative string, c *remotestore.Client) (string, error) {
	if c != nil {
		return organization.RemoteTargetStatus(ctx, c, root, relative)
	}
	return organization.TargetStatus(ctx, root, relative)
}
func (api organizationAPI) remoteContext(ctx context.Context, job organization.Job) (context.Context, func(), error) {
	if job.Definition.Remote == nil {
		return ctx, func() {}, nil
	}
	guard, err := api.jobs.AcquireJob(ctx, "rclone-namespace:NASTOOL")
	if err != nil {
		return ctx, func() {}, err
	}
	c, err := api.rclone.Open(job.Definition.Remote)
	if err != nil {
		guard.Close()
		return ctx, func() {}, err
	}
	return organization.RemoteContext(ctx, c, job.Definition), func() { _ = c.Close(); guard.Close() }, nil
}

// Explicit requests only. A lost running/staging receipt is never re-uploaded;
// publication recovery uses the retained, fully hashed private remote copy.
func (api organizationAPI) runRemoteJob(ctx context.Context, job organization.Job, action string) error {
	if !api.remoteApproval.Valid() || job.Remote == nil || job.State != "ready" && job.State != "needs_review" && job.State != "completed" {
		return organization.ErrState
	}
	guard, err := api.jobs.AcquireJob(ctx, "rclone-namespace:NASTOOL")
	if err != nil {
		return err
	}
	defer guard.Close()
	c, err := api.rclone.Open(job.Definition.Remote)
	if err != nil {
		return err
	}
	defer c.Close()
	ctx = organization.RemoteContext(ctx, c, job.Definition)
	if action == "resume-move" {
		return api.resumeMove(ctx, job)
	}
	for _, item := range job.Items {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.State == "completed" {
			continue
		}
		if action == "reconcile" && organization.MoveMode(job.Mode) {
			continue
		}
		if action != "execute" && item.State == "planned" {
			continue
		}
		err := api.remoteItem(ctx, c, job, item, action)
		if err != nil {
			persist, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = api.jobs.RecordError(persist, job.ID, item.Index)
			cancel()
			return err
		}
	}
	return nil
}
func remoteProofCopy(p organization.Proof) organization.Proof {
	if p.Remote != nil {
		r := *p.Remote
		p.Remote = &r
	}
	return p
}
func (api organizationAPI) remoteItem(ctx context.Context, c *remotestore.Client, job organization.Job, item organization.JobItem, action string) error {
	current := func() error {
		if err := api.validateJob(ctx, job); err != nil {
			return err
		}
		return api.jobs.VerifyRemotePhase(ctx, job.ID, item.Index, item.Proof, item.State)
	}
	if action == "execute" {
		if item.State != "planned" {
			return organization.ErrState
		}
		if err := api.validateJob(ctx, job); err != nil {
			return err
		}
		claimed, err := api.jobs.Claim(ctx, job.ID, item.Index)
		if err != nil {
			return err
		}
		if !claimed {
			return organization.ErrState
		}
		item.Proof, err = organization.BuildRemoteProof(ctx, job.Definition, item.Entry, organization.TempName(job.ID, item.Index), api.remoteApproval)
		if err != nil {
			return err
		}
		if err := api.validateJob(ctx, job); err != nil {
			return err
		}
		if err := api.jobs.Stage(ctx, job.ID, item.Index, item.Proof); err != nil {
			return err
		}
		item.State = "staging"
		if err := organization.UploadRemoteStage(ctx, c, job.Definition, item.Entry, item.Proof, current); err != nil {
			return err
		}
		if err := current(); err != nil {
			return err
		}
		prepared := remoteProofCopy(item.Proof)
		prepared.Incomplete, prepared.Remote.StageReady = false, true
		if err := api.jobs.PrepareRemote(ctx, job.ID, item.Index, prepared); err != nil {
			return err
		}
		item.State, item.Proof = "prepared", prepared
	}
	if item.State != "prepared" && item.State != "publishing" {
		return organization.ErrState
	}
	if item.Proof.Remote == nil || item.Proof.Incomplete || !item.Proof.Remote.StageReady {
		return organization.ErrState
	}
	if action == "reconcile" && !item.Proof.Remote.PublicationIntent {
		return nil
	}
	if err := current(); err != nil {
		return err
	}
	if !item.Proof.Remote.PublicationIntent {
		// Initial publication must not adopt even identical existing data.
		status, err := organization.RemoteTargetStatus(ctx, c, job.TargetRoot, item.Target)
		if err != nil {
			return err
		}
		if status != "available" {
			return organization.ErrState
		}
		intent := remoteProofCopy(item.Proof)
		intent.Remote.PublicationIntent = true
		if err := current(); err != nil {
			return err
		}
		if err := api.jobs.BeginRemotePublish(ctx, job.ID, item.Index, intent); err != nil {
			return err
		}
		item.State, item.Proof = "publishing", intent
	}
	if !item.Proof.Remote.TargetVerified {
		if action == "reconcile" {
			if err := organization.VerifyRemotePublication(ctx, c, job.Definition, item.Entry, item.Proof); err != nil {
				return err
			}
		} else if err := organization.PublishRemote(ctx, c, job.Definition, item.Entry, item.Proof, current); err != nil {
			return err
		}
		if err := current(); err != nil {
			return err
		}
		verified := remoteProofCopy(item.Proof)
		verified.Remote.TargetVerified = true
		if err := api.jobs.FinishRemotePublish(ctx, job.ID, item.Index, verified); err != nil {
			return err
		}
		item.State, item.Proof = "prepared", verified
	}
	if err := organization.VerifyRemotePublication(ctx, c, job.Definition, item.Entry, item.Proof); err != nil {
		return err
	}
	if err := current(); err != nil {
		return err
	}
	if organization.MoveMode(job.Mode) {
		return api.finishMove(ctx, job, item)
	}
	return api.jobs.Complete(ctx, job, item)
}
