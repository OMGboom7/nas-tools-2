//go:build linux || darwin

package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Even after durable abandonment intent, target absence never substitutes for
// positive original-source evidence. Own hard-link removal changes ctime/link
// counts; only the original inode, original bytes and bounded own links qualify.
func discUnpublishedBoundary(ctx context.Context, d Definition, e Entry, p Proof) error {
	if !SupportedMode(d.Mode) || p.SourceHold != "" || p.SourceHoldIdentity != "" || p.SourceParentIdentity != "" {
		return ErrState
	}
	if err := discProofValid(d, e, p); err != nil {
		return err
	}
	if !p.Anchor || p.Temp == "" || p.ParentIdentity == "" || p.StagingIdentity == "" {
		return ErrState
	}
	source, err := discSource(ctx, d, e)
	if err != nil {
		return err
	}
	defer source.Close()
	m, _ := discManifest(d, e)
	nodes, err := discWalk(ctx, source, false)
	if err != nil {
		return err
	}
	if len(nodes) != len(m.Nodes) {
		return ErrState
	}
	files := discFiles(p)
	linkCounts := discLinkCounts(m)
	for i, n := range nodes {
		want := m.Nodes[i]
		if n.Path != want.Path || n.Directory != want.Directory {
			return ErrState
		}
		if !n.Directory && d.Mode == "link" {
			if n.Node.Identity != want.Node.Identity || n.Node.Parent != want.Node.Parent || n.Node.Size != want.Node.Size || n.Node.Modified != want.Node.Modified || n.Node.Links < want.Node.Links || n.Node.Links > want.Node.Links+2*linkCounts[want.Node.Identity] {
				return ErrState
			}
		} else if n.Node != want.Node {
			return ErrState
		}
		if !n.Directory {
			if time.Since(time.Unix(0, n.Node.Modified)) < 30*time.Second {
				return ErrState
			}
			if err := discHashFile(ctx, source, n.Path, n.Node.Identity, n.Node.Size, files[n.Path].Digest); err != nil {
				return err
			}
		}
	}
	root, _, err := libraryNode(source, ".")
	if err != nil || root.Identity != m.Root.Identity || root.Modified != m.Root.Modified || root.Changed != m.Root.Changed || root.Links != m.Root.Links {
		return ErrState
	}
	parent, name, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	if _, err := lstatAt(parent, name); !errors.Is(err, unix.ENOENT) {
		return ErrClaimed
	}
	return nil
}

// A journaled incomplete copy is disposable only when every remaining byte is
// an exact prefix of its approved original. This is not permission to resume it.
func discPrefix(ctx context.Context, d Definition, e Entry, root *os.File, n DiscNode, fp DiscFileProof) error {
	node, kind, err := discLeaf(root, n.Path)
	if err != nil || kind != unix.S_IFREG || node.Identity != fp.Identity || node.Size < 0 || node.Size > n.Node.Size {
		return ErrState
	}
	source, err := discSource(ctx, d, e)
	if err != nil {
		return err
	}
	defer source.Close()
	sp, sn, err := anchoredParent(source, n.Path, false)
	if err != nil {
		return err
	}
	defer sp.Close()
	src, err := openRegular(sp, sn)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil || Identity(info) != n.Node.Identity {
		return ErrState
	}
	h := sha256.New()
	size, err := io.Copy(h, io.LimitReader(contextReader{ctx: ctx, reader: src}, node.Size))
	if err != nil || size != node.Size {
		return ErrState
	}
	if err := discHashFile(ctx, root, n.Path, fp.Identity, node.Size, hex.EncodeToString(h.Sum(nil))); err != nil {
		return err
	}
	after, _, err := discLeaf(root, n.Path)
	if err != nil || after != node {
		return ErrState
	}
	return nil
}
func discDisposableTree(ctx context.Context, d Definition, e Entry, p Proof, root *os.File, dirs map[string]string, intent bool) error {
	if !p.Incomplete {
		return discVerifyNodes(ctx, d, e, p, root, dirs, intent, false, true)
	}
	// Validate the complete topology/inodes first, without treating half-written
	// bytes as a published disc. Witnesses and payload pin the very same leaves.
	if err := discVerifyNodes(ctx, d, e, p, root, dirs, intent, false, false); err != nil {
		return err
	}
	if d.Mode != "copy" && d.Mode != "move" {
		return discVerifyNodes(ctx, d, e, p, root, dirs, intent, false, true)
	}
	m, _ := discManifest(d, e)
	files := discFiles(p)
	for _, n := range m.Nodes {
		if n.Directory {
			continue
		}
		if _, _, err := discLeaf(root, n.Path); errors.Is(err, unix.ENOENT) && intent {
			continue
		} else if err != nil {
			return err
		}
		if err := discPrefix(ctx, d, e, root, n, files[n.Path]); err != nil {
			return err
		}
	}
	return nil
}
func inspectDiscUnpublished(ctx context.Context, d Definition, e Entry, p Proof, intent bool, beforeDelete func() error) error {
	if beforeDelete != nil && !intent {
		return ErrState
	}
	if err := discUnpublishedBoundary(ctx, d, e, p); err != nil {
		return err
	}
	parent, _, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	stage, err := discPrivate(parent, p.Temp, p.StagingIdentity)
	if errors.Is(err, unix.ENOENT) && intent {
		if beforeDelete != nil {
			if err := beforeDelete(); err != nil {
				return err
			}
		}
		return parent.Sync()
	}
	if err != nil {
		return err
	}
	defer stage.Close()
	names, err := stage.ReadDir(3)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	present := map[string]bool{}
	for _, n := range names {
		if n.Name() != "payload" && n.Name() != "witness" {
			return ErrState
		}
		present[n.Name()] = true
	}
	if !intent && (!present["payload"] || !present["witness"]) || present["payload"] && !present["witness"] {
		return ErrState
	}
	for _, name := range []string{"payload", "witness"} {
		if !present[name] {
			continue
		}
		f, err := discDir(stage, name)
		if err != nil {
			return err
		}
		dirs := p.Disc.Directories
		if name == "witness" {
			dirs = p.Disc.WitnessDirectories
		}
		err = discDisposableTree(ctx, d, e, p, f, dirs, intent)
		f.Close()
		if err != nil {
			return err
		}
	}
	if beforeDelete == nil {
		return nil
	}
	m, _ := discManifest(d, e)
	expected := map[string]DiscNode{}
	for _, n := range m.Nodes {
		expected[n.Path] = n
	}
	files := discFiles(p)
	linkCounts := discLinkCounts(m)
	boundary := func() error {
		if err := beforeDelete(); err != nil {
			return err
		}
		// Per-node checks below are linear in total payload bytes. Rehashing
		// the entire Blu-ray for every unlink would be quadratic on real discs.
		source, err := discSource(ctx, d, e)
		if err != nil {
			return err
		}
		node, _, err := libraryNode(source, ".")
		source.Close()
		if err != nil || node.Identity != m.Root.Identity || node.Modified != m.Root.Modified || node.Changed != m.Root.Changed || node.Links != m.Root.Links {
			return ErrState
		}
		current, name, err := discTargetParent(d, e, &p, false)
		if err != nil {
			return err
		}
		_, err = lstatAt(current, name)
		current.Close()
		if !errors.Is(err, unix.ENOENT) {
			return ErrClaimed
		}
		return unpublishedStageBinding(parent, stage, p)
	}
	for _, name := range []string{"payload", "witness"} {
		if !present[name] {
			continue
		}
		if err := boundary(); err != nil {
			return err
		}
		f, err := discDir(stage, name)
		if err != nil {
			return err
		}
		dirs := p.Disc.Directories
		if name == "witness" {
			dirs = p.Disc.WitnessDirectories
		}
		if err := discDisposableTree(ctx, d, e, p, f, dirs, true); err != nil {
			f.Close()
			return err
		}
		err = discRemoveKnown(ctx, f, dirs, files, func(path string) error {
			if err := boundary(); err != nil {
				return err
			}
			n, ok := expected[path]
			if !ok {
				return ErrState
			}
			source, err := discSource(ctx, d, e)
			if err != nil {
				return err
			}
			defer source.Close()
			node, kind, err := discLeaf(source, path)
			if err != nil {
				return err
			}
			if n.Directory {
				if kind != unix.S_IFDIR || node != n.Node {
					return ErrState
				}
				return nil
			}
			if kind != unix.S_IFREG {
				return ErrState
			}
			if d.Mode == "link" {
				if node.Identity != n.Node.Identity || node.Parent != n.Node.Parent || node.Size != n.Node.Size || node.Modified != n.Node.Modified || node.Links < n.Node.Links || node.Links > n.Node.Links+2*linkCounts[n.Node.Identity] {
					return ErrState
				}
			} else if node != n.Node {
				return ErrState
			}
			fp := files[path]
			if err := discHashFile(ctx, source, path, n.Node.Identity, n.Node.Size, fp.Digest); err != nil {
				return err
			}
			if p.Incomplete && (d.Mode == "copy" || d.Mode == "move") {
				return discPrefix(ctx, d, e, f, n, fp)
			}
			if d.Mode == "softlink" {
				parent, leaf, err := anchoredParent(f, path, false)
				if err != nil {
					return err
				}
				link, err := readLinkAt(parent, leaf)
				parent.Close()
				if err != nil || link != fp.LinkTarget {
					return ErrState
				}
				return nil
			}
			return discHashFile(ctx, f, path, fp.Identity, n.Node.Size, fp.Digest)
		})
		f.Close()
		if err != nil {
			return err
		}
		if err := boundary(); err != nil {
			return err
		}
		if err := discUnlinkDirectory(stage, name, dirs["."]); err != nil {
			return err
		}
	}
	if err := boundary(); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parent.Fd()), p.Temp, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	return discUnpublishedBoundary(ctx, d, e, p)
}
