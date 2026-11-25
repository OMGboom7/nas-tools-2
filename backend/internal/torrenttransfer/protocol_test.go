package torrenttransfer

import (
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

func TestTransferTaskBindsFullIdentityAndEveryPayloadFile(t *testing.T) {
	m := torrentmeta.TransferMetadata{Metadata: torrentmeta.Metadata{InfoHash: strings.Repeat("a", 40)}, V1Hash: strings.Repeat("a", 40), V2Hash: strings.Repeat("b", 64), Files: []torrentfiles.File{{Index: 0, Path: "Movie/file.mkv", Size: 8}}}
	s := Task{Kind: "qbittorrent", WireID: strings.Repeat("b", 40), V1Hash: m.V1Hash, V2Hash: m.V2Hash, AddedAt: 123, Directory: "/library", State: "stoppedUP", Paused: true, Complete: true, Labels: []string{}, Files: []torrentfiles.File{{Index: 7, Path: "Movie/file.mkv", Size: 8, Selected: true}}, Done: []int{7}}
	if !s.Matches(m) || !s.FullComplete(m) || !s.SameInstance(s) {
		t.Fatal("valid complete hybrid task rejected")
	}
	for _, change := range []func(*Task){func(s *Task) { s.AddedAt++ }, func(s *Task) { s.Directory = "/other" }, func(s *Task) { s.V2Hash = strings.Repeat("c", 64) }, func(s *Task) { s.RawQbTags = "changed" }} {
		other := s
		change(&other)
		if s.SameInstance(other) {
			t.Fatal("task identity change accepted")
		}
	}
	s.Done = nil
	if s.FullComplete(m) {
		t.Fatal("aggregate progress replaced file proof")
	}
	s.Done = []int{7}
	s.Checking = true
	if s.FullComplete(m) {
		t.Fatal("checking task considered ready to start")
	}
}

func TestTransferTaskDoesNotInferFullV2IdentityFromPrefix(t *testing.T) {
	m := torrentmeta.TransferMetadata{Metadata: torrentmeta.Metadata{InfoHash: strings.Repeat("b", 64)}, V2Hash: strings.Repeat("b", 64), Files: []torrentfiles.File{{Index: 0, Path: "movie.mkv", Size: 8}}}
	s := Task{Kind: "qbittorrent", WireID: strings.Repeat("b", 40), AddedAt: 123, Directory: "/library", State: "stoppedUP", Paused: true, Labels: []string{}, Files: m.Files}
	if s.Matches(m) {
		t.Fatal("truncated task ID treated as full identity")
	}
	s.V2Hash = m.V2Hash
	if !s.Matches(m) {
		t.Fatal("full property hash not recognized")
	}
}

func TestTransferCompletionWithVisibleAndHiddenPadding(t *testing.T) {
	payload := torrentfiles.File{Index: 7, Path: "bundle/.pad/movie", Size: 8, Selected: true}
	pad := torrentfiles.File{Index: 9, Path: "bundle/.pad/2", Size: 2, Padding: true}
	m := torrentmeta.TransferMetadata{V1Hash: strings.Repeat("a", 40), Files: []torrentfiles.File{payload, pad}}
	live := pad
	live.Padding, live.Selected = false, true
	s := Task{Kind: "transmission", WireID: m.V1Hash, V1Hash: m.V1Hash, AddedAt: 123, Directory: "/library", State: "stopped", Paused: true, Complete: true, Labels: []string{}, Files: []torrentfiles.File{live, payload}, Done: []int{7, 9}}
	if !s.FullComplete(m) {
		t.Fatal("complete client-visible padding rejected")
	}
	s.Done = []int{7}
	if s.FullComplete(m) {
		t.Fatal("selected incomplete padding accepted")
	}
	s.Files[0].Selected = false
	if !s.FullComplete(m) {
		t.Fatal("unwanted padding blocked completion")
	}
	s.Files = []torrentfiles.File{payload}
	if !s.FullComplete(m) {
		t.Fatal("hidden padding blocked completion")
	}
	s.Done = nil
	if s.FullComplete(m) {
		t.Fatal("missing payload completion accepted")
	}
	s.Done = []int{7}
	s.Files[0].Selected = false
	if s.FullComplete(m) {
		t.Fatal("unselected payload accepted")
	}
}
