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

func discDir(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func discRoot(base *os.File, path string) (*os.File, error) {
	if path == "." {
		return discDir(base, ".")
	}
	p, n, err := anchoredParent(base, path, false)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return discDir(p, n)
}
func readDiscManifest(ctx context.Context, base, path string) (DiscManifest, error) {
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil {
		return DiscManifest{}, err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return DiscManifest{}, err
	}
	r, err := anchoredRoot(canonical, Identity(info))
	if err != nil {
		return DiscManifest{}, err
	}
	defer r.Close()
	if !validRelative(path) {
		return DiscManifest{}, ErrPath
	}
	var node LibraryNode
	if path == "." {
		node, _, err = libraryNode(r, ".")
	} else {
		p, n, e := anchoredParent(r, path, false)
		if e != nil {
			return DiscManifest{}, e
		}
		node, _, err = libraryNode(p, n)
		p.Close()
	}
	if err != nil {
		return DiscManifest{}, err
	}
	f, err := discRoot(r, path)
	if err != nil {
		return DiscManifest{}, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || Identity(info) != node.Identity {
		return DiscManifest{}, ErrState
	}
	nodes, err := discWalk(ctx, f, false)
	if err != nil {
		return DiscManifest{}, err
	}
	var after LibraryNode
	if path == "." {
		after, _, err = libraryNode(r, ".")
	} else {
		p, n, e := anchoredParent(r, path, false)
		if e != nil {
			return DiscManifest{}, e
		}
		after, _, err = libraryNode(p, n)
		p.Close()
	}
	if err != nil || after != node {
		return DiscManifest{}, ErrState
	}
	clips := 0
	catalogs := 0
	for _, n := range nodes {
		if n.Directory {
			continue
		}
		p := filepath.ToSlash(n.Path)
		if strings.HasSuffix(p, "BDMV/index.bdmv") {
			catalogs++
		}
		if strings.Contains("/"+p, "/BDMV/STREAM/") && strings.EqualFold(filepath.Ext(p), ".m2ts") {
			clips++
		}
	}
	if catalogs == 0 || clips == 0 {
		return DiscManifest{}, ErrState
	}
	return DiscManifest{Root: node, Nodes: nodes}, nil
}
func discWalk(ctx context.Context, root *os.File, links bool) ([]DiscNode, error) {
	result := []DiscNode{}
	visited := 0
	var walk func(*os.File, string, int) error
	walk = func(dir *os.File, prefix string, depth int) error {
		if depth > 64 {
			return ErrLimit
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		before, _, err := libraryNode(dir, ".")
		if err != nil {
			return err
		}
		for {
			entries, err := dir.ReadDir(128)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			for _, entry := range entries {
				visited++
				if visited > 20000 {
					return ErrLimit
				}
				path := filepath.Join(prefix, entry.Name())
				if !validRelative(path) || strings.HasPrefix(entry.Name(), ".nastool-") {
					return ErrPath
				}
				lower := strings.ToLower(entry.Name())
				if strings.Contains(lower, ".!qb") || strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".aria2") || strings.HasSuffix(lower, ".tmp") {
					return ErrState
				}
				node, kind, err := libraryNode(dir, entry.Name())
				if err != nil {
					return err
				}
				if node.Size < 0 {
					return ErrState
				}
				if kind == unix.S_IFDIR {
					result = append(result, DiscNode{Path: path, Directory: true, Node: node})
					child, err := discDir(dir, entry.Name())
					if err != nil {
						return err
					}
					if info, e := child.Stat(); e != nil || Identity(info) != node.Identity {
						child.Close()
						return ErrState
					}
					err = walk(child, path, depth+1)
					child.Close()
					if err != nil {
						return err
					}
				} else if kind == unix.S_IFREG || links && kind == unix.S_IFLNK {
					result = append(result, DiscNode{Path: path, Node: node})
				} else {
					return ErrPath
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
		after, _, err := libraryNode(dir, ".")
		if err != nil || before != after {
			return ErrState
		}
		return nil
	}
	copy, err := discDir(root, ".")
	if err != nil {
		return nil, err
	}
	defer copy.Close()
	if err := walk(copy, "", 0); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}
