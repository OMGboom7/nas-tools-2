//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A journaled private directory contains the new link before the exchange,
// and the displaced file afterwards. Compare full content before the exchange
// and again before unlinking the displaced inode, including during recovery.
func ReplaceDiskDuplicate(ctx context.Context, pair DiskPair, job string, index int, holdID string, record func(string) error, current func() error) (bool, error) {
	if job == "" || index < 0 || record == nil || current == nil || pair.Keep.Node.Identity == pair.Duplicate.Node.Identity || diskDevice(pair.Keep.Node) != diskDevice(pair.Duplicate.Node) {
		return false, ErrState
	}
	root, err := openDiskRoot(pair.Root)
	if err != nil {
		return false, err
	}
	defer root.Close()
	sp, s, err := anchoredParent(root, pair.Keep.Path, false)
	if err != nil {
		return false, err
	}
	defer sp.Close()
	dp, d, err := anchoredParent(root, pair.Duplicate.Path, false)
	if err != nil {
		return false, err
	}
	defer dp.Close()
	si, err := sp.Stat()
	if err != nil || Identity(si) != pair.Keep.Node.Parent {
		return false, ErrPath
	}
	di, err := dp.Stat()
	if err != nil || Identity(di) != pair.Duplicate.Node.Parent {
		return false, ErrPath
	}
	srcFD, dstFD := int(sp.Fd()), int(dp.Fd())
	proof := func(dir *os.File, name string, n LibraryNode) (string, error) {
		f, err := openRegular(dir, name)
		if err != nil {
			return "", err
		}
		defer f.Close()
		same := func() bool {
			st, err := f.Stat()
			return err == nil && Identity(st) == n.Identity && st.Size() == n.Size && st.ModTime().UnixNano() == n.Modified
		}
		if !same() {
			return "", ErrState
		}
		before, kind, err := libraryNode(dir, name)
		if err != nil || before.Identity != n.Identity || kind != unix.S_IFREG {
			return "", ErrState
		}
		h, err := hashSource(ctx, f, n.Size)
		if err != nil {
			return "", err
		}
		after, _, err := libraryNode(dir, name)
		if err != nil || before != after || !same() {
			return "", ErrState
		}
		return h, nil
	}
	source, err := proof(sp, s, pair.Keep.Node)
	if err != nil {
		return false, err
	}
	named, err := lstatAt(dp, d)
	if err != nil {
		return false, err
	}
	published := statIdentity(named) == pair.Keep.Node.Identity
	stage := diskStageName(job, index)
	stagePath := filepath.Join(filepath.Dir(pair.Duplicate.Path), stage)
	if !published {
		keepStat, err := lstatAt(sp, s)
		if err != nil || keepStat.Uid != named.Uid || keepStat.Gid != named.Gid || keepStat.Mode != named.Mode {
			return false, ErrState
		}
		h, e := proof(dp, d, pair.Duplicate.Node)
		if e != nil {
			return false, e
		}
		if h != source {
			return false, nil
		}
	}
	if err := current(); err != nil {
		return false, err
	}
	if holdID == "" {
		if published {
			return false, ErrState
		}
		if err := unix.Mkdirat(dstFD, stage, 0700); err != nil {
			return false, err
		}
	}
	fd, err := unix.Openat(dstFD, stage, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) && published && holdID != "" {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	hold := os.NewFile(uintptr(fd), stagePath)
	defer hold.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&0777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		return false, ErrPath
	}
	id := statIdentity(st)
	if holdID == "" {
		if err := dp.Sync(); err != nil {
			return false, err
		}
		if err := record(id); err != nil {
			return false, err
		}
	} else if holdID != id {
		return false, ErrState
	}
	if !published {
		staged, e := lstatAt(hold, "payload")
		if errors.Is(e, os.ErrNotExist) {
			if err := unix.Linkat(srcFD, s, fd, "payload", 0); err != nil {
				return false, err
			}
			if err := hold.Sync(); err != nil {
				return false, err
			}
			staged, e = lstatAt(hold, "payload")
		}
		if e != nil || statIdentity(staged) != pair.Keep.Node.Identity {
			return false, ErrState
		}
		if err := current(); err != nil {
			return false, err
		}
		live, e := lstatAt(dp, d)
		if e != nil || statIdentity(live) != pair.Duplicate.Node.Identity {
			return false, ErrState
		}
		if err := swapFiles(fd, "payload", dstFD, d); err != nil {
			return false, err
		}
	}
	if err := dp.Sync(); err != nil {
		return false, err
	}
	if err := hold.Sync(); err != nil {
		return false, err
	}
	h, err := proof(dp, d, pair.Keep.Node)
	if err != nil || h != source {
		return false, ErrState
	}
	h, err = proof(hold, "payload", pair.Duplicate.Node)
	if err == nil {
		if h != source {
			return false, ErrState
		}
		if err := current(); err != nil {
			return false, err
		}
		// Reopen the configured parent paths after the long reads. Keep the
		// displaced file if either published name moved during verification.
		for _, file := range []DiskFile{pair.Keep, pair.Duplicate} {
			liveRoot, e := openDiskRoot(pair.Root)
			if e != nil {
				return false, e
			}
			parent, leaf, e := anchoredParent(liveRoot, file.Path, false)
			liveRoot.Close()
			if e != nil {
				return false, e
			}
			node, kind, e := libraryNode(parent, leaf)
			parent.Close()
			if e != nil || kind != unix.S_IFREG || node.Parent != file.Node.Parent || node.Identity != pair.Keep.Node.Identity || node.Size != pair.Keep.Node.Size || node.Modified != pair.Keep.Node.Modified {
				return false, ErrState
			}
		}
		displaced, kind, e := libraryNode(hold, "payload")
		if e != nil || kind != unix.S_IFREG || displaced.Identity != pair.Duplicate.Node.Identity || displaced.Size != pair.Duplicate.Node.Size || displaced.Modified != pair.Duplicate.Node.Modified {
			return false, ErrState
		}
		if err := unix.Unlinkat(fd, "payload", 0); err != nil {
			return false, err
		}
	} else if !errors.Is(err, os.ErrNotExist) || !published {
		return false, ErrState
	}
	if err := hold.Sync(); err != nil {
		return false, err
	}
	outer, err := lstatAt(dp, stage)
	if err != nil || statIdentity(outer) != id {
		return false, ErrPath
	}
	if err := unix.Unlinkat(dstFD, stage, unix.AT_REMOVEDIR); err != nil {
		return false, err
	}
	return true, dp.Sync()
}
