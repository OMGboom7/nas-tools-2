//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// A missing object is accepted only AFTER durable whole-job abandonment intent,
// to finish a previously interrupted cleanup. Before intent the owned payload
// and any saved witness must exist (full digest or exact source prefix). No
// recursive removal, source unlink, or target unlink.
func inspectUnpublished(ctx context.Context, d Definition, item Entry, p Proof, intent bool, beforeDelete func() error) error {
	if !SupportedMode(d.Mode) {
		return ErrMode
	}
	if p.Incomplete && !p.Anchor || p.Identity == "" || len(p.Digest) != 64 || p.ParentIdentity == "" || p.StagingIdentity == "" || filepath.Base(p.Temp) != p.Temp || !validRelative(p.Temp) || p.SourceHold != "" || p.SourceHoldIdentity != "" || p.SourceParentIdentity != "" {
		return ErrState
	}
	if beforeDelete != nil && !intent {
		return ErrState
	}
	if err := unpublishedBoundary(ctx, d, item, p); err != nil {
		return err
	}
	root, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, _, err := anchoredParent(root, item.Target, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), p.Temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) && intent {
		if beforeDelete != nil {
			if err = beforeDelete(); err != nil {
				return err
			}
		}
		if err = unpublishedBoundary(ctx, d, item, p); err != nil {
			return err
		}
		// Persist a directory removal that may have preceded a process crash.
		return parent.Sync()
	}
	if err != nil {
		return ErrState
	}
	stage := os.NewFile(uintptr(fd), p.Temp)
	defer stage.Close()
	if err = unpublishedStageBinding(parent, stage, p); err != nil {
		return err
	}
	names, err := stage.ReadDir(3)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	present := map[string]bool{}
	for _, entry := range names {
		if entry.Name() != "payload" && (entry.Name() != "anchor" || !p.Anchor) {
			return ErrState
		}
		present[entry.Name()] = true
	}
	if !intent && (!present["payload"] || p.Anchor && !present["anchor"]) || p.Anchor && present["payload"] && !present["anchor"] {
		return ErrState
	}
	refs := len(names)
	for _, name := range []string{"payload", "anchor"} {
		if present[name] {
			if err = verifyDisposableObject(ctx, d, item, p, stage, name, refs); err != nil {
				return err
			}
		}
	}
	if beforeDelete == nil {
		return nil
	}
	if err = beforeDelete(); err != nil {
		return err
	}
	if err = unpublishedBoundary(ctx, d, item, p); err != nil {
		return err
	}
	if err = unpublishedStageBinding(parent, stage, p); err != nil {
		return err
	}
	// Delete the writable name first, while the saved witness pins its inode.
	// On a crash, an anchor-only stage is still positive object evidence. Never
	// adopt a payload-only replacement after the last witness has disappeared.
	for _, name := range []string{"payload", "anchor"} {
		if !present[name] {
			continue
		}
		if err = beforeDelete(); err != nil {
			return err
		}
		if err = unpublishedBoundary(ctx, d, item, p); err != nil {
			return err
		}
		if err = unpublishedStageBinding(parent, stage, p); err != nil {
			return err
		}
		if err = verifyDisposableObject(ctx, d, item, p, stage, name, refs); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = unix.Unlinkat(fd, name, 0); err != nil {
			return err
		}
		refs--
		if err = stage.Sync(); err != nil {
			return err
		}
	}
	if err = stage.Sync(); err != nil {
		return err
	}
	if err = unpublishedBoundary(ctx, d, item, p); err != nil {
		return err
	}
	if err = unpublishedStageBinding(parent, stage, p); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// AT_REMOVEDIR refuses an unexpected/nonempty directory. Never recursively
	// delete extra files, even when this private directory has the expected inode.
	if err = unix.Unlinkat(int(parent.Fd()), p.Temp, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	if err = parent.Sync(); err != nil {
		return err
	}
	return unpublishedBoundary(ctx, d, item, p)
}

func unpublishedStageBinding(parent, stage *os.File, p Proof) error {
	info, err := stage.Stat()
	if err != nil || Identity(info) != p.StagingIdentity {
		return ErrState
	}
	var stat unix.Stat_t
	if unix.Fstat(int(stage.Fd()), &stat) != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0700 {
		return ErrState
	}
	named, err := lstatAt(parent, p.Temp)
	if err != nil || statIdentity(named) != p.StagingIdentity || named.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrState
	}
	return nil
}

func inspectDiscarded(ctx context.Context, d Definition, item Entry, p Proof) error {
	if !SupportedMode(d.Mode) || p.Identity == "" || len(p.Digest) != 64 || p.StagingIdentity == "" || p.ParentIdentity == "" || filepath.Base(p.Temp) != p.Temp || !validRelative(p.Temp) || p.SourceHold != "" || p.SourceHoldIdentity != "" || p.SourceParentIdentity != "" {
		return ErrState
	}
	if err := unpublishedBoundary(ctx, d, item, p); err != nil {
		return err
	}
	root, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, _, err := anchoredParent(root, item.Target, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	if _, err = lstatAt(parent, p.Temp); !errors.Is(err, unix.ENOENT) {
		return ErrState
	}
	return parent.Sync()
}

// Positive original-source evidence is required on EVERY cleanup/resume,
// including when a durable-intent stage is already gone. Target absence alone
// never proves a cancelled transfer, and equal-content replacement is rejected.
func unpublishedBoundary(ctx context.Context, d Definition, item Entry, p Proof) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return err
	}
	defer root.Close()
	parent, leaf, err := anchoredParent(root, item.Source, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	f, err := openRegular(parent, leaf)
	if err != nil {
		return ErrState
	}
	info, err := f.Stat()
	f.Close()
	if err != nil || !matches(info, item) || time.Since(info.ModTime()) < 30*time.Second {
		return ErrState
	}
	if err = verifyOriginal(ctx, parent, leaf, item, p.Digest); err != nil {
		return err
	}
	if err = validateSourceBinding(d, item); err != nil {
		return err
	}
	targetRoot, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return err
	}
	defer targetRoot.Close()
	targetParent, name, err := anchoredParent(targetRoot, item.Target, false)
	if err != nil {
		return err
	}
	defer targetParent.Close()
	info, err = targetParent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrPath
	}
	_, err = lstatAt(targetParent, name)
	if !errors.Is(err, unix.ENOENT) {
		return ErrClaimed
	}
	return nil
}
