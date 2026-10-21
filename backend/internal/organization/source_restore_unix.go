//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type sourceRestoreEvidence struct{ Source, Payload, Witness *LibraryNode }

func sourceRestoreNode(ctx context.Context, d Definition, e Entry, p Proof, parent *os.File, name string) (*LibraryNode, error) {
	node, kind, err := libraryNode(parent, name)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if IsDisc(e) {
		if kind != unix.S_IFDIR {
			return nil, ErrState
		}
		dir, err := discDir(parent, name)
		if err != nil {
			return nil, err
		}
		err = verifyDiscOriginal(ctx, d, e, p, dir, false)
		dir.Close()
		if err != nil {
			return nil, err
		}
	} else if err := verifyOriginal(ctx, parent, name, e, p.Digest); err != nil {
		return nil, err
	}
	after, _, err := libraryNode(parent, name)
	if err != nil || after != node {
		return nil, ErrState
	}
	return &node, nil
}
func sourceRestoreHoldEntries(hold *os.File, disc bool) error {
	dir, err := discDir(hold, ".")
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.ReadDir(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) > 2 {
		return ErrState
	}
	for _, n := range names {
		if n.Name() != "payload" && (disc || n.Name() != "witness") {
			return ErrState
		}
	}
	return nil
}
func captureSourceRestore(ctx context.Context, d Definition, e Entry, p Proof, parent, hold *os.File, leaf string) (string, sourceRestoreEvidence, error) {
	v := sourceRestoreEvidence{}
	if ctx.Err() != nil {
		return "", v, ctx.Err()
	}
	var err error
	v.Source, err = sourceRestoreNode(ctx, d, e, p, parent, leaf)
	if err != nil {
		return "", v, err
	}
	if hold != nil {
		if err := sourceRestoreHoldEntries(hold, IsDisc(e)); err != nil {
			return "", v, err
		}
		v.Payload, err = sourceRestoreNode(ctx, d, e, p, hold, "payload")
		if err != nil {
			return "", v, err
		}
		if !IsDisc(e) {
			v.Witness, err = sourceRestoreNode(ctx, d, e, p, hold, "witness")
			if err != nil || v.Witness == nil {
				return "", v, ErrState
			}
		}
	}
	if v.Source != nil {
		return "source_in_place", v, nil
	}
	if v.Payload != nil {
		return "isolated_source", v, nil
	}
	if v.Witness != nil {
		return "witness_only", v, nil
	}
	return "", v, ErrState
}

// A missing source name is never sufficient evidence. Restore only the original
// identity and full bytes held by this journal. No target is opened or changed.
func inspectSourceRestore(ctx context.Context, d Definition, e Entry, p Proof, apply bool, current func() error) (string, string, error) {
	if !MoveMode(d.Mode) || p.Identity == "" || len(p.Digest) != 64 || e.Source == "." {
		return "", "", ErrState
	}
	var parent, hold *os.File
	var leaf string
	var err error
	if p.SourceHold != "" {
		parent, hold, leaf, err = openMoveHold(d, e, p)
		if err != nil {
			return "", "", err
		}
		defer hold.Close()
	} else {
		if p.SourceHoldIdentity != "" || p.SourceParentIdentity != "" {
			return "", "", ErrState
		}
		root, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
		if err != nil {
			return "", "", err
		}
		parent, leaf, err = anchoredParent(root, e.Source, false)
		root.Close()
		if err != nil {
			return "", "", err
		}
	}
	defer parent.Close()
	where, before, err := captureSourceRestore(ctx, d, e, p, parent, hold, leaf)
	if err != nil {
		return "", "", err
	}
	if !apply {
		return where, Digest(before), nil
	}
	if current == nil {
		return "", "", ErrState
	}
	if err := current(); err != nil {
		return "", "", err
	}
	validate := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hold != nil {
			return validateMoveHoldBinding(d, e, p)
		}
		r, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
		if err != nil {
			return err
		}
		defer r.Close()
		actual, _, err := anchoredParent(r, e.Source, false)
		if err != nil {
			return err
		}
		defer actual.Close()
		pi, err := actual.Stat()
		original, originalErr := parent.Stat()
		if err != nil || originalErr != nil || Identity(pi) != Identity(original) {
			return ErrPath
		}
		return nil
	}
	if err := validate(); err != nil {
		return "", "", err
	}
	second, after, err := captureSourceRestore(ctx, d, e, p, parent, hold, leaf)
	if err != nil || second != where || Digest(before) != Digest(after) {
		return "", "", ErrState
	}
	if where != "source_in_place" {
		if hold == nil {
			return "", "", ErrState
		}
		if err := validate(); err != nil {
			return "", "", err
		}
		if where == "isolated_source" {
			err = renameNoReplace(int(hold.Fd()), "payload", int(parent.Fd()), leaf)
		} else {
			err = unix.Linkat(int(hold.Fd()), "witness", int(parent.Fd()), leaf, 0)
		}
		if err != nil {
			return "", "", modeOperationError(err)
		}
		if err := hold.Sync(); err != nil {
			return "", "", err
		}
		if err := parent.Sync(); err != nil {
			return "", "", err
		}
	}
	if err := validate(); err != nil {
		return "", "", err
	}
	actual, final, err := captureSourceRestore(ctx, d, e, p, parent, hold, leaf)
	if err != nil || actual != "source_in_place" {
		return "", "", ErrState
	}
	return actual, Digest(final), nil
}
