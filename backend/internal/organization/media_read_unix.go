//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func ExistingSidecar(ctx context.Context, job Job, item JobItem, scope, name string) (bool, error) {
	if ctx.Err() != nil || job.State != "completed" {
		return false, ErrState
	}
	target, err := sidecarTarget(item, scope, name)
	if err != nil {
		return false, err
	}
	if RemoteMode(job.Mode) {
		c, err := remoteClient(ctx, job.Definition)
		if err != nil {
			return false, err
		}
		return c.Existing(ctx, job.TargetRoot, target)
	}
	if err := validateDestinationBinding(job.Definition, item.Entry, item.Proof); err != nil {
		return false, err
	}
	root, err := anchoredRoot(job.Definition.TargetRoot, job.Definition.TargetIdentity)
	if err != nil {
		return false, err
	}
	defer root.Close()
	parent, leaf, err := anchoredParent(root, target, false)
	if err != nil {
		return false, err
	}
	defer parent.Close()
	var st unix.Stat_t
	err = unix.Fstatat(int(parent.Fd()), leaf, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return err == nil, err
}

// ReadCompletedMedia exposes one proven file to a readonly consumer. Symlink
// intake pins the original approved source instead of following an arbitrary
// target link; current content/path bindings must survive the entire read.
func ReadCompletedMedia(ctx context.Context, job Job, item JobItem, consume func(*os.File) ([]byte, error)) ([]byte, error) {
	// Remote sources have a separate content-bound range capability. They must
	// never enter this pinned-local-descriptor path.
	if RemoteMode(job.Mode) {
		return nil, ErrMode
	}
	if job.State != "completed" || item.Kind != "media" || consume == nil {
		return nil, ErrState
	}
	// No unverified STREAM clip is selected as a representative episode.
	if IsDisc(item.Entry) {
		return nil, ErrMode
	}
	stamp, err := sidecarMediaStamp(ctx, job, item)
	if err != nil {
		return nil, err
	}
	if err := VerifyCompletedTarget(ctx, job, item); err != nil {
		return nil, err
	}
	if after, err := sidecarMediaStamp(ctx, job, item); err != nil || stamp != after {
		return nil, ErrState
	}
	d := job.Definition
	rootPath, identity, relative, expected := d.TargetRoot, d.TargetIdentity, item.Target, item.Proof.Identity
	if d.Mode == "softlink" {
		rootPath, identity, relative, expected = d.SourceRoot, d.SourceIdentity, item.Source, item.Identity
	}
	root, err := anchoredRoot(rootPath, identity)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parent, leaf, err := anchoredParent(root, relative, false)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	f, err := openRegular(parent, leaf)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || Identity(info) != expected || info.Size() != item.Size {
		return nil, ErrState
	}
	data, err := consume(f)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if after, err := sidecarMediaStamp(ctx, job, item); err != nil || stamp != after {
		return nil, ErrState
	}
	return data, nil
}
