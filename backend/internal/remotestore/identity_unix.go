//go:build linux || darwin

package remotestore

import (
	"fmt"
	"golang.org/x/sys/unix"
)

// This is only an in-process digest-cache key, never a durable executable
// identity. Exclude atime: our own reads/execution must not invalidate it.
func binaryChangeStamp(name string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Stat(name, &stat); err != nil {
		return "", ErrUnavailable
	}
	return fmt.Sprintf("%d:%d:%d:%d", stat.Dev, stat.Ino, stat.Ctim.Sec, stat.Ctim.Nsec), nil
}
