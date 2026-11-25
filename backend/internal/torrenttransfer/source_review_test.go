package torrenttransfer

import (
	"context"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

type sourceFixture struct {
	task Task
}

func (f sourceFixture) Address() string { return "http://source.fixture" }
func (f sourceFixture) CompletedIDs(context.Context) ([]string, error) {
	return []string{f.task.WireID}, nil
}
func (f sourceFixture) TransferTask(context.Context, string) (Task, error) { return f.task, nil }

func TestScanSourcesBindsTaskAndMappedDirectory(t *testing.T) {
	path, id, _ := transferFileFixture(t)
	root, err := OpenMetainfoRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	settings := Settings{Enable: true, From: []string{"1"}, To: []string{"2"}, TorrentPath: path, FromPath: "/downloads", ToPath: "/media"}
	f := sourceFixture{task: Task{Kind: "transmission", WireID: id, V1Hash: id, AddedAt: 1, State: "seeding", SourceSeeding: true, Complete: true, Directory: "/downloads/movies", Labels: []string{}, Files: []torrentfiles.File{{Index: 0, Path: "movie.mkv", Size: 8, Selected: true}}, Done: []int{0}}}
	count := 0
	for review, err := range ScanSources(t.Context(), settings, f, root) {
		if err != nil || review.Directory != "/media/movies" {
			t.Fatal(review.Directory, err)
		}
		if err := review.Revalidate(t.Context(), settings, f, root); err != nil {
			t.Fatal(err)
		}
		f.task.AddedAt++
		if err := review.Revalidate(t.Context(), settings, f, root); err == nil {
			t.Fatal("replacement task accepted")
		}
		count++
	}
	if count != 1 {
		t.Fatal(count)
	}
	f.task.Paused, f.task.SourceSeeding, f.task.State = true, false, "stopped"
	for _, err := range ScanSources(t.Context(), settings, f, root) {
		t.Fatal("stopped Transmission task admitted", err)
	}
	if _, err := MapDirectory("//host/share/movies", "/", "/media"); err == nil {
		t.Fatal("UNC path admitted under POSIX root")
	}
}
