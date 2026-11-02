//go:build unix

package sitesignin

import (
	"os"
	"syscall"
)

func cacheIdentity(info os.FileInfo) any {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return []uint64{uint64(stat.Dev), uint64(stat.Ino)}
	}
	return nil
}
