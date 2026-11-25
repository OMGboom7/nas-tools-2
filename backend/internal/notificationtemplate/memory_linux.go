//go:build linux

package notificationtemplate

import "golang.org/x/sys/unix"

// Bound writable anonymous mappings in the disposable Linux worker. A
// template filter may allocate intermediate data before the output writer runs.
func limitRendererMemory() error {
	const budget = 192 << 20
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_DATA, &lim); err != nil {
		return err
	}
	if lim.Cur <= budget {
		return nil
	}
	lim.Cur = budget
	return unix.Setrlimit(unix.RLIMIT_DATA, &lim)
}
