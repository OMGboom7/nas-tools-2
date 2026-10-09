//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

const guardDirectory = ".nastool-organization-locks"

type JobGuard struct {
	file *os.File
	once sync.Once
}

// Close never unlinks the lock. Removing its name could let a later actor lock
// a different inode while an existing actor still holds the original lock.
func (g *JobGuard) Close() {
	if g == nil {
		return
	}
	g.once.Do(func() { _ = unix.Flock(int(g.file.Fd()), unix.LOCK_UN); _ = g.file.Close() })
}

// AcquireJob rejects an active peer immediately; it is not a timeout lease or
// evidence that an old/uncooperative executor has stopped. All new production
// mutation paths must hold this guard through filesystem + ledger operations.
func (s *Store) AcquireJob(ctx context.Context, id string) (*JobGuard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := anchoredRoot(s.guardRoot, s.guardRootIdentity)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err = unix.Mkdirat(int(root.Fd()), guardDirectory, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	fd, err := unix.Openat(int(root.Fd()), guardDirectory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrPath
	}
	parent := os.NewFile(uintptr(fd), guardDirectory)
	defer parent.Close()
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0700 {
		return nil, ErrPath
	}
	parentIdentity := statIdentity(stat)
	name := Digest(id) + ".lock"
	lockFD, err := unix.Openat(fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, ErrPath
	}
	file := os.NewFile(uintptr(lockFD), name)
	if err = unix.Fstat(lockFD, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0777 != 0600 || stat.Nlink != 1 || stat.Size != 0 {
		file.Close()
		return nil, ErrPath
	}
	fileIdentity := statIdentity(stat)
	if err = unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, modeOperationError(err)
	}
	guard := &JobGuard{file: file}
	if err = ctx.Err(); err != nil {
		guard.Close()
		return nil, err
	}
	// Reject a namespace replacement while opening/acquiring the lock.
	fresh, err := anchoredRoot(s.guardRoot, s.guardRootIdentity)
	if err != nil {
		guard.Close()
		return nil, err
	}
	defer fresh.Close()
	stat, err = lstatAt(fresh, guardDirectory)
	if err != nil || statIdentity(stat) != parentIdentity {
		guard.Close()
		return nil, ErrPath
	}
	stat, err = lstatAt(parent, name)
	if err != nil || statIdentity(stat) != fileIdentity {
		guard.Close()
		return nil, ErrPath
	}
	return guard, nil
}
