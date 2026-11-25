//go:build linux || darwin

package organization

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func openDiskRoot(root DiskRoot) (*os.File, error) {
	if root.Configured != "" {
		actual, err := filepath.EvalSymlinks(root.Configured)
		if err != nil || actual != root.Path {
			return nil, ErrPath
		}
	}
	return anchoredRoot(root.Path, root.Identity)
}

func diskSHA1(ctx context.Context, f *os.File, size int64, fast bool) (string, error) {
	h := sha1.New()
	const block = int64(1 << 20)
	copyPart := func(at, n int64) error {
		count, err := io.Copy(h, contextReader{ctx, io.NewSectionReader(f, at, n)})
		if err == nil && count != n {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	if !fast {
		if err := copyPart(0, size); err != nil {
			return "", err
		}
	} else {
		if err := copyPart(0, min(size, block)); err != nil {
			return "", err
		}
		if size > 2*block {
			if err := copyPart(size/2, block); err != nil {
				return "", err
			}
			if err := copyPart(size-block, block); err != nil {
				return "", err
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func diskFingerprint(ctx context.Context, root *os.File, file DiskFile, cache map[string]DiskCacheRow, fast bool) (string, error) {
	p, n, err := anchoredParent(root, file.Path, false)
	if err != nil {
		return "", err
	}
	defer p.Close()
	node, kind, err := libraryNode(p, n)
	if err != nil || kind != unix.S_IFREG || node != file.Node {
		return "", ErrState
	}
	path := filepath.Join(root.Name(), file.Path)
	stamp := Digest([]any{node, fast})
	if row, ok := cache[path]; ok && row.Stamp == stamp && len(row.SHA1) == 40 {
		return row.SHA1, nil
	}
	f, err := openRegular(p, n)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || Identity(info) != node.Identity {
		return "", ErrState
	}
	hash, err := diskSHA1(ctx, f, node.Size, fast)
	if err != nil {
		return "", err
	}
	latest, _, err := libraryNode(p, n)
	if err != nil || latest != node {
		return "", ErrState
	}
	mt := time.Unix(0, node.Modified).Local()
	stampTime := mt.Format("2006-01-02 15:04:05")
	if mt.Nanosecond()/1000 != 0 {
		stampTime = mt.Format("2006-01-02 15:04:05.000000")
	}
	cache[path] = DiskCacheRow{path, node.Size, stampTime, hash, stamp}
	return hash, nil
}

// Compare within each configured directory and filesystem. Read same-sized
// files only, and avoid rehashing hard links to the same inode.
func ScanDiskDuplicates(ctx context.Context, root DiskRoot, exts []string, minSize int64, fast, dry bool, cache map[string]DiskCacheRow) (DiskScan, error) {
	var s DiskScan
	pinned, err := openDiskRoot(root)
	if err != nil {
		return s, err
	}
	defer pinned.Close()
	tree, err := os.OpenRoot(root.Path)
	if err != nil {
		return s, err
	}
	defer tree.Close()
	st, err := tree.Stat(".")
	if err != nil || Identity(st) != root.Identity {
		return s, ErrState
	}
	type bucket struct {
		dev  string
		size int64
	}
	bySize := map[bucket][]DiskFile{}
	err = fs.WalkDir(tree.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			s.Failed++
			return nil
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".nastool-") {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !slices.Contains(exts, strings.ToLower(filepath.Ext(name))) {
			return nil
		}
		p, n, err := anchoredParent(pinned, name, false)
		if err != nil {
			s.Failed++
			return nil
		}
		node, kind, err := libraryNode(p, n)
		p.Close()
		if err != nil || kind != unix.S_IFREG {
			s.Failed++
			return nil
		}
		if node.Size >= minSize {
			s.Files++
			k := bucket{diskDevice(node), node.Size}
			bySize[k] = append(bySize[k], DiskFile{name, node})
		}
		return nil
	})
	if err != nil {
		return s, err
	}
	for _, group := range bySize {
		if len(group) < 2 {
			continue
		}
		if !slices.ContainsFunc(group, func(other DiskFile) bool { return other.Node.Identity != group[0].Node.Identity }) {
			s.ExistingLinks += len(group) - 1
			continue
		}
		sums := map[string]string{}
		buckets := map[string][]DiskFile{}
		for _, f := range group {
			if err := ctx.Err(); err != nil {
				return s, err
			}
			hash, ok := sums[f.Node.Identity]
			if !ok {
				hash, err = diskFingerprint(ctx, pinned, f, cache, fast)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return s, err
					}
					s.Failed++
					continue
				}
				sums[f.Node.Identity] = hash
			}
			buckets[hash] = append(buckets[hash], f)
		}
		for _, candidates := range buckets {
			if len(candidates) < 2 {
				continue
			}
			leaders := map[string]DiskFile{}
			full := map[string]string{}
			for _, f := range candidates {
				key := ""
				if fast && !dry {
					var ok bool
					key, ok = full[f.Node.Identity]
					if !ok {
						// Split sampled collisions before choosing a keeper for each group.
						key, err = diskFingerprint(ctx, pinned, f, map[string]DiskCacheRow{}, false)
						if err != nil {
							s.Failed++
							continue
						}
						full[f.Node.Identity] = key
					}
				}
				first, ok := leaders[key]
				if !ok {
					leaders[key] = f
					continue
				}
				if first.Node.Identity == f.Node.Identity {
					s.ExistingLinks++
					continue
				}
				s.Pairs = append(s.Pairs, DiskPair{root, first, f})
			}
		}
	}
	sort.Slice(s.Pairs, func(i, j int) bool { return s.Pairs[i].Duplicate.Path < s.Pairs[j].Duplicate.Path })
	return s, nil
}
