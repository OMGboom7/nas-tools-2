package organization

import (
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

func LibraryMode(mode string) bool {
	return mode == "no_force" || mode == "force_nfo" || mode == "force_all"
}
func LibraryOverwrite(mode, name string) bool {
	return mode == "force_all" || mode == "force_nfo" && strings.HasSuffix(strings.ToLower(name), ".nfo")
}
