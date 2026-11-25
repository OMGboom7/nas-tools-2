//go:build linux || darwin

package organization

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiskDedupFastCollisionAndJournalRecovery(t *testing.T) {
	dir := t.TempDir()
	root, err := FreezeDiskRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	src := bytes.Repeat([]byte{17}, 2<<20)
	different := bytes.Clone(src)
	different[len(src)-1] = 18
	for name, content := range map[string][]byte{"A.mkv": different, "B.mkv": src, "C.MKV": src, "D.txt": src} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "A.mkv"), filepath.Join(dir, "L.mkv")); err != nil {
		t.Fatal(err)
	}
	cache := map[string]DiskCacheRow{}
	scan, err := ScanDiskDuplicates(t.Context(), root, []string{".mkv"}, 2<<20, true, true, cache)
	if err != nil || len(scan.Pairs) != 2 || scan.Files != 3 {
		t.Fatal(scan, err)
	}
	scan, err = ScanDiskDuplicates(t.Context(), root, []string{".mkv"}, 2<<20, true, false, cache)
	if err != nil || len(scan.Pairs) != 1 || scan.Pairs[0].Keep.Path != "B.mkv" {
		t.Fatal(scan, err)
	}
	pair := scan.Pairs[0]
	var hold string
	stop := errors.New("simulated process exit")
	// Stop after exchange and full read-back, immediately before unlink.
	calls := 0
	validate := func() error {
		calls++
		if calls == 3 {
			return stop
		}
		return nil
	}
	done, err := ReplaceDiskDuplicate(t.Context(), pair, "fixture", 1, "", func(s string) error { hold = s; return nil }, validate)
	if done || !errors.Is(err, stop) || hold == "" {
		t.Fatal(done, err, hold, calls)
	}
	stage := DiskStagePath(pair, "fixture", 1)
	before, err := os.Stat(filepath.Join(stage, "payload"))
	if err != nil || Identity(before) != pair.Duplicate.Node.Identity {
		t.Fatal(before, err)
	}
	done, err = ReplaceDiskDuplicate(t.Context(), pair, "fixture", 1, hold, func(string) error { return errors.New("duplicate stage") }, func() error { return nil })
	if !done || err != nil {
		t.Fatal(done, err)
	}
	if _, err = os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("stage not cleared", err)
	}
	for _, name := range []string{"B.mkv", "C.MKV"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || Identity(info) != pair.Keep.Node.Identity {
			t.Fatal(name, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "A.mkv"))
	if err != nil || !bytes.Equal(b, different) {
		t.Fatal("sample collision changed file", err)
	}
	scan, err = ScanDiskDuplicates(context.Background(), root, []string{".mkv"}, 0, true, false, cache)
	if err != nil || len(scan.Pairs) != 0 || scan.ExistingLinks != 1 {
		t.Fatal(scan, err)
	}
}

func TestDiskDedupChangedDestinationKeepsContent(t *testing.T) {
	dir := t.TempDir()
	root, err := FreezeDiskRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.mkv", "b.mkv"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	scan, err := ScanDiskDuplicates(t.Context(), root, []string{".mkv"}, 0, false, false, map[string]DiskCacheRow{})
	if err != nil || len(scan.Pairs) != 1 {
		t.Fatal(scan, err)
	}
	if err := os.Rename(filepath.Join(dir, "b.mkv"), filepath.Join(dir, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.mkv"), []byte("changed!"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = ReplaceDiskDuplicate(t.Context(), scan.Pairs[0], "fixture", 1, "", func(string) error { return nil }, func() error { return nil })
	b, e := os.ReadFile(filepath.Join(dir, "b.mkv"))
	if err == nil || e != nil || string(b) != "changed!" {
		t.Fatal(string(b), err, e)
	}
}
