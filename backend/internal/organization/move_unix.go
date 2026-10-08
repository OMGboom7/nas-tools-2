//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func MoveHoldName(job string, index int) string {
	return fmt.Sprintf(".nastool-move-%s-%d.hold", job, index)
}

// PrepareMoveTarget publishes an independent copy without touching the source.
// The caller must obtain separate source-removal consent before later steps.
func PrepareMoveTarget(ctx context.Context, d Definition, item Entry, temp string, prepared func(Proof) error) (Proof, error) {
	if d.Mode != "move" {
		return Proof{}, ErrMode
	}
	d.Mode = "copy"
	return Copy(ctx, d, item, temp, prepared)
}

func verifyMoveTarget(ctx context.Context, d Definition, item Entry, p Proof) error {
	if d.Mode != "move" {
		return ErrMode
	}
	d.Mode = "copy"
	err := VerifyPublished(ctx, d, item, p)
	if errors.Is(err, os.ErrNotExist) {
		return ErrState
	}
	return err
}

func verifyOriginal(ctx context.Context, parent *os.File, name string, item Entry, digest string) error {
	f, err := openRegular(parent, name)
	if err != nil {
		return ErrState
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !matches(info, item) {
		return ErrState
	}
	actual, err := hashSource(ctx, f, item.Size)
	if err != nil {
		return err
	}
	if actual != digest {
		return ErrState
	}
	info, err = f.Stat()
	if err != nil || !matches(info, item) {
		return ErrState
	}
	return nil
}

// PrepareMove protects a recovery reference before saving the durable source
// removal intent. It does not rename/remove the source or complete the history.
func PrepareMove(ctx context.Context, d Definition, item Entry, p Proof, name string) (Proof, error) {
	if d.Mode != "move" || !strings.HasPrefix(name, ".nastool-move-") || filepath.Base(name) != name || !validRelative(name) || p.SourceHold != "" {
		return p, ErrState
	}
	if err := verifyMoveTarget(ctx, d, item, p); err != nil {
		return p, err
	}
	root, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return p, err
	}
	defer root.Close()
	parent, leaf, err := anchoredParent(root, item.Source, false)
	if err != nil {
		return p, err
	}
	defer parent.Close()
	if err = verifyOriginal(ctx, parent, leaf, item, p.Digest); err != nil {
		return p, err
	}
	if err = unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return p, ErrState
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return p, err
	}
	hold := os.NewFile(uintptr(fd), name)
	defer hold.Close()
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0700 {
		return p, ErrPath
	}
	if err = unix.Linkat(int(parent.Fd()), leaf, fd, "witness", 0); err != nil {
		return p, modeOperationError(err)
	}
	if err = verifyOriginal(ctx, hold, "witness", item, p.Digest); err != nil {
		return p, err
	}
	parentInfo, err := parent.Stat()
	if err != nil {
		return p, err
	}
	holdInfo, err := hold.Stat()
	if err != nil {
		return p, err
	}
	p.SourceHold, p.SourceHoldIdentity, p.SourceParentIdentity = name, Identity(holdInfo), Identity(parentInfo)
	if err = hold.Sync(); err != nil {
		return p, err
	}
	if err = parent.Sync(); err != nil {
		return p, err
	}
	return p, nil
}

func openMoveHold(d Definition, item Entry, p Proof) (*os.File, *os.File, string, error) {
	if d.Mode != "move" || !strings.HasPrefix(p.SourceHold, ".nastool-move-") || filepath.Base(p.SourceHold) != p.SourceHold || !validRelative(p.SourceHold) || p.SourceHoldIdentity == "" || p.SourceParentIdentity == "" {
		return nil, nil, "", ErrState
	}
	root, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return nil, nil, "", err
	}
	parent, leaf, err := anchoredParent(root, item.Source, false)
	root.Close()
	if err != nil {
		return nil, nil, "", err
	}
	info, err := parent.Stat()
	if err != nil || Identity(info) != p.SourceParentIdentity {
		parent.Close()
		return nil, nil, "", ErrPath
	}
	fd, err := unix.Openat(int(parent.Fd()), p.SourceHold, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		parent.Close()
		return nil, nil, "", err
	}
	hold := os.NewFile(uintptr(fd), p.SourceHold)
	info, err = hold.Stat()
	if err != nil || Identity(info) != p.SourceHoldIdentity || info.Mode().Perm() != 0700 {
		parent.Close()
		hold.Close()
		return nil, nil, "", ErrPath
	}
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) {
		parent.Close()
		hold.Close()
		return nil, nil, "", ErrPath
	}
	return parent, hold, leaf, nil
}

func originalStillNamed(parent *os.File, leaf string, item Entry) (bool, error) {
	stat, err := lstatAt(parent, leaf)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return statIdentity(stat) == item.Identity, nil
}

// ContinueMove requires a separately confirmed request. A wrong source swapped
// into the rename race is restored only into an empty slot, never deleted or
// used to replace a new arrival. All uncertain cases retain both recovery copies.
func ContinueMove(ctx context.Context, d Definition, item Entry, p Proof, quarantined bool, markQuarantined func() error) error {
	return continueMove(ctx, d, item, p, quarantined, markQuarantined, nil)
}

// The hook is only used in tests to exercise a source rename race precisely.
func continueMove(ctx context.Context, d Definition, item Entry, p Proof, quarantined bool, markQuarantined func() error, beforeRename func() error) error {
	if !quarantined && markQuarantined == nil {
		return ErrState
	}
	if err := verifyMoveTarget(ctx, d, item, p); err != nil {
		return err
	}
	parent, hold, leaf, err := openMoveHold(d, item, p)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer hold.Close()
	if err = verifyOriginal(ctx, hold, "witness", item, p.Digest); err != nil {
		return err
	}
	if !quarantined {
		renamed := false
		_, statErr := lstatAt(hold, "payload")
		if errors.Is(statErr, os.ErrNotExist) {
			if err = validateSourceBinding(d, item); err != nil {
				return err
			}
			if err = verifyOriginal(ctx, parent, leaf, item, p.Digest); err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = validateMoveHoldBinding(d, item, p); err != nil {
				return err
			}
			if beforeRename != nil {
				if err = beforeRename(); err != nil {
					return err
				}
			}
			if err = renameNoReplace(int(parent.Fd()), leaf, int(hold.Fd()), "payload"); err != nil {
				return modeOperationError(err)
			}
			renamed = true
		} else if statErr != nil {
			return statErr
		}
		if err = verifyOriginal(ctx, hold, "payload", item, p.Digest); err != nil {
			// Restore the captured object, not the witness. Failure to restore
			// retains it for review; never unlink a foreign file/directory/link.
			if !renamed {
				return ErrState
			}
			currentRoot, e := anchoredRoot(d.SourceRoot, d.SourceIdentity)
			if e == nil {
				currentParent, _, e := anchoredParent(currentRoot, item.Source, false)
				currentRoot.Close()
				if e == nil {
					info, e := currentParent.Stat()
					if e == nil && Identity(info) == p.SourceParentIdentity {
						_ = renameNoReplace(int(hold.Fd()), "payload", int(currentParent.Fd()), leaf)
					}
					currentParent.Close()
				}
			}
			return ErrState
		}
		if err = hold.Sync(); err != nil {
			return err
		}
		if err = parent.Sync(); err != nil {
			return err
		}
		still, err := originalStillNamed(parent, leaf, item)
		if err != nil {
			return err
		}
		if still {
			return ErrState
		}
		if err = validateMoveHoldBinding(d, item, p); err != nil {
			return err
		}
		if err = markQuarantined(); err != nil {
			return err
		}
	}
	return VerifyQuarantined(ctx, d, item, p)
}

// Positive proof includes a source object actually placed in quarantine, not
// just the absence of a source filename. No deletion happens during verification.
func VerifyQuarantined(ctx context.Context, d Definition, item Entry, p Proof) error {
	if err := verifyMoveTarget(ctx, d, item, p); err != nil {
		return err
	}
	parent, hold, leaf, err := openMoveHold(d, item, p)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer hold.Close()
	if err = verifyOriginal(ctx, hold, "payload", item, p.Digest); err != nil {
		return err
	}
	if err = verifyOriginal(ctx, hold, "witness", item, p.Digest); err != nil {
		return err
	}
	still, err := originalStillNamed(parent, leaf, item)
	if err != nil {
		return err
	}
	if still {
		return ErrState
	}
	return validateMoveHoldBinding(d, item, p)
}

func validateMoveHoldBinding(d Definition, item Entry, p Proof) error {
	parent, hold, _, err := openMoveHold(d, item, p)
	if err != nil {
		return err
	}
	parent.Close()
	hold.Close()
	return nil
}

// CleanupMovedSource runs only after state/history commit and explicit source
// removal confirmation. It never touches the source slot, parent directories,
// or foreign objects. A failed cleanup keeps the target recovery anchor intact.
func CleanupMovedSource(ctx context.Context, d Definition, item Entry, p Proof) error {
	if err := verifyMoveTarget(ctx, d, item, p); err != nil {
		return err
	}
	parent, hold, leaf, err := openMoveHold(d, item, p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	defer hold.Close()
	still, err := originalStillNamed(parent, leaf, item)
	if err != nil {
		return err
	}
	if still {
		return ErrState
	}
	for _, name := range []string{"payload", "witness"} {
		if err = ctx.Err(); err != nil {
			return err
		}
		_, err = lstatAt(hold, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = verifyOriginal(ctx, hold, name, item, p.Digest); err != nil {
			return err
		}
		if err = validateMoveHoldBinding(d, item, p); err != nil {
			return err
		}
		still, err = originalStillNamed(parent, leaf, item)
		if err != nil {
			return err
		}
		if still {
			return ErrState
		}
		if err = unix.Unlinkat(int(hold.Fd()), name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err = hold.Sync(); err != nil {
		return err
	}
	if err = validateMoveHoldBinding(d, item, p); err != nil {
		return err
	}
	if err = unix.Unlinkat(int(parent.Fd()), p.SourceHold, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return parent.Sync()
}

// The source-cleaned receipt must be durable before removing the last private
// target anchor. Recheck the published target while that anchor still exists;
// empty/already removed stages can be retried without guessing file ownership.
func CleanupMoveTarget(ctx context.Context, d Definition, item Entry, p Proof) error {
	if d.Mode != "move" {
		return ErrMode
	}
	if err := ctx.Err(); err != nil {
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
	info, err := parent.Stat()
	if err != nil || Identity(info) != p.ParentIdentity {
		return ErrPath
	}
	if !strings.HasPrefix(p.Temp, ".nastool-copy-") || filepath.Base(p.Temp) != p.Temp || !validRelative(p.Temp) {
		return ErrPath
	}
	fd, err := unix.Openat(int(parent.Fd()), p.Temp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, os.ErrNotExist) {
		return Cleanup(d, item, p)
	}
	if err != nil {
		return err
	}
	stage := os.NewFile(uintptr(fd), p.Temp)
	defer stage.Close()
	info, err = stage.Stat()
	if err != nil || Identity(info) != p.StagingIdentity {
		return ErrState
	}
	_, err = lstatAt(stage, "payload")
	if err == nil {
		if err = verifyMoveTarget(ctx, d, item, p); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return Cleanup(d, item, p)
}
