//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// PublishPrepared continues a saved, complete object, never re-copies data,
// removes sources, overwrites a target, or adopts an unjournaled stage. The
// callback revalidates the prepared ledger immediately before publication.
func PublishPrepared(ctx context.Context, d Definition, item Entry, p Proof, beforePublish func() error) error {
	if p.Incomplete {
		return ErrState
	}
	if !SupportedMode(d.Mode) {
		return ErrMode
	}
	if beforePublish == nil || p.Identity == "" || len(p.Digest) != 64 || p.ParentIdentity == "" || p.StagingIdentity == "" || !strings.HasPrefix(p.Temp, ".nastool-copy-") || filepath.Base(p.Temp) != p.Temp || !validRelative(p.Temp) || p.SourceHold != "" || p.SourceHoldIdentity != "" || p.SourceParentIdentity != "" {
		return ErrState
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateMode(ctx, d, item); err != nil {
		return err
	}
	sourceRoot, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	sourceParent, sourceName, err := anchoredParent(sourceRoot, item.Source, false)
	if err != nil {
		return err
	}
	defer sourceParent.Close()
	source, err := openRegular(sourceParent, sourceName)
	if err != nil {
		return ErrState
	}
	info, err := source.Stat()
	source.Close()
	if err != nil || !matches(info, item) || time.Since(info.ModTime()) < 30*time.Second {
		return ErrState
	}
	if err = verifyOriginal(ctx, sourceParent, sourceName, item, p.Digest); err != nil {
		return err
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
	info, err = parent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrPath
	}
	fd, err := unix.Openat(int(parent.Fd()), p.Temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrState
	}
	stage := os.NewFile(uintptr(fd), p.Temp)
	defer stage.Close()
	info, err = stage.Stat()
	if err != nil || Identity(info) != p.StagingIdentity {
		return ErrState
	}
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0700 {
		return ErrState
	}
	if err = verifyPreparedObject(ctx, d, item, p, stage); err != nil {
		return err
	}
	if err = beforePublish(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = validateSourceBinding(d, item); err != nil {
		return err
	}
	if err = validatePreparedBinding(d, item, p); err != nil {
		return err
	}
	if err = verifyOriginal(ctx, sourceParent, sourceName, item, p.Digest); err != nil {
		return err
	}
	if err = verifyPreparedObject(ctx, d, item, p, stage); err != nil {
		return err
	}
	if err = unix.Linkat(fd, "payload", int(parent.Fd()), name, 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return modeOperationError(err)
	}
	if errors.Is(err, unix.EEXIST) {
		// The original executor or another explicit recovery may have won.
		// Equal content is not enough; the private anchor must prove ownership.
		check := d
		if check.Mode == "move" {
			check.Mode = "copy"
		}
		if e := VerifyPublished(ctx, check, item, p); e != nil {
			return ErrClaimed
		}
	}
	if err = stage.Sync(); err != nil {
		return err
	}
	if err = parent.Sync(); err != nil {
		return err
	}
	check := d
	if check.Mode == "move" {
		check.Mode = "copy"
	}
	return VerifyPublished(ctx, check, item, p)
}

func verifyPreparedObject(ctx context.Context, d Definition, item Entry, p Proof, stage *os.File) error {
	if p.Anchor {
		if err := verifyAnchorObject(stage, p); err != nil {
			return err
		}
	}
	return verifyPreparedNamedObject(ctx, d, item, p, stage, "payload")
}

func verifyPreparedNamedObject(ctx context.Context, d Definition, item Entry, p Proof, stage *os.File, name string) error {
	if p.Incomplete {
		return ErrState
	}
	if d.Mode == "softlink" {
		if p.Kind != "symlink" || p.LinkTarget != filepath.Join(d.SourceRoot, item.Source) {
			return ErrState
		}
		return verifyLinkObject(stage, name, p)
	}
	if p.Kind != "" && p.Kind != "regular" || p.LinkTarget != "" || d.Mode == "link" && p.Identity != item.Identity {
		return ErrState
	}
	f, err := openRegular(stage, name)
	if err != nil {
		return ErrState
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || Identity(info) != p.Identity || info.Size() != item.Size {
		return ErrState
	}
	if d.Mode == "link" && !matches(info, item) {
		return ErrState
	}
	digest, err := hashSource(ctx, f, item.Size)
	if err != nil {
		return err
	}
	if digest != p.Digest {
		return ErrState
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return ErrState
	}
	current, err := openRegular(stage, name)
	if err != nil {
		return ErrState
	}
	defer current.Close()
	info, err = current.Stat()
	if err != nil || Identity(info) != p.Identity {
		return ErrState
	}
	return nil
}

func validatePreparedBinding(d Definition, item Entry, p Proof) error {
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
	info, err := parent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrPath
	}
	fd, err := unix.Openat(int(parent.Fd()), p.Temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrState
	}
	stage := os.NewFile(uintptr(fd), p.Temp)
	defer stage.Close()
	info, err = stage.Stat()
	if err != nil || Identity(info) != p.StagingIdentity {
		return ErrState
	}
	var directoryStat unix.Stat_t
	if err = unix.Fstat(fd, &directoryStat); err != nil || directoryStat.Uid != uint32(os.Geteuid()) || directoryStat.Mode&0777 != 0700 {
		return ErrState
	}
	stat, err := lstatAt(stage, "payload")
	if err != nil || statIdentity(stat) != p.Identity {
		return ErrState
	}
	if p.Anchor {
		return verifyAnchorObject(stage, p)
	}
	return nil
}
