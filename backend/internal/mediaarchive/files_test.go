//go:build linux || darwin

package mediaarchive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveFilesUseOriginalDirectoryAndExactRegularNames(t *testing.T) {
	base := t.TempDir()
	cfg := filepath.Join(base, "config.yaml")
	custom := filepath.Join(base, "custom")
	defaultDir := filepath.Join(base, "archive_file", "media_library")
	for _, p := range []string{custom, defaultDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	const name = "归档_20261125130000.md"
	write := func(p, v string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(defaultDir, name), "default")
	write(filepath.Join(custom, name), "custom")
	write(filepath.Join(custom, "归档_20260231000000.md"), "invalid date")
	write(filepath.Join(custom, "归档_00000101000000.md"), "invalid year")
	write(filepath.Join(custom, "keep.md"), "keep")
	if err := os.Symlink(filepath.Join(custom, "keep.md"), filepath.Join(custom, "归档_20260101000000.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(custom, "归档_20260102000000.md"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		container bool
		size      int64
	}{{true, 7}, {false, 6}} {
		dir, err := Open(cfg, custom, tc.container)
		if err != nil {
			t.Fatal(err)
		}
		files, next, err := List(t.Context(), dir, "")
		dir.Close()
		if err != nil || next != "" || len(files) != 1 || files[0].Name != name || files[0].Size != tc.size || files[0].Time != "2026-11-25 13:00:00" {
			t.Fatal(files, next, err)
		}
	}
	dir, err := Open(cfg, custom, false)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	files, _, err := List(t.Context(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../" + name, "归档_20260101000000.md", "归档_20260102000000.md", "归档_00000101000000.md", "keep.md"} {
		if err := Remove(dir, bad, bad, ""); err == nil {
			t.Fatal("removed unexpected entry", bad)
		}
	}
	if err := Remove(dir, name, "", ""); err == nil {
		t.Fatal("missing confirmation")
	}
	write(filepath.Join(custom, name), "replacement content")
	if err := Remove(dir, name, name, files[0].Proof); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	other, err := Open(cfg, custom, true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := Remove(other, name, name, files[0].Proof); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	if err := Remove(dir, name, name, ""); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, name, name, ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(custom, "keep.md"), filepath.Join(defaultDir, name)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchivePaginationAndDefaultDirectorySymlink(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 201; i++ {
		name := fmt.Sprintf("归档_2026112500%02d%02d.md", i/60, i%60)
		if err := os.WriteFile(filepath.Join(base, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	page := func(cursor string) ([]File, string) {
		t.Helper()
		dir, err := Open("config.yaml", base, false)
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		files, next, err := List(t.Context(), dir, cursor)
		if err != nil {
			t.Fatal(err)
		}
		return files, next
	}
	first, next := page("")
	last, end := page(next)
	if len(first) != 200 || first[0].Name != "归档_20261125000320.md" || next != "归档_20261125000001.md" || len(last) != 1 || last[0].Name != "归档_20261125000000.md" || end != "" {
		t.Fatal(next, end)
	}
	configDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "media_library"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(configDir, "archive_file")); err != nil {
		t.Fatal(err)
	}
	if dir, err := Open(filepath.Join(configDir, "config.yaml"), "", true); err == nil {
		dir.Close()
		t.Fatal("followed substituted archive directory")
	}
}
