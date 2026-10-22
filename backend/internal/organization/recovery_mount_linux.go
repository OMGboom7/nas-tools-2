//go:build linux

package organization

import "golang.org/x/sys/unix"

// st_dev alone misses bind mounts of the same filesystem. Independent
// destructive traversal requires positive mount IDs, never a permissive
// fallback on older kernels/filesystems which cannot supply that evidence.
func recoverySameMount(parent int, name string) error {
	var before, child unix.Statx_t
	flags := unix.AT_SYMLINK_NOFOLLOW | unix.AT_NO_AUTOMOUNT
	if err := unix.Statx(parent, ".", flags, unix.STATX_MNT_ID, &before); err != nil {
		return ErrMode
	}
	if err := unix.Statx(parent, name, flags, unix.STATX_MNT_ID, &child); err != nil {
		if err == unix.ENOENT {
			return err
		}
		return ErrMode
	}
	if before.Mask&unix.STATX_MNT_ID == 0 || child.Mask&unix.STATX_MNT_ID == 0 || before.Mnt_id != child.Mnt_id {
		return ErrPath
	}
	return nil
}
