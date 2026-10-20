//go:build linux || darwin

package organization

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCompletedMediaReaderPinsReadonlyFileAndRejectsLateReplacement(t *testing.T) {
	for _, changed := range []bool{false, true} {
		job, item := sidecarFixture(t)
		called := 0
		b, err := ReadCompletedMedia(t.Context(), job, item, func(f *os.File) ([]byte, error) {
			called++
			data, err := io.ReadAll(f)
			if err != nil {
				return nil, err
			}
			if _, err := f.Write([]byte("must not write")); err == nil {
				t.Fatal("consumer could write input")
			}
			if changed {
				path := filepath.Join(job.Definition.TargetRoot, item.Target)
				info, _ := os.Stat(path)
				if err := os.WriteFile(path, make([]byte, info.Size()), 0600); err != nil {
					t.Fatal(err)
				}
				os.Chtimes(path, info.ModTime(), info.ModTime())
			}
			return data, nil
		})
		if called != 1 || !changed && (err != nil || string(b) != "original") || changed && err == nil {
			t.Fatal(changed, string(b), err, called)
		}
	}
}
func TestExistingSidecarIsReadonlyAndDoesNotFollowOutsideLinks(t *testing.T) {
	job, item := sidecarFixture(t)
	if exists, err := ExistingSidecar(t.Context(), job, item, "media", "Movie.nfo"); err != nil || exists {
		t.Fatal(exists, err)
	}
	path := filepath.Join(job.Definition.TargetRoot, filepath.Dir(item.Target), "Movie.nfo")
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
		t.Fatal(err)
	}
	if exists, err := ExistingSidecar(t.Context(), job, item, "media", "Movie.nfo"); err != nil || !exists {
		t.Fatal("existing foreign link not preserved", exists, err)
	}
	if _, err := ExistingSidecar(t.Context(), job, item, "media", "../outside.nfo"); err == nil {
		t.Fatal("output name escaped")
	}
}
