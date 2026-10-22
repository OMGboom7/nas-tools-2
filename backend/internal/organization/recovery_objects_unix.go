//go:build linux || darwin

package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

func recoveryAnchoredParent(root *os.File, relative string) (*os.File, string, error) {
	if !validRelative(relative) || relative == "." {
		return nil, "", ErrPath
	}
	fd, err := unix.Dup(int(root.Fd()))
	if err != nil {
		return nil, "", err
	}
	unix.CloseOnExec(fd)
	parts := strings.Split(relative, "/")
	for _, part := range parts[:len(parts)-1] {
		if err := recoverySameMount(fd, part); err != nil {
			unix.Close(fd)
			return nil, "", err
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, "", err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path.Dir(relative)), parts[len(parts)-1], nil
}

func recoveryLocalHeader(parent *os.File, name, relative string) (RecoveryNode, error) {
	if err := recoverySameMount(int(parent.Fd()), name); err != nil {
		return RecoveryNode{}, err
	}
	n, kind, err := libraryNode(parent, name)
	if err != nil {
		return RecoveryNode{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return RecoveryNode{}, err
	}
	if statIdentity(st) != n.Identity {
		return RecoveryNode{}, ErrState
	}
	r := RecoveryNode{Path: relative, Node: n, Mode: uint32(st.Mode) & 07777}
	// Renames and removal of a reviewed hard-link sibling legitimately change
	// ctime/link counts. Identity, bytes, mtime and mode remain independently
	// checked; directory timestamps never stand in for a complete inventory.
	r.Node.Changed = [2]int64{}
	r.Node.Links = 0
	switch kind {
	case unix.S_IFREG:
		r.Kind = "file"
	case unix.S_IFDIR:
		r.Kind = "directory"
		r.Node.Size = 0
		r.Node.Modified = 0
	case unix.S_IFLNK:
		r.Kind = "link"
		r.Link, err = readLinkAt(parent, name)
		if err != nil || !utf8.ValidString(r.Link) {
			return RecoveryNode{}, ErrPath
		}
		sum := sha256.Sum256([]byte(r.Link))
		r.Digest = hex.EncodeToString(sum[:])
	default:
		return RecoveryNode{}, ErrMode
	}
	return r, nil
}
func recoveryLocalSnapshot(ctx context.Context, s RecoveryScope, selected string) ([]RecoveryNode, error) {
	return recoveryLocalScan(ctx, s, selected, true)
}
func recoveryLocalInventory(ctx context.Context, s RecoveryScope, selected string) ([]RecoveryNode, error) {
	return recoveryLocalScan(ctx, s, selected, false)
}
func recoveryLocalScan(ctx context.Context, s RecoveryScope, selected string, hashFiles bool) ([]RecoveryNode, error) {
	r, err := anchoredRoot(s.Root.Path, s.Root.Identity)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	p, leaf, err := recoveryAnchoredParent(r, selected)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	nodes := []RecoveryNode{}
	var bytes int64
	var walk func(*os.File, string, string, int) error
	walk = func(parent *os.File, name, relative string, depth int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if depth > 64 || len(nodes) >= RecoveryObjectLimit || !validRelative(relative) || !utf8.ValidString(relative) {
			return ErrLimit
		}
		before, err := recoveryLocalHeader(parent, name, relative)
		if err != nil {
			return err
		}
		if before.Kind == "file" {
			if before.Node.Size < 0 || before.Node.Size > RecoveryByteLimit-bytes {
				return ErrLimit
			}
			bytes += before.Node.Size
		}
		if before.Kind == "file" && hashFiles {
			if before.Node.Size < 0 || before.Node.Size > RecoveryByteLimit {
				return ErrLimit
			}
			f, e := openRegular(parent, name)
			if e != nil {
				return e
			}
			info, e := f.Stat()
			if e != nil || Identity(info) != before.Node.Identity {
				f.Close()
				return ErrState
			}
			before.Digest, e = hashSource(ctx, f, before.Node.Size)
			f.Close()
			if e != nil {
				return e
			}
		}
		nodes = append(nodes, before)
		if before.Kind == "directory" {
			fd, e := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if e != nil {
				return e
			}
			dir := os.NewFile(uintptr(fd), name)
			info, e := dir.Stat()
			if e != nil || Identity(info) != before.Node.Identity {
				dir.Close()
				return ErrState
			}
			names := []string{}
			for {
				batch, e := dir.Readdirnames(128)
				names = append(names, batch...)
				if len(names)+len(nodes) > RecoveryObjectLimit {
					dir.Close()
					return ErrLimit
				}
				if errors.Is(e, io.EOF) {
					break
				}
				if e != nil {
					dir.Close()
					return e
				}
			}
			sort.Strings(names)
			for _, child := range names {
				if e := walk(dir, child, path.Join(relative, child), depth+1); e != nil {
					dir.Close()
					return e
				}
			}
			dir.Close()
		}
		after, e := recoveryLocalHeader(parent, name, relative)
		if e != nil {
			return e
		}
		// Only the file's bytes were added to the header during the read.
		if before.Kind == "file" {
			after.Digest = before.Digest
		}
		if !recoveryNodeSame(before, after) {
			return ErrState
		}
		return nil
	}
	if err := walk(p, leaf, ".", 0); err != nil {
		return nil, err
	}
	recoverySort(nodes)
	return nodes, nil
}
func recoveryLocalBrowse(ctx context.Context, s RecoveryScope, relative string) ([]RecoveryBrowseEntry, error) {
	r, err := anchoredRoot(s.Root.Path, s.Root.Identity)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	dir := r
	if relative != "." {
		p, leaf, e := recoveryAnchoredParent(r, relative)
		if e != nil {
			return nil, e
		}
		fd, e := unix.Openat(int(p.Fd()), leaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		p.Close()
		if e != nil {
			return nil, e
		}
		dir = os.NewFile(uintptr(fd), relative)
		defer dir.Close()
	}
	entries := []RecoveryBrowseEntry{}
	for {
		names, e := dir.Readdirnames(128)
		for _, name := range names {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if len(entries) >= RecoveryObjectLimit || !validRelative(name) || !utf8.ValidString(name) {
				return nil, ErrLimit
			}
			n, k, err := libraryNode(dir, name)
			if err != nil {
				return nil, err
			}
			_ = n
			p := path.Join(relative, name)
			entries = append(entries, RecoveryBrowseEntry{Name: name, Path: p, Directory: k == unix.S_IFDIR, Candidate: RecoveryCandidate(p)})
		}
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return nil, e
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}
func recoveryLocalPresence(s RecoveryScope, relative string) (*RecoveryNode, error) {
	r, err := anchoredRoot(s.Root.Path, s.Root.Identity)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	p, leaf, err := recoveryAnchoredParent(r, relative)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	n, err := recoveryLocalHeader(p, leaf, ".")
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}
func recoveryLocalCapture(ctx context.Context, d RecoveryDefinition, capture string, current func() error) error {
	if current == nil || !RecoveryCandidate(capture) {
		return ErrState
	}
	s := d.Scope
	original, err := recoveryLocalPresence(s, d.Path)
	if err != nil {
		return err
	}
	held, err := recoveryLocalPresence(s, capture)
	if err != nil {
		return err
	}
	if original == nil {
		if held == nil {
			return ErrState
		}
		nodes, e := recoveryLocalSnapshot(ctx, s, capture)
		if e != nil || Digest(nodes) != Digest(d.Nodes) {
			return ErrState
		}
		return nil
	}
	if held != nil {
		return ErrState
	}
	nodes, err := recoveryLocalSnapshot(ctx, s, d.Path)
	if err != nil || Digest(nodes) != Digest(d.Nodes) {
		return ErrState
	}
	if err := current(); err != nil {
		return err
	}
	r, err := anchoredRoot(s.Root.Path, s.Root.Identity)
	if err != nil {
		return err
	}
	defer r.Close()
	p, leaf, err := recoveryAnchoredParent(r, d.Path)
	if err != nil {
		return err
	}
	defer p.Close()
	if path.Dir(capture) != path.Dir(d.Path) {
		return ErrPath
	}
	latest, err := recoveryLocalHeader(p, leaf, ".")
	if err != nil {
		return err
	}
	want := d.Nodes[0]
	want.Digest = ""
	if latest.Kind == "link" {
		want.Digest = latest.Digest
	}
	if !recoveryNodeSame(latest, want) {
		return ErrState
	}
	if err := renameNoReplace(int(p.Fd()), leaf, int(p.Fd()), path.Base(capture)); err != nil {
		return err
	}
	if err := unix.Fsync(int(p.Fd())); err != nil {
		return err
	}
	nodes, err = recoveryLocalSnapshot(ctx, s, capture)
	if err != nil || Digest(nodes) != Digest(d.Nodes) {
		return ErrState
	}
	return current()
}
func recoveryLocalRemove(ctx context.Context, s RecoveryScope, location string, want RecoveryNode, current func() error) error {
	if current == nil {
		return ErrState
	}
	if err := current(); err != nil {
		return err
	}
	var nodes []RecoveryNode
	var err error
	if want.Kind == "directory" {
		nodes, err = recoveryLocalInventory(ctx, s, location)
	} else {
		nodes, err = recoveryLocalSnapshot(ctx, s, location)
	}
	if err != nil {
		return err
	}
	want.Path = "."
	if len(nodes) != 1 || !recoveryNodeSame(nodes[0], want) {
		return ErrState
	}
	r, err := anchoredRoot(s.Root.Path, s.Root.Identity)
	if err != nil {
		return err
	}
	defer r.Close()
	p, leaf, err := recoveryAnchoredParent(r, location)
	if err != nil {
		return err
	}
	defer p.Close()
	latest, err := recoveryLocalHeader(p, leaf, ".")
	if err != nil {
		return err
	}
	if want.Kind == "file" {
		latest.Digest = want.Digest
	}
	if !recoveryNodeSame(latest, want) {
		return ErrState
	}
	flags := 0
	if want.Kind == "directory" {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(int(p.Fd()), leaf, flags); err != nil {
		return err
	}
	return unix.Fsync(int(p.Fd()))
}
func recoveryLocalReturn(ctx context.Context, d RecoveryDisposal, want RecoveryNode, current func() error) error {
	if current == nil || ctx.Err() != nil {
		return ErrState
	}
	if err := current(); err != nil {
		return err
	}
	r, err := anchoredRoot(d.Definition.Scope.Root.Path, d.Definition.Scope.Root.Identity)
	if err != nil {
		return err
	}
	defer r.Close()
	p, leaf, err := recoveryAnchoredParent(r, d.Path)
	if err != nil {
		return err
	}
	defer p.Close()
	if path.Dir(d.Capture) != path.Dir(d.Path) {
		return ErrPath
	}
	held, err := recoveryLocalHeader(p, path.Base(d.Capture), ".")
	if err != nil || !recoveryNodeSame(held, want) {
		return ErrState
	}
	if err := renameNoReplace(int(p.Fd()), path.Base(d.Capture), int(p.Fd()), leaf); err != nil {
		return err
	}
	if err := unix.Fsync(int(p.Fd())); err != nil {
		return err
	}
	actual, err := recoveryLocalHeader(p, leaf, ".")
	if err != nil || !recoveryNodeSame(actual, want) {
		return ErrState
	}
	return current()
}
