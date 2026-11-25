//go:build linux || darwin

package mediaarchive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const Supported = true

func openDirectory(base string, components []string, create bool) (*os.File, error) {
	// Resolve the operator-configured root, then pin its directory chain.
	// The two fixed children of the default root must be actual directories.
	// Existing configured symlinks are resolved as before. For a missing
	// custom directory, create the absent suffix relative to its pinned parent.
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	if create {
		for {
			_, err := os.Lstat(base)
			if !errors.Is(err, os.ErrNotExist) {
				break
			}
			components = append([]string{filepath.Base(base)}, components...)
			base = filepath.Dir(base)
		}
	}
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.FieldsFunc(absolute, func(c rune) bool { return c == '/' })
	for i, part := range append(parts, components...) {
		next, e := unix.Openat(fd, part, flags, 0)
		if errors.Is(e, unix.ENOENT) && create && i >= len(parts) {
			if e = unix.Mkdirat(fd, part, 0700); e == nil || errors.Is(e, unix.EEXIST) {
				next, e = unix.Openat(fd, part, flags, 0)
			}
		}
		unix.Close(fd)
		if e != nil {
			return nil, e
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), filepath.Join(append([]string{absolute}, components...)...)), nil
}

func archiveFile(dir *os.File, parent unix.Stat_t, name string) (File, error) {
	date := timestamp(name)
	if date == "" {
		return File{}, ErrChanged
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return File{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return File{}, ErrChanged
	}
	proof := fmt.Sprintf("%d:%d:%s:%d:%d:%d:%d:%d", parent.Dev, parent.Ino, name, stat.Dev, stat.Ino, stat.Size, stat.Ctim.Sec, stat.Ctim.Nsec)
	return File{Name: name, Time: date, Size: stat.Size, Proof: fmt.Sprintf("%x", sha256.Sum256([]byte(proof)))}, nil
}

func List(ctx context.Context, dir *os.File, after string) (files []File, next string, err error) {
	var parent unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &parent); err != nil {
		return nil, "", err
	}
	// Retain only the newest page plus one lookahead, even in large folders.
	for {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		names, err := dir.Readdirnames(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, "", err
		}
		for _, name := range names {
			if !ValidName(name) || after != "" && name >= after {
				continue
			}
			f, e := archiveFile(dir, parent, name)
			if errors.Is(e, os.ErrNotExist) || errors.Is(e, ErrChanged) {
				continue
			}
			if e != nil {
				return nil, "", e
			}
			files = append(files, f)
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
		if len(files) > PageSize+1 {
			files = files[:PageSize+1]
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if len(files) > PageSize {
		files = files[:PageSize]
		next = files[len(files)-1].Name
	}
	return files, next, nil
}

func Remove(dir *os.File, name, confirmation, proof string) error {
	if !ValidName(name) || confirmation != name {
		return ErrChanged
	}
	var parent unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &parent); err != nil {
		return err
	}
	file, err := archiveFile(dir, parent, name)
	if err != nil {
		return err
	}
	if proof != "" && proof != file.Proof {
		return ErrChanged
	}
	// unlinkat with flags zero cannot remove directories or follow a leaf link.
	return unix.Unlinkat(int(dir.Fd()), name, 0)
}
