//go:build !windows

package diskusage

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

func Read(path string) (Space, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Space{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Space{}, fmt.Errorf("unsupported filesystem for %q", path)
	}
	var usage syscall.Statfs_t
	if err := syscall.Statfs(path, &usage); err != nil {
		return Space{}, err
	}
	return Space{
		Volume:         strconv.FormatUint(uint64(stat.Dev), 10),
		TotalBytes:     float64(usage.Blocks) * float64(usage.Bsize),
		AvailableBytes: float64(usage.Bavail) * float64(usage.Bsize),
	}, nil
}
