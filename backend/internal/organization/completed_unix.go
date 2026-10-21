//go:build linux || darwin

package organization

import (
	"context"
	"path/filepath"
)

// VerifyCompletedTarget is a readonly recheck of an already committed item,
// after its staging anchor has legitimately been cleaned up. Never use it to
// adopt an uncertain/prepared publication or infer source removal from absence.
func VerifyCompletedTarget(ctx context.Context, job Job, item JobItem) error {
	if RemoteMode(job.Mode) {
		if item.State != "completed" || item.Reason != "" || item.Proof.Remote == nil || !item.Proof.Remote.TargetVerified {
			return ErrState
		}
		c, err := remoteClient(ctx, job.Definition)
		if err != nil {
			return err
		}
		return VerifyRemoteTarget(ctx, c, job.Definition, item.Entry, item.Proof, true)
	}
	if IsDisc(item.Entry) {
		if item.State != "completed" || item.Reason != "" {
			return ErrState
		}
		return verifyDiscPublished(ctx, job.Definition, item.Entry, item.Proof, false, true)
	}
	d := job.Definition
	p := item.Proof
	if item.State != "completed" || item.Reason != "" || p.Incomplete || p.Identity == "" || len(p.Digest) != 64 || p.ParentIdentity == "" || !SupportedMode(d.Mode) {
		return ErrState
	}
	root, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, name, err := anchoredParent(root, item.Target, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := parent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrState
	}
	if d.Mode == "softlink" {
		if p.Kind != "symlink" || p.LinkTarget != filepath.Join(d.SourceRoot, item.Source) {
			return ErrState
		}
		if err := verifyLinkObject(parent, name, p); err != nil {
			return err
		}
		source, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
		if err != nil {
			return err
		}
		defer source.Close()
		sp, sn, err := anchoredParent(source, item.Source, false)
		if err != nil {
			return err
		}
		defer sp.Close()
		if err := verifyOriginal(ctx, sp, sn, item.Entry, p.Digest); err != nil {
			return err
		}
		if err := validateSourceBinding(d, item.Entry); err != nil {
			return err
		}
	} else {
		if p.Kind != "" && p.Kind != "regular" || p.LinkTarget != "" {
			return ErrState
		}
		f, err := openRegular(parent, name)
		if err != nil {
			return err
		}
		defer f.Close()
		before, err := f.Stat()
		if err != nil || Identity(before) != p.Identity || before.Size() != item.Size {
			return ErrState
		}
		if d.Mode == "link" && (!matches(before, item.Entry) || p.Identity != item.Identity) {
			return ErrState
		}
		digest, err := hashSource(ctx, f, item.Size)
		if err != nil {
			return err
		}
		after, err := f.Stat()
		if err != nil || Identity(after) != p.Identity || after.Size() != item.Size || !before.ModTime().Equal(after.ModTime()) || digest != p.Digest {
			return ErrState
		}
		if d.Mode == "link" {
			if err := validateSourceBinding(d, item.Entry); err != nil {
				return err
			}
		}
	}
	return validateDestinationBinding(d, item.Entry, p)
}
