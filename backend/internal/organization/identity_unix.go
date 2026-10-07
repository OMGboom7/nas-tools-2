//go:build linux || darwin

package organization

import (
	"fmt"
	"io/fs"
	"syscall"
)

func Identity(info fs.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

const CopySupported = true
