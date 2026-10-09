//go:build linux || darwin

package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func verifyAnchorObject(stage *os.File, p Proof) error {
	stat, err := lstatAt(stage, "anchor")
	if err != nil || statIdentity(stat) != p.Identity {
		return ErrState
	}
	if p.Kind == "symlink" {
		return verifyLinkObject(stage, "anchor", p)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrState
	}
	return nil
}

func verifyDisposableObject(ctx context.Context, d Definition, item Entry, p Proof, stage *os.File, name string, refs int) error {
	if !p.Incomplete {
		return verifyPreparedNamedObject(ctx, d, item, p, stage, name)
	}
	if d.Mode == "link" || d.Mode == "softlink" {
		// A link is created atomically, but it is not publication/complete proof
		// until the later prepared transaction. Its exact object still must match.
		p.Incomplete = false
		return verifyPreparedNamedObject(ctx, d, item, p, stage, name)
	}
	if (d.Mode != "copy" && d.Mode != "move") || !p.Anchor || p.Kind != "regular" || p.LinkTarget != "" || p.Identity == item.Identity {
		return ErrState
	}
	file, err := openRegular(stage, name)
	if err != nil {
		return ErrState
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || Identity(before) != p.Identity || before.Size() < 0 || before.Size() > item.Size {
		return ErrState
	}
	var stat unix.Stat_t
	if unix.Fstat(int(file.Fd()), &stat) != nil || stat.Uid != uint32(os.Geteuid()) || uint64(stat.Nlink) != uint64(refs) {
		return ErrState
	}
	actual, err := hashSource(ctx, file, before.Size())
	if err != nil {
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
	source, err := openRegular(parent, leaf)
	if err != nil {
		return ErrState
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !matches(info, item) {
		return ErrState
	}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, contextReader{ctx, io.LimitReader(source, before.Size())}, make([]byte, 256<<10))
	if err != nil {
		return err
	}
	if n != before.Size() || actual != hex.EncodeToString(hash.Sum(nil)) {
		return ErrState
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return ErrState
	}
	info, err = source.Stat()
	if err != nil || !matches(info, item) {
		return ErrState
	}
	bound, err := lstatAt(stage, name)
	if err != nil || statIdentity(bound) != p.Identity || bound.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrState
	}
	// Full original-source preservation is checked by unpublishedBoundary on
	// BOTH sides of this comparison. A matching prefix alone is never enough.
	return nil
}
