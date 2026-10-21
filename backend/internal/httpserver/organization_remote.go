package httpserver

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

type remoteMonitorTarget struct {
	TargetID string               `json:"targetId"`
	Path     string               `json:"path"`
	Remote   *remotestore.Summary `json:"remote"`
}

func logicalRemoteRoot(root string) (string, error) {
	if !filepath.IsAbs(root) || len(root) > 4096 || filepath.Clean(root) == "/" || strings.ContainsAny(root, "\\\x00\r\n") {
		return "", organization.ErrPath
	}
	return filepath.Clean(root), nil
}
func (api organizationAPI) monitorRemoteRoot(ctx context.Context, root organizationRoot, expected *organization.RemoteAutomation) (string, remoteMonitorTarget, organization.RemoteProfileSeal, error) {
	var seal organization.RemoteProfileSeal
	public := remoteMonitorTarget{TargetID: root.ID, Path: root.Path}
	c, err := api.rclone.Open(nil)
	if err != nil || !api.pureGo {
		return "", public, seal, organization.ErrMode
	}
	defer c.Close()
	if expected == nil {
		if err := c.Probe(ctx, root.Path); err != nil {
			return "", public, seal, err
		}
		b := c.Binding()
		seal = organization.RemoteProfileSeal{Digest: organization.Digest(b), Directories: b.Directories}
		public.Remote = b.Summary()
	} else {
		wanted, ok := expected.Profiles[root.ID]
		if !ok {
			return "", public, seal, organization.ErrState
		}
		b := c.Binding()
		b.Directories = wanted.Directories
		seal = organization.RemoteProfileSeal{Digest: organization.Digest(b), Directories: b.Directories}
		public.Remote = b.Summary()
		if seal != wanted {
			return "", public, seal, organization.ErrState
		}
	}
	return "remote:" + root.ID + ":" + strconv.FormatBool(seal.Directories) + ":" + seal.Digest, public, seal, nil
}
func remoteAutomationFromRoots(roots []string) *organization.RemoteAutomation {
	var grant *organization.RemoteAutomation
	for _, root := range roots {
		if !strings.HasPrefix(root, "remote:") {
			continue
		}
		parts := strings.Split(root, ":")
		if len(parts) != 4 {
			continue
		}
		dirs, err := strconv.ParseBool(parts[2])
		if err != nil {
			continue
		}
		if grant == nil {
			grant = &organization.RemoteAutomation{Protocol: 1, Profiles: map[string]organization.RemoteProfileSeal{}}
		}
		grant.Profiles[parts[1]] = organization.RemoteProfileSeal{Digest: parts[3], Directories: dirs}
	}
	return grant
}
func remoteTargets(roots organizationRoots, grant *organization.RemoteAutomation) []remoteMonitorTarget {
	result := []remoteMonitorTarget{}
	if grant == nil {
		return result
	}
	for _, root := range roots.Targets {
		if profile, ok := grant.Profiles[root.ID]; ok {
			result = append(result, remoteMonitorTarget{TargetID: root.ID, Path: root.Path, Remote: (remotestore.Binding{Driver: "rclone", Directories: profile.Directories}).Summary()})
		}
	}
	return result
}
func approveRemoteAutomation(grant *organization.RemoteAutomation, exclusive, recovery bool) error {
	if grant == nil {
		return nil
	}
	if !exclusive || !recovery {
		return organization.ErrState
	}
	grant.Exclusive, grant.RecoveryCopy = true, true
	grant.ApprovedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if !grant.Valid() {
		return organization.ErrState
	}
	return nil
}

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

func (api organizationAPI) resolveUnknown(ctx context.Context, job organization.Job) error {
	if job.Definition.Unknown == nil || job.Definition.Remote == nil {
		return api.jobs.ResolveUnknown(ctx, job)
	}
	current, err := api.jobs.Get(ctx, job.ID)
	if err != nil {
		return err
	}
	if current.UnknownState == "resolved" || current.UnknownState == "preserved" || current.UnknownState == "detached" {
		return nil
	}
	for _, item := range current.Items {
		if item.State != "completed" || item.Reason != "" {
			return nil
		}
	}
	ctx, closeRemote, err := api.remoteContext(ctx, current)
	if err != nil {
		return err
	}
	defer closeRemote()
	return api.jobs.ResolveUnknown(ctx, current)
}

func (api *organizationAPI) approveLegacyRemote(actor, origin string, exclusive, recovery, stopped bool) error {
	if actor == "" || !exclusive || !recovery || !stopped || !api.pureGo || !api.rclone.Available() {
		return organization.ErrState
	}
	api.remoteApproval = organization.RemoteApproval{Actor: actor, Origin: origin, Created: time.Now().UTC().Format(time.RFC3339Nano), Exclusive: true, RecoveryCopy: true}
	return nil
}

// Explicit requests or independently authorized monitor leases only. A lost
// running/staging receipt is never re-uploaded; publication recovery uses the
// retained, fully hashed private remote copy.
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
	if api.monitorGeneration != "" {
		ctx = organization.AutomaticRemoteContext(ctx, "sync-monitor", api.monitorGeneration)
	}
	if api.downloadGeneration != "" {
		ctx = organization.AutomaticRemoteContext(ctx, "download-monitor", api.downloadGeneration)
	}
	ctx = organization.RemoteContext(ctx, c, job.Definition)
	operationCtx := ctx
	ctx = organization.RemoteOperationContext(ctx, func() error { return api.validateJob(operationCtx, job) })
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
