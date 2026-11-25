package torrenttransfer

import (
	"errors"
	"path"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

// ErrTaskAbsent is reserved for a successful exact lookup with zero rows.
// Transport failures and malformed responses never prove absence.
var ErrTaskAbsent = errors.New("torrent transfer task absent")
var ErrProtocol = errors.New("torrent transfer protocol or metadata unsupported")

type AddReceipt struct {
	WireID    string `json:"wireId"`
	Duplicate bool   `json:"duplicate"`
	// qB's Ok. only acknowledges the request; it does not prove ownership of
	// a new task. A controller must bind a fresh post-add snapshot separately.
	CreatedKnown bool `json:"createdKnown"`
}

// Task is an exact read of a downloader task, including its incarnation and
// file-level evidence. It is not permission to start, remove or replay it.
type Task struct {
	Kind          string              `json:"kind"`
	WireID        string              `json:"wireId"`
	V1Hash        string              `json:"v1Hash"`
	V2Hash        string              `json:"v2Hash"`
	AddedAt       int64               `json:"addedAt"`
	Name          string              `json:"name"`
	Directory     string              `json:"directory"`
	MetainfoFile  string              `json:"metainfoFile,omitempty"`
	State         string              `json:"state"`
	RawQbTags     string              `json:"rawQbTags"`
	Labels        []string            `json:"labels"`
	Paused        bool                `json:"paused"`
	Checking      bool                `json:"checking"`
	Complete      bool                `json:"complete"`
	HasTracker    bool                `json:"hasTracker"`
	SourceSeeding bool                `json:"sourceSeeding"`
	Error         bool                `json:"error"`
	AutoManaged   bool                `json:"autoManaged"`
	Files         []torrentfiles.File `json:"files"`
	Done          []int               `json:"done"`
}

func Labels(raw []string) ([]string, error) {
	if raw == nil || len(raw) > 128 {
		return nil, ErrState
	}
	out := []string{}
	for _, label := range raw {
		if label == "" || len(label) > 256 || !utf8.ValidString(label) || strings.ContainsFunc(label, unicode.IsControl) {
			return nil, ErrState
		}
		if slices.Contains(out, label) {
			return nil, ErrState
		}
		out = append(out, label)
	}
	sort.Strings(out)
	return out, nil
}

func (s Task) Valid() bool {
	if s.Kind != "qbittorrent" && s.Kind != "transmission" || !Hex(s.WireID, 40) || s.AddedAt <= 0 || s.V1Hash == "" && s.V2Hash == "" || s.V1Hash != "" && !Hex(s.V1Hash, 40) || s.V2Hash != "" && !Hex(s.V2Hash, 64) {
		return false
	}
	if s.WireID != s.V1Hash && (s.V2Hash == "" || s.WireID != s.V2Hash[:40]) {
		return false
	}
	paused, checking, seeding, completeState := false, false, false, false
	if s.Kind == "qbittorrent" {
		switch s.State {
		case "pausedUP", "stoppedUP":
			paused, completeState = true, true
		case "pausedDL", "stoppedDL":
			paused = true
		case "uploading", "stalledUP", "queuedUP", "forcedUP":
			seeding, completeState = true, true
		case "checkingUP", "checkingDL", "checkingResumeData":
			checking = true
		case "error", "missingFiles":
			if !s.Error {
				return false
			}
		case "downloading", "stalledDL", "queuedDL", "forcedDL", "metaDL", "forcedMetaDL", "moving":
		default:
			return false
		}
	} else {
		switch s.State {
		case "stopped":
			paused, completeState = true, true
		case "seed-wait", "seeding":
			seeding, completeState = true, true
		case "check-wait", "checking":
			checking = true
		case "download-wait", "downloading":
		default:
			return false
		}
	}
	if s.Paused != paused || s.Checking != checking || s.SourceSeeding != seeding || s.Complete && (!completeState || s.Error) {
		return false
	}
	if s.MetainfoFile != "" && (len(s.MetainfoFile) > 512 || !utf8.ValidString(s.MetainfoFile) || strings.ContainsFunc(s.MetainfoFile, unicode.IsControl) || path.Base(s.MetainfoFile) != s.MetainfoFile || strings.Contains(s.MetainfoFile, "\\") || !strings.HasSuffix(s.MetainfoFile, ".torrent")) {
		return false
	}
	if _, err := NormalizeRemote(s.Directory); err != nil {
		return false
	}
	if _, err := Labels(s.Labels); err != nil || len(s.RawQbTags) > 32<<10 || !utf8.ValidString(s.RawQbTags) || strings.ContainsFunc(s.RawQbTags, unicode.IsControl) {
		return false
	}
	files, err := torrentfiles.Normalize(s.Files)
	if err != nil {
		return false
	}
	ids := map[int]bool{}
	for _, f := range files {
		ids[f.Index] = true
	}
	seen := map[int]bool{}
	for _, id := range s.Done {
		if seen[id] || !ids[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (s Task) Matches(m torrentmeta.TransferMetadata) bool {
	return s.Valid() && (m.V1Hash != "" && s.V1Hash == m.V1Hash || m.V1Hash == "" && m.V2Hash != "" && s.V2Hash == m.V2Hash) && (s.V2Hash == "" || s.V2Hash == m.V2Hash) && torrentfiles.Match(m.Files, s.Files)
}

// SameInstance deliberately excludes progress and running state. File
// selection, original raw qB labels, paths and creation time stay bound.
func (s Task) SameInstance(other Task) bool {
	if !s.Valid() || !other.Valid() {
		return false
	}
	a, _ := NormalizeRemote(s.Directory)
	b, _ := NormalizeRemote(other.Directory)
	ls, _ := Labels(s.Labels)
	lo, _ := Labels(other.Labels)
	return s.Kind == other.Kind && s.WireID == other.WireID && s.V1Hash == other.V1Hash && s.V2Hash == other.V2Hash && s.AddedAt == other.AddedAt && a == b && s.MetainfoFile == other.MetainfoFile && s.RawQbTags == other.RawQbTags && slices.Equal(ls, lo) && s.AutoManaged == other.AutoManaged && torrentfiles.Wanted(s.Files, other.Files)
}

func (s Task) FullComplete(m torrentmeta.TransferMetadata) bool {
	if !s.Matches(m) || !s.Complete || s.Checking || s.Error {
		return false
	}
	padding := map[string]bool{}
	done := map[int]bool{}
	for _, id := range s.Done {
		done[id] = true
	}
	for _, f := range m.Files {
		padding[f.Path] = f.Padding
	}
	for _, f := range s.Files {
		if !padding[f.Path] && (!f.Selected || !done[f.Index]) {
			return false
		}
	}
	return true
}
