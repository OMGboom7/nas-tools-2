//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func prepareDiscMove(ctx context.Context, d Definition, e Entry, p Proof, name string) (Proof, error) {
	if !MoveMode(d.Mode) || e.Source == "." || p.SourceHold != "" || !strings.HasPrefix(name, ".nastool-move-") || filepath.Base(name) != name {
		return p, ErrState
	}
	if err := verifyMoveTarget(ctx, d, e, p); err != nil {
		return p, err
	}
	check := d
	check.Mode = "copy"
	if _, err := discSourceStamp(ctx, check, e, &p, true); err != nil {
		return p, err
	}
	r, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return p, err
	}
	defer r.Close()
	parent, _, err := anchoredParent(r, e.Source, false)
	if err != nil {
		return p, err
	}
	defer parent.Close()
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		return p, err
	}
	hold, err := discDir(parent, name)
	if err != nil {
		return p, err
	}
	defer hold.Close()
	info, err := hold.Stat()
	if err != nil {
		return p, err
	}
	p.SourceHold, p.SourceHoldIdentity = name, Identity(info)
	pi, err := parent.Stat()
	if err != nil {
		return p, err
	}
	p.SourceParentIdentity = Identity(pi)
	owned, err := discPrivate(parent, name, p.SourceHoldIdentity)
	if err != nil {
		return p, err
	}
	owned.Close()
	if err := hold.Sync(); err != nil {
		return p, err
	}
	if err := parent.Sync(); err != nil {
		return p, err
	}
	return p, nil
}
func discOriginalNamed(parent *os.File, leaf string, e Entry) (bool, error) {
	stat, err := lstatAt(parent, leaf)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return statIdentity(stat) == e.Identity, nil
}
func verifyDiscOriginal(ctx context.Context, d Definition, e Entry, p Proof, root *os.File, partial bool) error {
	m, err := discManifest(d, e)
	if err != nil {
		return err
	}
	info, err := root.Stat()
	if err != nil || Identity(info) != m.Root.Identity {
		return ErrState
	}
	expected := map[string]DiscNode{}
	for _, n := range m.Nodes {
		expected[n.Path] = n
	}
	hashes := discFiles(p)
	actual, err := discWalk(ctx, root, false)
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for _, n := range actual {
		want, ok := expected[n.Path]
		if !ok || n.Directory != want.Directory || n.Node.Identity != want.Node.Identity {
			return ErrState
		}
		found[n.Path] = true
		if n.Directory {
			if !partial && n.Node != want.Node {
				return ErrState
			}
		} else {
			if n.Node.Size != want.Node.Size || n.Node.Modified != want.Node.Modified || n.Node.Changed != want.Node.Changed {
				return ErrState
			}
			if err := discHashFile(ctx, root, n.Path, want.Node.Identity, want.Node.Size, hashes[n.Path].Digest); err != nil {
				return err
			}
		}
	}
	if !partial && len(found) != len(expected) {
		return ErrState
	}
	return nil
}
func continueDiscMove(ctx context.Context, d Definition, e Entry, p Proof, quarantined bool, mark func() error) error {
	if !quarantined && mark == nil {
		return ErrState
	}
	if err := verifyMoveTarget(ctx, d, e, p); err != nil {
		return err
	}
	parent, hold, leaf, err := openMoveHold(d, e, p)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer hold.Close()
	if !quarantined {
		renamed := false
		if _, err := lstatAt(hold, "payload"); errors.Is(err, unix.ENOENT) {
			check := d
			check.Mode = "copy"
			if _, err := discSourceStamp(ctx, check, e, &p, true); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := validateMoveHoldBinding(d, e, p); err != nil {
				return err
			}
			if err := renameNoReplace(int(parent.Fd()), leaf, int(hold.Fd()), "payload"); err != nil {
				return modeOperationError(err)
			}
			renamed = true
		} else if err != nil {
			return err
		}
		payload, err := discDir(hold, "payload")
		if err != nil {
			return err
		}
		err = verifyDiscOriginal(ctx, d, e, p, payload, false)
		payload.Close()
		if err != nil {
			if renamed {
				root, openErr := anchoredRoot(d.SourceRoot, d.SourceIdentity)
				if openErr == nil {
					current, _, openErr := anchoredParent(root, e.Source, false)
					root.Close()
					if openErr == nil {
						info, e := current.Stat()
						if e == nil && Identity(info) == p.SourceParentIdentity {
							_ = renameNoReplace(int(hold.Fd()), "payload", int(current.Fd()), leaf)
						}
						current.Close()
					}
				}
			}
			return ErrState
		}
		if err := hold.Sync(); err != nil {
			return err
		}
		if err := parent.Sync(); err != nil {
			return err
		}
		same, err := discOriginalNamed(parent, leaf, e)
		if err != nil || same {
			return ErrState
		}
		if err := validateMoveHoldBinding(d, e, p); err != nil {
			return err
		}
		if err := mark(); err != nil {
			return err
		}
	}
	return verifyDiscQuarantined(ctx, d, e, p)
}
func verifyDiscQuarantined(ctx context.Context, d Definition, e Entry, p Proof) error {
	if err := verifyMoveTarget(ctx, d, e, p); err != nil {
		return err
	}
	parent, hold, leaf, err := openMoveHold(d, e, p)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer hold.Close()
	payload, err := discDir(hold, "payload")
	if err != nil {
		return err
	}
	defer payload.Close()
	if err := verifyDiscOriginal(ctx, d, e, p, payload, false); err != nil {
		return err
	}
	same, err := discOriginalNamed(parent, leaf, e)
	if err != nil || same {
		return ErrState
	}
	return validateMoveHoldBinding(d, e, p)
}
func cleanupDiscMovedSource(ctx context.Context, d Definition, e Entry, p Proof) error {
	if err := verifyMoveTarget(ctx, d, e, p); err != nil {
		return err
	}
	parent, hold, leaf, err := openMoveHold(d, e, p)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer hold.Close()
	same, err := discOriginalNamed(parent, leaf, e)
	if err != nil || same {
		return ErrState
	}
	payload, err := discDir(hold, "payload")
	if err == nil {
		if err := verifyDiscOriginal(ctx, d, e, p, payload, true); err != nil {
			payload.Close()
			return err
		}
		m, _ := discManifest(d, e)
		dirs := map[string]string{".": m.Root.Identity}
		files := map[string]DiscFileProof{}
		hashes := discFiles(p)
		for _, n := range m.Nodes {
			if n.Directory {
				dirs[n.Path] = n.Node.Identity
			} else {
				files[n.Path] = DiscFileProof{Path: n.Path, Identity: n.Node.Identity, Digest: hashes[n.Path].Digest}
			}
		}
		if err := discRemoveKnown(ctx, payload, dirs, files); err != nil {
			payload.Close()
			return err
		}
		payload.Close()
		if err := validateMoveHoldBinding(d, e, p); err != nil {
			return err
		}
		if err := discUnlinkDirectory(hold, "payload", e.Identity); err != nil {
			return err
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	// Retain this owned empty directory as positive cleanup evidence until the
	// source-cleaned database receipt has committed. Never infer from absence.
	entries, err := hold.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) != 0 {
		return ErrState
	}
	if err := hold.Sync(); err != nil {
		return err
	}
	return parent.Sync()
}
func cleanupDiscMoveTarget(ctx context.Context, d Definition, e Entry, p Proof) error {
	if err := verifyMoveTarget(ctx, d, e, p); err != nil {
		// Partial witness cleanup is allowed only by the already durable
		// target-cleanup receipt; verify the complete published tree instead.
		if err := verifyDiscPublished(ctx, d, e, p, false, true); err != nil {
			return err
		}
	}
	parent, hold, leaf, err := openMoveHold(d, e, p)
	if err == nil {
		same, e2 := discOriginalNamed(parent, leaf, e)
		if e2 != nil || same {
			parent.Close()
			hold.Close()
			return ErrState
		}
		entries, e2 := hold.ReadDir(1)
		if e2 != nil && !errors.Is(e2, io.EOF) {
			parent.Close()
			hold.Close()
			return e2
		}
		if len(entries) != 0 {
			parent.Close()
			hold.Close()
			return ErrState
		}
		hold.Close()
		if err := validateMoveHoldBinding(d, e, p); err != nil {
			parent.Close()
			return err
		}
		err = unix.Unlinkat(int(parent.Fd()), p.SourceHold, unix.AT_REMOVEDIR)
		if err == nil {
			err = parent.Sync()
		}
		parent.Close()
		if err != nil {
			return err
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return cleanupDisc(ctx, d, e, p)
}
