//go:build darwin

package organization

import "golang.org/x/sys/unix"

func recoverySameMount(parent int, name string) error {
	var before, child unix.Stat_t
	if unix.Fstat(parent, &before) != nil {
		return ErrPath
	}
	if err := unix.Fstatat(parent, name, &child, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if before.Dev != child.Dev {
		return ErrPath
	}
	return nil
}
