package diskusage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadLibraryVolume(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "电影 #1")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(child, "movie.mkv")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := Read(dir)
	if err != nil || root.Volume == "" || root.TotalBytes <= 0 || root.AvailableBytes > root.TotalBytes {
		t.Fatal(root, err)
	}
	for _, path := range []string{child, file} {
		space, err := Read(path)
		if err != nil || space.Volume != root.Volume || space.TotalBytes != root.TotalBytes {
			t.Fatalf("%s: %#v, %v", path, space, err)
		}
	}
	if _, err := Read(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing library:", err)
	}
}
