//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func discMetadata(e Entry, path string) bool {
	if filepath.Dir(path) != "." {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if ext == ".nfo" {
		return stem == filepath.Base(e.Target) || stem == "movie" || stem == "tvshow" || stem == "season"
	}
	return (ext == ".jpg" || ext == ".jpeg" || ext == ".png") && (map[string]bool{"poster": true, "fanart": true, "background": true, "logo": true, "disc": true, "clearart": true, "banner": true, "thumb": true}[stem] || seasonArtworkName.MatchString(stem))
}
func discVerifyNodes(ctx context.Context, d Definition, e Entry, p Proof, root *os.File, dirs map[string]string, partial, metadata, hash bool) error {
	if err := discProofValid(d, e, p); err != nil {
		return err
	}
	info, err := root.Stat()
	if err != nil || Identity(info) != dirs["."] {
		return ErrState
	}
	m, _ := discManifest(d, e)
	wanted := map[string]DiscNode{}
	for _, n := range m.Nodes {
		wanted[n.Path] = n
	}
	files := discFiles(p)
	nodes, err := discWalk(ctx, root, d.Mode == "softlink")
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for _, n := range nodes {
		want, ok := wanted[n.Path]
		if metadata && discMetadata(e, n.Path) && !n.Directory {
			if node, kind, err := discLeaf(root, n.Path); err == nil && kind == unix.S_IFREG && node.Size <= 16<<20 {
				found[n.Path] = true
				continue
			}
		}
		if !ok || n.Directory != want.Directory {
			return ErrState
		}
		found[n.Path] = true
		if n.Directory {
			if n.Node.Identity != dirs[n.Path] {
				return ErrState
			}
			continue
		}
		fp, ok := files[n.Path]
		if !ok || n.Node.Identity != fp.Identity {
			return ErrState
		}
		parent, leaf, err := anchoredParent(root, n.Path, false)
		if err != nil {
			return err
		}
		if d.Mode == "softlink" {
			link, linkErr := readLinkAt(parent, leaf)
			parent.Close()
			if linkErr != nil || link != fp.LinkTarget {
				return ErrState
			}
			if hash {
				source, err := discSource(ctx, d, e)
				if err != nil {
					return err
				}
				err = discHashFile(ctx, source, n.Path, want.Node.Identity, want.Node.Size, fp.Digest)
				source.Close()
				if err != nil {
					return err
				}
			}
		} else {
			_, kind, err := libraryNode(parent, leaf)
			parent.Close()
			if err != nil || kind != unix.S_IFREG {
				return ErrState
			}
			if hash {
				if err := discHashFile(ctx, root, n.Path, fp.Identity, want.Node.Size, fp.Digest); err != nil {
					return err
				}
			}
		}
	}
	if !partial {
		for _, n := range m.Nodes {
			if metadata && discMetadata(e, n.Path) {
				continue
			}
			if !found[n.Path] {
				return ErrState
			}
		}
	}
	return nil
}
func discLeaf(root *os.File, path string) (LibraryNode, uint32, error) {
	p, n, err := anchoredParent(root, path, false)
	if err != nil {
		return LibraryNode{}, 0, err
	}
	defer p.Close()
	return libraryNode(p, n)
}
func discVerifyTree(ctx context.Context, d Definition, e Entry, p Proof, root *os.File, metadata bool) error {
	return discVerifyNodes(ctx, d, e, p, root, p.Disc.Directories, false, metadata, true)
}
func discVerifyWitness(ctx context.Context, d Definition, e Entry, p Proof, root *os.File, partial bool) error {
	return discVerifyNodes(ctx, d, e, p, root, p.Disc.WitnessDirectories, partial, false, false)
}
func verifyDiscPublished(ctx context.Context, d Definition, e Entry, p Proof, witness, metadata bool) error {
	if p.Incomplete {
		return ErrState
	}
	if d.Mode == "move" {
		d.Mode = "copy"
	}
	if err := discProofValid(d, e, p); err != nil {
		return err
	}
	parent, name, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	target, err := discDir(parent, name)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := discVerifyTree(ctx, d, e, p, target, metadata); err != nil {
		return err
	}
	if witness {
		stage, err := discPrivate(parent, p.Temp, p.StagingIdentity)
		if err != nil {
			return err
		}
		defer stage.Close()
		w, err := discDir(stage, "witness")
		if err != nil {
			return err
		}
		defer w.Close()
		return discVerifyWitness(ctx, d, e, p, w, false)
	}
	return nil
}
func publishDiscPrepared(ctx context.Context, d Definition, e Entry, p Proof, before func() error) error {
	if d.Mode == "move" {
		d.Mode = "copy"
	}
	if before == nil || p.Incomplete || p.SourceHold != "" {
		return ErrState
	}
	if err := discProofValid(d, e, p); err != nil {
		return err
	}
	parent, name, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	if _, err := lstatAt(parent, name); err == nil {
		if err := before(); err != nil {
			return err
		}
		return verifyDiscPublished(ctx, d, e, p, true, false)
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	stage, err := discPrivate(parent, p.Temp, p.StagingIdentity)
	if err != nil {
		return err
	}
	defer stage.Close()
	payload, err := discDir(stage, "payload")
	if err != nil {
		return err
	}
	defer payload.Close()
	if err := discVerifyTree(ctx, d, e, p, payload, false); err != nil {
		return err
	}
	w, err := discDir(stage, "witness")
	if err != nil {
		return err
	}
	defer w.Close()
	if err := discVerifyWitness(ctx, d, e, p, w, false); err != nil {
		return err
	}
	stamp, err := discSourceStamp(ctx, d, e, &p, true)
	if err != nil {
		return err
	}
	if err := before(); err != nil {
		return err
	}
	after, err := discSourceStamp(ctx, d, e, &p, false)
	if err != nil || stamp != after {
		return ErrState
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	current, _, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return err
	}
	current.Close()
	owned, err := discPrivate(parent, p.Temp, p.StagingIdentity)
	if err != nil {
		return err
	}
	owned.Close()
	if err := renameNoReplace(int(stage.Fd()), "payload", int(parent.Fd()), name); err != nil && !errors.Is(err, unix.EEXIST) {
		return modeOperationError(err)
	}
	if err := stage.Sync(); err != nil {
		return err
	}
	if err := parent.Sync(); err != nil {
		return err
	}
	return verifyDiscPublished(ctx, d, e, p, true, false)
}

// Only an already-committed target permits removal of remaining witnesses.
// Missing nodes are tolerated solely inside the same pinned owned directory;
// foreign/extra nodes prevent cleanup, never a broad recursive deletion.
func cleanupDisc(ctx context.Context, d Definition, e Entry, p Proof) error {
	if d.Mode == "move" {
		d.Mode = "copy"
	}
	if err := verifyDiscPublished(ctx, d, e, p, false, true); err != nil {
		return err
	}
	parent, _, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	stage, err := discPrivate(parent, p.Temp, p.StagingIdentity)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer stage.Close()
	if _, err := lstatAt(stage, "payload"); !errors.Is(err, unix.ENOENT) {
		return ErrState
	}
	w, err := discDir(stage, "witness")
	if err == nil {
		if err := discVerifyWitness(ctx, d, e, p, w, true); err != nil {
			w.Close()
			return err
		}
		if err := discRemoveKnown(ctx, w, p.Disc.WitnessDirectories, discFiles(p)); err != nil {
			w.Close()
			return err
		}
		w.Close()
		if err := discUnlinkDirectory(stage, "witness", p.Disc.WitnessDirectories["."]); err != nil {
			return err
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	owned, err := discPrivate(parent, p.Temp, p.StagingIdentity)
	if err != nil {
		return err
	}
	owned.Close()
	if err := unix.Unlinkat(int(parent.Fd()), p.Temp, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}
func discUnlinkDirectory(parent *os.File, name, id string) error {
	node, kind, err := libraryNode(parent, name)
	if err != nil || kind != unix.S_IFDIR || node.Identity != id {
		return ErrState
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return parent.Sync()
}
func discRemoveKnown(ctx context.Context, root *os.File, dirs map[string]string, files map[string]DiscFileProof, before ...func(string) error) error {
	nodes, err := discWalk(ctx, root, true)
	if err != nil {
		return err
	}
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(before) == 1 {
			if err := before[0](n.Path); err != nil {
				return err
			}
		}
		parent, leaf, err := anchoredParent(root, n.Path, false)
		if err != nil {
			return err
		}
		actual, kind, err := libraryNode(parent, leaf)
		if err != nil {
			parent.Close()
			return err
		}
		if n.Directory {
			if kind != unix.S_IFDIR || actual.Identity != dirs[n.Path] {
				parent.Close()
				return ErrState
			}
			err = unix.Unlinkat(int(parent.Fd()), leaf, unix.AT_REMOVEDIR)
		} else {
			p, ok := files[n.Path]
			if !ok || actual.Identity != p.Identity {
				parent.Close()
				return ErrState
			}
			if p.LinkTarget != "" {
				link, e := readLinkAt(parent, leaf)
				if e != nil || link != p.LinkTarget {
					parent.Close()
					return ErrState
				}
			} else if kind != unix.S_IFREG {
				parent.Close()
				return ErrState
			}
			err = unix.Unlinkat(int(parent.Fd()), leaf, 0)
		}
		if err == nil {
			err = parent.Sync()
		}
		parent.Close()
		if err != nil {
			return err
		}
	}
	return root.Sync()
}
