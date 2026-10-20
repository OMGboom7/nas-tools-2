//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Discovery checks only directory layout. Adoption below independently checks
// every leaf and configured input binding, including a symlinked index file.
func libraryDirectDisc(root *os.File) bool {
	bd, err := discDir(root, "BDMV")
	if err != nil {
		return false
	}
	defer bd.Close()
	_, kind, err := libraryNode(bd, "index.bdmv")
	if err != nil || kind != unix.S_IFREG && kind != unix.S_IFLNK {
		return false
	}
	stream, err := discDir(bd, "STREAM")
	if err != nil {
		return false
	}
	stream.Close()
	return true
}
func libraryDiscCandidate(root *os.File) bool {
	if libraryDirectDisc(root) {
		return true
	}
	dir, err := discDir(root, ".")
	if err != nil {
		return false
	}
	defer dir.Close()
	names, err := dir.ReadDir(257)
	if err != nil || len(names) > 256 {
		return false
	}
	found := false
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		child, err := discDir(root, n.Name())
		if err != nil {
			continue
		}
		disc := libraryDirectDisc(child)
		child.Close()
		if disc {
			if !discVolumeName.MatchString(n.Name()) {
				return false
			}
			found = true
		}
	}
	return found
}
func libraryDiscAncestor(ctx context.Context, d LibraryDefinition) (string, error) {
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return "", err
	}
	defer r.Close()
	path := d.Selection
	if path != "." {
		p, n, err := anchoredParent(r, path, false)
		if err != nil {
			return "", err
		}
		_, kind, err := libraryNode(p, n)
		p.Close()
		if err != nil {
			return "", err
		}
		if kind != unix.S_IFDIR {
			path = filepath.Dir(path)
		}
	}
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		dir, err := discRoot(r, path)
		if err != nil {
			return "", err
		}
		candidate := libraryDiscCandidate(dir)
		dir.Close()
		if candidate {
			if path != "." && discVolumeName.MatchString(filepath.Base(path)) {
				parent, err := discRoot(r, filepath.Dir(path))
				if err == nil {
					composite := libraryDiscCandidate(parent)
					parent.Close()
					if composite {
						return filepath.Dir(path), nil
					}
				}
			}
			return path, nil
		}
		if path == "." {
			return "", nil
		}
		path = filepath.Dir(path)
	}
}
func captureLibraryDisc(ctx context.Context, d LibraryDefinition, path string) (LibraryMedia, error) {
	m := LibraryMedia{Path: path, Disc: &LibraryDisc{Directories: []DiscNode{}, Files: []LibraryMedia{}}}
	if !validRelative(path) || libraryDiscExcluded(d, path) {
		return m, ErrPath
	}
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return m, err
	}
	defer r.Close()
	var rootNode LibraryNode
	if path == "." {
		rootNode, _, err = libraryNode(r, ".")
	} else {
		p, n, e := anchoredParent(r, path, false)
		if e != nil {
			return m, e
		}
		rootNode, _, err = libraryNode(p, n)
		p.Close()
	}
	if err != nil {
		return m, err
	}
	root, err := discRoot(r, path)
	if err != nil {
		return m, err
	}
	defer root.Close()
	info, err := root.Stat()
	if err != nil || Identity(info) != rootNode.Identity {
		return m, ErrState
	}
	// Only this directory receives metadata and private backup stages. Ignore
	// its mutable timestamps, not its pinned identity or parent binding.
	m.Node = LibraryNode{Identity: rootNode.Identity, Parent: rootNode.Parent}
	m.Disc.Directories = append(m.Disc.Directories, DiscNode{Path: ".", Directory: true, Node: m.Node})
	visited := 1
	var walk func(*os.File, string, int) error
	walk = func(dir *os.File, prefix string, depth int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if depth > 64 {
			return ErrLimit
		}
		before, _, err := libraryNode(dir, ".")
		if err != nil {
			return err
		}
		for {
			names, err := dir.ReadDir(128)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			for _, entry := range names {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				visited++
				if visited > libraryDiscNodes {
					return ErrLimit
				}
				name := entry.Name()
				relative := filepath.Join(prefix, name)
				if !validRelative(relative) {
					return ErrPath
				}
				node, kind, err := libraryNode(dir, name)
				if err != nil {
					return err
				}
				if prefix == "" && libraryDiscMutable(d, path, name) && (kind == unix.S_IFREG || kind == unix.S_IFLNK || strings.HasPrefix(name, ".nastool-library-") || strings.HasPrefix(name, ".nastool-sidecar-")) {
					continue
				}
				lower := strings.ToLower(name)
				if strings.HasPrefix(name, ".nastool-") || strings.Contains(lower, ".!qb") || strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".aria2") || strings.HasSuffix(lower, ".tmp") {
					return ErrPath
				}
				if kind == unix.S_IFDIR {
					child, err := discDir(dir, name)
					if err != nil {
						return err
					}
					info, err := child.Stat()
					if err != nil || Identity(info) != node.Identity {
						child.Close()
						return ErrState
					}
					m.Disc.Directories = append(m.Disc.Directories, DiscNode{Path: relative, Directory: true, Node: node})
					err = walk(child, relative, depth+1)
					child.Close()
					if err != nil {
						return err
					}
				} else if kind == unix.S_IFREG || kind == unix.S_IFLNK {
					leaf, err := captureLibraryFile(d, filepath.Join(path, relative))
					if err != nil || leaf.Node != node {
						return ErrState
					}
					m.Disc.Files = append(m.Disc.Files, leaf)
				} else {
					return ErrPath
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
		after, _, err := libraryNode(dir, ".")
		if err != nil || after != before {
			return ErrState
		}
		return nil
	}
	if err := walk(root, "", 0); err != nil {
		return m, err
	}
	m.Disc.visited = visited
	// Detect a root moved/replaced during the walk, even when all original
	// descriptors still exist. Root timestamps may differ between outputs.
	current, err := discRoot(r, path)
	if err != nil {
		return m, err
	}
	info, err = current.Stat()
	current.Close()
	if err != nil || Identity(info) != m.Node.Identity {
		return m, ErrState
	}
	sort.Slice(m.Disc.Directories, func(i, j int) bool { return m.Disc.Directories[i].Path < m.Disc.Directories[j].Path })
	sort.Slice(m.Disc.Files, func(i, j int) bool { return m.Disc.Files[i].Path < m.Disc.Files[j].Path })
	if err := libraryDiscLayout(path, *m.Disc); err != nil {
		return m, err
	}
	return m, nil
}
func libraryDiscLayout(root string, disc LibraryDisc) error {
	dirs := map[string]bool{}
	files := map[string]bool{}
	for _, d := range disc.Directories {
		dirs[filepath.ToSlash(d.Path)] = true
	}
	for _, f := range disc.Files {
		relative, err := filepath.Rel(root, f.Path)
		if err != nil || !validRelative(relative) {
			return ErrPath
		}
		files[filepath.ToSlash(relative)] = true
	}
	direct := func(prefix string) bool {
		bd := filepath.ToSlash(filepath.Join(prefix, "BDMV"))
		if !dirs[bd] || !dirs[bd+"/STREAM"] || !files[bd+"/index.bdmv"] {
			return false
		}
		for f := range files {
			if strings.HasPrefix(f, bd+"/STREAM/") && strings.EqualFold(filepath.Ext(f), ".m2ts") {
				return true
			}
		}
		return false
	}
	if direct("") {
		return nil
	}
	volumes := 0
	for p := range dirs {
		if p != "." && !strings.Contains(p, "/") && discVolumeName.MatchString(p) {
			if !direct(p) {
				return ErrState
			}
			volumes++
		}
	}
	if volumes == 0 {
		return ErrState
	}
	return nil
}

func verifyLibraryDisc(ctx context.Context, d LibraryDefinition, m LibraryMedia) error {
	if err := libraryDiscDefinition(d, m); err != nil {
		return err
	}
	actual, err := captureLibraryDisc(ctx, d, m.Path)
	if err != nil {
		return err
	}
	actual.Disc.Expanded = m.Disc.Expanded
	if Digest(actual) != Digest(m) {
		return ErrState
	}
	return nil
}

func libraryReadDiscNFO(ctx context.Context, d LibraryDefinition, m LibraryMedia, relative string) ([]byte, error) {
	// Metadata links use the configured-root policy of payload links. Never
	// follow an arbitrary NFO target merely because it is beside a disc.
	leaf, err := captureLibraryFile(d, relative)
	if err != nil {
		return nil, err
	}
	if leaf.Input.Size < 0 || leaf.Input.Size > 2<<20 {
		return nil, ErrLimit
	}
	root := d.Root
	if leaf.InputRoot != root.ID {
		var ok bool
		root, ok = libraryRoot(d.Allowed, leaf.InputRoot)
		if !ok {
			return nil, ErrPath
		}
	}
	r, err := anchoredRoot(root.Path, root.Identity)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	p, n, err := anchoredParent(r, leaf.InputPath, false)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	f, err := openRegular(p, n)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	node, kind, err := libraryNode(p, n)
	info, e := f.Stat()
	if err != nil || e != nil || kind != unix.S_IFREG || node != leaf.Input || Identity(info) != leaf.Input.Identity {
		return nil, ErrState
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: f}, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, ErrLimit
	}
	after, err := captureLibraryFile(d, relative)
	if err != nil || Digest(after) != Digest(leaf) || VerifyLibraryMedia(ctx, d, m) != nil {
		return nil, ErrState
	}
	return data, nil
}
