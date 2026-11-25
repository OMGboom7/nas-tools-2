//go:build linux || darwin

package mediaarchive

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Generate publishes only complete Markdown. The final link is exclusive, even
// when another process creates an archive with the same second in its name.
func Generate(ctx context.Context, configFile, custom string, container bool, name string, keep int, collect func(io.Writer) (int, error), current func() error) (Result, error) {
	var result Result
	if !ValidName(name) || keep < 0 {
		return result, ErrChanged
	}
	if err := current(); err != nil {
		return result, err
	}
	dir, err := directory(configFile, custom, container, true)
	if err != nil {
		return result, err
	}
	defer dir.Close()
	fd := int(dir.Fd())
	tmp := ".nastool-archive-" + rand.Text()
	stage, err := unix.Openat(fd, tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return result, err
	}
	file := os.NewFile(uintptr(stage), tmp)
	defer file.Close()
	sameStage := func() bool {
		var held, named unix.Stat_t
		return unix.Fstat(stage, &held) == nil && unix.Fstatat(fd, tmp, &named, unix.AT_SYMLINK_NOFOLLOW) == nil && held.Dev == named.Dev && held.Ino == named.Ino
	}
	defer func() {
		if sameStage() {
			_ = unix.Unlinkat(fd, tmp, 0)
		}
	}()
	// Check that the configured location still names this directory after the
	// remote scan, and before removing files from the pinned directory.
	validate := func() error {
		if err := current(); err != nil {
			return err
		}
		mapped, err := Open(configFile, custom, container)
		if err != nil {
			return err
		}
		defer mapped.Close()
		held, err := dir.Stat()
		if err != nil {
			return err
		}
		live, err := mapped.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(held, live) {
			return ErrChanged
		}
		return nil
	}
	buf := bufio.NewWriter(file)
	count, err := collect(buf)
	if err != nil || count == 0 {
		return result, err
	}
	if err := buf.Flush(); err != nil {
		return result, err
	}
	if err := file.Sync(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validate(); err != nil {
		return result, err
	}
	if !sameStage() {
		return result, ErrChanged
	}
	if err := unix.Linkat(fd, tmp, fd, name, 0); err != nil {
		return result, err
	}
	result.Name, result.Libraries = name, count
	if err := dir.Sync(); err != nil {
		return result, err
	}

	// Enumerate through the pinned directory. Each page gets its own stream;
	// the last timestamp remains a valid cursor even after pruning its entry.
	cursor := ""
	ordinal := 0
	for {
		pageFD, err := unix.Openat(fd, ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
		if err != nil {
			return result, err
		}
		page := os.NewFile(uintptr(pageFD), dir.Name())
		files, next, err := List(ctx, page, cursor)
		page.Close()
		if err != nil {
			return result, err
		}
		for _, f := range files {
			ordinal++
			if ordinal <= keep {
				continue
			}
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if err := validate(); err != nil {
				return result, err
			}
			if err := Remove(dir, f.Name, f.Name, f.Proof); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return result, err
			}
			result.Removed++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return result, dir.Sync()
}
