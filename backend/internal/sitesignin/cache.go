package sitesignin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

// FileCache retains the original config/temp/signin files and every unknown JSON
// value. A malformed or concurrently changed file is never replaced with {}.
type FileCache struct {
	Directory string
	mu        sync.Mutex
}

func cacheName(name string) bool { return name == "tjupt" || name == "52pt" || name == "chdbits" }

type cacheSnapshot struct {
	raw     []byte
	info    os.FileInfo
	missing bool
}

func (f *FileCache) directory(create bool) (string, error) {
	base, err := filepath.Abs(f.Directory)
	if err != nil || base == string(filepath.Separator) {
		return "", ErrScope
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	current := base
	for _, part := range []string{"temp", "signin"} {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				return current, os.ErrNotExist
			}
			if err = os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", ErrScope
		}
	}
	return current, nil
}
func (f *FileCache) snapshot(name string) (cacheSnapshot, error) {
	if !cacheName(name) {
		return cacheSnapshot{}, ErrScope
	}
	dir, err := f.directory(false)
	if errors.Is(err, os.ErrNotExist) {
		return cacheSnapshot{missing: true}, nil
	}
	if err != nil {
		return cacheSnapshot{}, err
	}
	path := filepath.Join(dir, name+".json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return cacheSnapshot{missing: true}, nil
	}
	if err != nil {
		return cacheSnapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4<<20 {
		return cacheSnapshot{}, ErrScope
	}
	file, err := os.Open(path)
	if err != nil {
		return cacheSnapshot{}, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return cacheSnapshot{}, ErrScope
	}
	raw, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return cacheSnapshot{}, ErrResponse
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return cacheSnapshot{}, ErrScope
	}
	return cacheSnapshot{raw: raw, info: info}, nil
}
func cacheToken(s cacheSnapshot) string {
	if s.missing {
		return sitelogin.Hash("original-signin-cache-missing")
	}
	return sitelogin.Hash([]any{string(s.raw), s.info.Size(), s.info.ModTime().UnixNano(), s.info.Mode().String(), cacheIdentity(s.info)})
}
func rawCache(s cacheSnapshot) (map[string]json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	if s.missing {
		return values, nil
	}
	if json.Unmarshal(s.raw, &values) != nil || values == nil {
		return nil, ErrResponse
	}
	return values, nil
}
func (f *FileCache) Read(ctx context.Context, name string) (map[string][]string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s, err := f.snapshot(name)
	if err != nil {
		return nil, "", err
	}
	values, err := rawCache(s)
	if err != nil {
		return nil, "", err
	}
	out := map[string][]string{}
	valueString := func(raw json.RawMessage) (string, bool) {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value, true
		}
		var number json.Number
		if json.Unmarshal(raw, &number) == nil {
			if _, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
				return number.String(), true
			}
		}
		return "", false
	}
	for key, raw := range values {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			list = []json.RawMessage{raw}
		}
		converted := []string{}
		for _, v := range list {
			str, ok := valueString(v)
			if !ok || len(str) > 12000 {
				converted = nil
				break
			}
			converted = append(converted, str)
		}
		if len(converted) > 0 {
			out[key] = converted
		}
	}
	return out, cacheToken(s), nil
}
func (f *FileCache) Put(ctx context.Context, name, token, key string, value []string, guard func(context.Context) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if guard == nil || guard(ctx) != nil || key == "" || len(key) > 12000 || len(value) == 0 || len(value) > 64 {
		return ErrScope
	}
	for _, v := range value {
		if len(v) > 12000 || strings.ContainsRune(v, 0) {
			return ErrScope
		}
	}
	before, err := f.snapshot(name)
	if err != nil || cacheToken(before) != token {
		return ErrScope
	}
	values, err := rawCache(before)
	if err != nil {
		return err
	}
	if name == "tjupt" {
		if len(value) != 1 {
			return ErrScope
		}
		values[key], err = json.Marshal(value[0])
	} else {
		values[key], err = json.Marshal(value)
	}
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(values, "", "    ")
	if err != nil || len(raw) > 4<<20 {
		return ErrResponse
	}
	dir, err := f.directory(true)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".go-signin-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	mode := os.FileMode(0600)
	if before.info != nil {
		mode = before.info.Mode().Perm()
	}
	if err = temp.Chmod(mode); err == nil {
		_, err = temp.Write(raw)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	current, err := f.snapshot(name)
	if err != nil || cacheToken(current) != token || guard(ctx) != nil {
		return ErrScope
	}
	if err = os.Rename(tempName, filepath.Join(dir, name+".json")); err != nil {
		return err
	}
	after, err := f.snapshot(name)
	if err != nil || string(after.raw) != string(raw) {
		return ErrUnknown
	}
	return nil
}
