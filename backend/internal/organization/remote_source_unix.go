//go:build linux || darwin

package organization

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func remoteSourcePath(e Entry, f RemoteFile) string {
	if e.Disc == nil {
		return e.Source
	}
	return filepath.Join(e.Source, f.Path)
}
func remoteSources(ctx context.Context, d Definition, e Entry) ([]RemoteFile, []string, error) {
	if strings.Contains(e.Target, ".rclonelink") {
		return nil, nil, ErrMode
	}
	r, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()
	files := []RemoteFile{}
	dirs := []string{}
	if e.Disc == nil {
		p, n, err := anchoredParent(r, e.Source, false)
		if err != nil {
			return nil, nil, err
		}
		defer p.Close()
		node, kind, err := libraryNode(p, n)
		if err != nil || kind != unix.S_IFREG || node.Identity != e.Identity || node.Size != e.Size || node.Modified != mustModified(e.Modified) || time.Since(time.Unix(0, node.Modified)) < 30*time.Second {
			return nil, nil, ErrState
		}
		files = append(files, RemoteFile{Path: ".", Size: e.Size, Source: node})
	} else {
		m, err := discManifest(d, e)
		if err != nil {
			return nil, nil, err
		}
		root, err := discSource(ctx, d, e)
		if err != nil {
			return nil, nil, err
		}
		defer root.Close()
		actual, err := discWalk(ctx, root, false)
		if err != nil || Digest(actual) != Digest(m.Nodes) {
			return nil, nil, ErrState
		}
		dirs = append(dirs, ".")
		for _, n := range m.Nodes {
			if strings.HasSuffix(n.Path, ".rclonelink") {
				return nil, nil, ErrMode
			}
			if n.Directory {
				dirs = append(dirs, n.Path)
			} else {
				if time.Since(time.Unix(0, n.Node.Modified)) < 30*time.Second {
					return nil, nil, ErrState
				}
				files = append(files, RemoteFile{Path: n.Path, Size: n.Node.Size, Source: n.Node})
			}
		}
	}
	for i := range files {
		f := &files[i]
		err := readRemoteSource(ctx, d, e, *f, func(file io.Reader) error {
			hash, err := hashReader(ctx, file, f.Size)
			if err == nil {
				f.Digest = hash
			}
			return err
		})
		if err != nil {
			return nil, nil, err
		}
	}
	return files, dirs, nil
}
func readRemoteSource(ctx context.Context, d Definition, e Entry, want RemoteFile, consume func(io.Reader) error) error {
	if consume == nil || ctx.Err() != nil {
		return ErrState
	}
	r, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return err
	}
	defer r.Close()
	p, n, err := anchoredParent(r, remoteSourcePath(e, want), false)
	if err != nil {
		return err
	}
	defer p.Close()
	before, kind, err := libraryNode(p, n)
	if err != nil || kind != unix.S_IFREG || before != want.Source {
		return ErrState
	}
	f, err := openRegular(p, n)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || Identity(info) != want.Source.Identity {
		return ErrState
	}
	if err := consume(contextReader{ctx: ctx, reader: io.LimitReader(f, want.Size+1)}); err != nil {
		return err
	}
	after, _, err := libraryNode(p, n)
	if err != nil || after != before {
		return ErrState
	}
	return nil
}
func mustModified(raw string) int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return -1
	}
	return n
}
func hashReader(ctx context.Context, r io.Reader, size int64) (string, error) {
	return hashRemoteReader(ctx, r, size)
}
