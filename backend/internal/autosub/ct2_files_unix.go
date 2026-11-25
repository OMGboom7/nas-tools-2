//go:build linux || darwin

package autosub

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func openModelFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}

// These persistent names live in our versioned cache only. Never open a
// symlink or hard-linked user file for writes. The lock survives by name so a
// concurrent process always serializes publication against the same inode.
func openModelPart(root *os.Root, name string) (*os.File, error) {
	dir, e := root.Open(".")
	if e != nil {
		return nil, e
	}
	defer dir.Close()
	fd, e := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, errModel
	}
	return f, nil
}
func lockModelObject(ctx context.Context, root *os.Root, name string) (*os.File, error) {
	f, e := openModelPart(root, name)
	if e != nil {
		return nil, e
	}
	for {
		e = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if e == nil {
			return f, nil
		}
		if !errors.Is(e, unix.EWOULDBLOCK) {
			f.Close()
			return nil, e
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
func publishModelObject(root *os.Root, tmp, name string, f *os.File) error {
	d, e := root.Open(".")
	if e != nil {
		return e
	}
	defer d.Close()
	expected, e := f.Stat()
	if e != nil {
		return e
	}
	stage, e := root.Lstat(tmp)
	if e != nil || !os.SameFile(expected, stage) {
		return errModel
	}
	if e = unix.Linkat(int(d.Fd()), tmp, int(d.Fd()), name, 0); e != nil {
		return e
	}
	published, e := root.Lstat(name)
	if e != nil || !os.SameFile(expected, published) {
		return errModel
	}
	if e = d.Sync(); e != nil {
		return e
	}
	stage, e = root.Lstat(tmp)
	if e != nil || !os.SameFile(expected, stage) {
		return errModel
	}
	if e = root.Remove(tmp); e != nil {
		return e
	}
	return d.Sync()
}
