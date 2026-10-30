package organization

import (
	"path/filepath"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

// Library media are adopted readonly from a reviewed existing library. They
// are not invented completed intake jobs and never write transfer history.
type LibraryRoot struct{ ID, Path, Identity, Kind string }
type LibraryNode struct {
	Identity, Parent string
	Size, Modified   int64
	Changed          [2]int64
	Links            uint64
}
type LibraryMedia struct {
	Remote    *LibraryRemoteMedia `json:",omitempty"`
	Disc      *LibraryDisc        `json:",omitempty"`
	Path      string
	Blocked   string
	Node      LibraryNode
	Link      string
	InputRoot string
	InputPath string
	Input     LibraryNode
}
type LibraryDefinition struct {
	OpenSubtitle                                        *OpenSubtitleLibrary `json:"openSubtitle,omitempty"`
	Remote                                              *remotestore.Binding `json:",omitempty"`
	RemoteApproval                                      *RemoteApproval      `json:",omitempty"`
	AllowDiscExpansion                                  bool                 `json:",omitempty"`
	Root                                                LibraryRoot
	Allowed                                             []LibraryRoot
	Selection                                           string
	Excludes                                            []string
	Mode, ConfigDigest, Settings, SettingsDigest, Actor string
	RequestID                                           string
	Count                                               int
	Schedule                                            *LibraryScheduleOrigin
}
type LibraryScheduleOrigin struct {
	Epoch, Batch string
	Event        bool
}
type LibraryScan struct {
	Files         []LibraryMedia
	Skipped       int
	SnapshotBytes int
}
type LibraryOutputProof struct {
	Remote                                                               *LibraryRemoteOutput `json:",omitempty"`
	OldLink                                                              string               `json:",omitempty"`
	Target, Stage, StageIdentity, ParentIdentity, NewIdentity, NewDigest string
	NewSize                                                              int64
	Old                                                                  LibraryNode
	OldDigest                                                            string
}

// A remote review freezes the complete object inventory, not local inode
// identities. Before metadata lookup/publication the worker adopts full SHA-256
// content into the durable running item under a separately approved namespace.
type LibraryRemoteMedia struct {
	Node   remotestore.Node
	Digest string
}

type LibraryRemoteOutput struct {
	Driver, Operation, Manifest string
	Old                         *remotestore.Node `json:",omitempty"`
}

func libraryRoot(roots []LibraryRoot, id string) (LibraryRoot, bool) {
	for _, r := range roots {
		if r.ID == id {
			return r, true
		}
	}
	return LibraryRoot{}, false
}

// InputRoot is a frozen root ID, not a filesystem path. Resolve it only against
// the original job document, never current config or an offline filesystem.
// Disc links retain independent input bindings, all of which are protected.
func libraryProtectedLocations(d LibraryDefinition, m LibraryMedia) ([]string, error) {
	locations := []string{}
	if m.Path != "" {
		if !filepath.IsAbs(d.Root.Path) || !validRelative(m.Path) {
			return nil, ErrPath
		}
		locations = append(locations, filepath.Join(d.Root.Path, m.Path))
	}
	inputs := []LibraryMedia{m}
	if m.Disc != nil {
		if len(m.Disc.Files) > libraryDiscNodes {
			return nil, ErrLimit
		}
		inputs = m.Disc.Files
	}
	for _, input := range inputs {
		if input.InputRoot == "" && input.InputPath == "" {
			continue
		}
		if !validRelative(input.InputPath) || input.InputPath == "." {
			return nil, ErrPath
		}
		root, found := d.Root, input.InputRoot == d.Root.ID
		if !found {
			root, found = libraryRoot(d.Allowed, input.InputRoot)
		}
		if !found || !filepath.IsAbs(root.Path) {
			return nil, ErrPath
		}
		locations = append(locations, filepath.Join(root.Path, input.InputPath))
	}
	return locations, nil
}

func LibraryMode(mode string) bool {
	return mode == "no_force" || mode == "force_nfo" || mode == "force_all"
}
func LibraryOverwrite(mode, name string) bool {
	return mode == "force_all" || mode == "force_nfo" && strings.HasSuffix(strings.ToLower(name), ".nfo")
}
