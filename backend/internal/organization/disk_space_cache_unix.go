//go:build linux || darwin

package organization

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

func readDiskCache(parent *os.File, name string) (DiskCache, error) {
	var data DiskCache
	f, err := openRegular(parent, name)
	if err != nil {
		return data, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	if err := dec.Decode(&data); err != nil {
		return data, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF || data.Rows == nil {
		return data, ErrState
	}
	return data, nil
}

func cacheDirectory(configFile string, create bool) (*os.File, string, error) {
	root, err := FreezeDiskRoot(filepath.Dir(configFile))
	if err != nil {
		return nil, "", err
	}
	dir, err := anchoredRoot(root.Path, root.Identity)
	if err != nil {
		return nil, "", err
	}
	for _, component := range []string{"plugins", "DiskSpaceSaver"} {
		fd := int(dir.Fd())
		if create {
			if err := unix.Mkdirat(fd, component, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				dir.Close()
				return nil, "", err
			}
			if err := dir.Sync(); err != nil {
				dir.Close()
				return nil, "", err
			}
		}
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		dir.Close()
		if err != nil {
			return nil, "", err
		}
		dir = os.NewFile(uintptr(next), component)
	}
	return dir, "sha1.json", nil
}

// The old result_path is an import source. Keep that file available to the
// original installation and publish future cache updates in the plugin folder.
func LoadDiskCache(configFile, legacy string) (DiskCache, error) {
	dir, name, err := cacheDirectory(configFile, false)
	if err == nil {
		defer dir.Close()
		data, e := readDiskCache(dir, name)
		if !errors.Is(e, os.ErrNotExist) {
			return data, e
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return DiskCache{}, err
	}
	if legacy != "" {
		if !filepath.IsAbs(legacy) {
			return DiskCache{}, ErrPath
		}
		base, e := FreezeDiskRoot(filepath.Dir(legacy))
		if e == nil {
			p, e := anchoredRoot(base.Path, base.Identity)
			if e != nil {
				return DiskCache{}, e
			}
			defer p.Close()
			data, e := readDiskCache(p, filepath.Base(legacy))
			if !errors.Is(e, os.ErrNotExist) {
				return data, e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return DiskCache{}, e
		}
	}
	return DiskCache{Rows: []DiskCacheRow{}, Inodes: json.RawMessage("[]")}, nil
}
func SaveDiskCache(ctx context.Context, configFile string, data DiskCache, rows map[string]DiskCacheRow) error {
	dir, name, err := cacheDirectory(configFile, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	data.Rows = make([]DiskCacheRow, 0, len(rows))
	for _, row := range rows {
		data.Rows = append(data.Rows, row)
	}
	slices.SortFunc(data.Rows, func(a, b DiskCacheRow) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	tmp := ".sha1-" + hex.EncodeToString(token[:])
	fd := int(dir.Fd())
	out, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(fd, tmp, 0)
	f := os.NewFile(uintptr(out), tmp)
	err = json.NewEncoder(f).Encode(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if st, e := lstatAt(dir, name); e == nil && st.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrPath
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if err := unix.Renameat(fd, tmp, fd, name); err != nil {
		return err
	}
	return dir.Sync()
}
