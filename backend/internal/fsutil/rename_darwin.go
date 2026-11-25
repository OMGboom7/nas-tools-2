//go:build darwin

package fsutil

import "golang.org/x/sys/unix"

func RenameExclusive(from, to string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_EXCL)
}
