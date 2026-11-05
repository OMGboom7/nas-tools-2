//go:build darwin || linux

package backuprestore

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func owned(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(os.Geteuid())
}

func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, ErrState
	}
	return f, nil
}

func (w Workspace) lock(exclusive bool) (*os.File, error) {
	return w.lockFile(".nastool-restore.lock", exclusive)
}

func (w Workspace) lockFile(name string, exclusive bool) (*os.File, error) {
	path := filepath.Join(w.root, name)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Nlink != 1 || st.Size != 0 || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, ErrState
	}
	kind := unix.LOCK_SH
	if exclusive {
		kind = unix.LOCK_EX
	}
	if err := unix.Flock(fd, kind|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	var current unix.Stat_t
	if err := unix.Lstat(path, &current); err != nil || current.Dev != st.Dev || current.Ino != st.Ino {
		f.Close()
		return nil, ErrState
	}
	return f, nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
