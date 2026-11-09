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
