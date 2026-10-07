// Package organization provides the filesystem side of the native organizer.
// Previewing never creates, opens for writing, moves, or removes media files.
package organization

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrPath = errors.New("invalid or changed organization path")
var ErrLimit = errors.New("organization scan exceeded limits")

type File struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

type ScanResult struct {
	Files   []File `json:"files"`
	Skipped int    `json:"skipped"`
}

var extensions = map[string]string{}

func init() {
	for kind, list := range map[string]string{
		"media":    ".mp4 .mkv .ts .iso .rmvb .avi .mov .mpeg .mpg .wmv .3gp .asf .m4v .flv .m2ts .strm .tp .f4v",
		"subtitle": ".srt .ass .ssa",
		"audio":    ".mka .flac .ape .wav",
	} {
		for _, ext := range strings.Fields(list) {
			extensions[ext] = kind
		}
	}
}

func validRelative(name string) bool {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || filepath.IsAbs(name) || strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	if name == "." {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Roots come from administrator configuration, not a request's arbitrary path.
// Configured root aliases are resolved once; symlinks beneath them are rejected.
func OpenRoot(path string) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return nil, ErrPath
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, ErrPath
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, ErrPath
	}
	return root, nil
}

func noLinks(root *os.Root, name string) (fs.FileInfo, error) {
	if !validRelative(name) {
		return nil, ErrPath
	}
	var info fs.FileInfo
	current := ""
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		var err error
		info, err = root.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, ErrPath
		}
	}
	return info, nil
}

func Scan(ctx context.Context, base, selected string) (ScanResult, error) {
	result := ScanResult{Files: []File{}}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	root, err := OpenRoot(base)
	if err != nil {
		return result, err
	}
	defer root.Close()
	info, err := noLinks(root, selected)
	if err != nil {
		return result, ErrPath
	}
	visited := 0
	var walk func(string, fs.FileInfo, int) error
	walk = func(name string, info fs.FileInfo, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > 20000 || depth > 32 {
			return ErrLimit
		}
		if info.Mode()&fs.ModeSymlink != 0 || strings.HasPrefix(filepath.Base(name), ".") && name != "." {
			result.Skipped++
			return nil
		}
		if !info.IsDir() {
			kind := extensions[strings.ToLower(filepath.Ext(name))]
			if !info.Mode().IsRegular() || kind == "" {
				result.Skipped++
				return nil
			}
			if len(result.Files) >= 1000 {
				return ErrLimit
			}
			result.Files = append(result.Files, File{name, kind, info.Size(), info.ModTime().UnixNano()})
			return nil
		}
		directory, err := root.Open(name)
		if err != nil {
			return ErrPath
		}
		defer directory.Close()
		actual, err := directory.Stat()
		if err != nil || !actual.IsDir() || !os.SameFile(info, actual) {
			return ErrPath
		}
		for {
			entries, readErr := directory.ReadDir(128)
			if readErr != nil && readErr != io.EOF {
				return ErrPath
			}
			for _, entry := range entries {
				if !utf8.ValidString(entry.Name()) || strings.ContainsAny(entry.Name(), "\\\r\n\x00") {
					return ErrPath
				}
				child := filepath.Join(name, entry.Name())
				childInfo, err := root.Lstat(child)
				if err != nil {
					return ErrPath
				}
				if err := walk(child, childInfo, depth+1); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				break
			}
		}
		current, err := root.Lstat(name)
		if err != nil || !os.SameFile(info, current) || !info.ModTime().Equal(current.ModTime()) {
			return ErrPath
		}
		return nil
	}
	if err := walk(selected, info, 0); err != nil {
		return ScanResult{}, err
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	return result, nil
}

// TargetStatus only checks conflicts. "available" is not permission to execute:
// actual execution must revalidate both roots and snapshots under its claim.
func TargetStatus(ctx context.Context, base, name string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if !validRelative(name) || name == "." {
		return "", ErrPath
	}
	root, err := OpenRoot(base)
	if err != nil {
		return "", err
	}
	defer root.Close()
	parts := strings.Split(name, string(filepath.Separator))
	current := ""
	for i, part := range parts {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return "available", nil
		}
		if err != nil {
			return "", ErrPath
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "blocked", nil
		}
		if i == len(parts)-1 {
			return "conflict", nil
		}
		if !info.IsDir() {
			return "blocked", nil
		}
	}
	return "", ErrPath
}
