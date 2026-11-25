//go:build linux || darwin

package mediaarchive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateFailureRevocationAndSameSecondCollision(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nested", "archive")
	const name = "归档_20260301000000.md"
	allow := func() error { return nil }
	collect := func(w io.Writer) (int, error) { _, err := io.WriteString(w, "- Movies\n"); return 1, err }
	run := func(read func(io.Writer) (int, error), check func() error) (Result, error) {
		return Generate(t.Context(), "config.yaml", dir, false, name, 1, read, check)
	}
	if _, err := run(func(w io.Writer) (int, error) { _, _ = io.WriteString(w, "partial"); return 0, io.ErrUnexpectedEOF }, allow); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	checks := 0
	if _, err := run(collect, func() error {
		checks++
		if checks == 2 {
			return ErrChanged
		}
		return nil
	}); !errors.Is(err, ErrChanged) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if _, err := run(collect, allow); err != nil {
		t.Fatal(err)
	}
	if _, err := run(func(w io.Writer) (int, error) { _, err := io.WriteString(w, "replacement"); return 1, err }, allow); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(b) != "- Movies\n" {
		t.Fatal(string(b), err)
	}
}
