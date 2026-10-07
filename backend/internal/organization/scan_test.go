package organization

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fixtureFile(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestScanIncludesMediaCompanionsAndSkipsUnsafeEntries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, name := range []string{"Show/episode.mkv", "Show/episode.en.srt", "Show/episode.mka", "Show/downloading.mkv.!qb", "Show/downloading.mp4.part", ".hidden/movie.mp4", "note.txt"} {
		fixtureFile(t, root, name)
	}
	fixtureFile(t, outside, "private.mkv")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	result, err := Scan(t.Context(), root, ".")
	if err != nil || len(result.Files) != 3 || result.Skipped != 5 {
		t.Fatal(result, err)
	}
	if result.Files[0].Kind != "subtitle" || result.Files[1].Kind != "audio" || result.Files[2].Kind != "media" {
		t.Fatal(result)
	}
	for _, file := range result.Files {
		if file.Size != 8 || file.Modified <= 0 {
			t.Fatal(file)
		}
	}
	contents, err := os.ReadFile(filepath.Join(root, "Show/episode.mkv"))
	if err != nil || string(contents) != "original" {
		t.Fatal("scan modified media", err)
	}
}

func TestScanRejectsSelectorsAndCancellation(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "movie.mkv")
	if err := os.Symlink(".", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../", "../movie.mkv", "/movie.mkv", "alias/movie.mkv", "a/../movie.mkv", "movie.mkv\x00", "a\\b"} {
		if _, err := Scan(t.Context(), root, name); !errors.Is(err, ErrPath) {
			t.Fatal(name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, root, "."); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if result, err := Scan(t.Context(), root, "movie.mkv"); err != nil || len(result.Files) != 1 {
		t.Fatal(result, err)
	}
}

func TestScanLimitsReturnNoPartialPlan(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1001; i++ {
		fixtureFile(t, root, fmt.Sprintf("%04d.mkv", i))
	}
	result, err := Scan(t.Context(), root, ".")
	if !errors.Is(err, ErrLimit) || len(result.Files) != 0 {
		t.Fatal(result, err)
	}
	deep := t.TempDir()
	name := ""
	for i := 0; i < 34; i++ {
		name = filepath.Join(name, "sub")
	}
	fixtureFile(t, deep, filepath.Join(name, "movie.mkv"))
	if _, err := Scan(t.Context(), deep, "."); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

func TestTargetStatusChecksAncestorsAndNeverCreatesFiles(t *testing.T) {
	root := t.TempDir()
	fixtureFile(t, root, "Movie/movie.mkv")
	fixtureFile(t, root, "not-directory")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"New/movie.mkv": "available", "Movie/movie.mkv": "conflict", "not-directory/movie.mkv": "blocked", "linked/movie.mkv": "blocked"} {
		got, err := TargetStatus(t.Context(), root, name)
		if err != nil || got != want {
			t.Fatal(name, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "New")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created directory", err)
	}
	for _, name := range []string{".", "../outside.mkv", "/outside.mkv", "a//b.mkv"} {
		if _, err := TargetStatus(t.Context(), root, name); !errors.Is(err, ErrPath) {
			t.Fatal(name, err)
		}
	}
}
