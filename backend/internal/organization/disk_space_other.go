//go:build !linux && !darwin

package organization

import "context"

func ScanDiskDuplicates(context.Context, DiskRoot, []string, int64, bool, bool, map[string]DiskCacheRow) (DiskScan, error) {
	return DiskScan{}, ErrMode
}
func LoadDiskCache(string, string) (DiskCache, error)                                 { return DiskCache{}, ErrMode }
func SaveDiskCache(context.Context, string, DiskCache, map[string]DiskCacheRow) error { return ErrMode }
func ReplaceDiskDuplicate(context.Context, DiskPair, string, int, string, func(string) error, func() error) (bool, error) {
	return false, ErrMode
}
