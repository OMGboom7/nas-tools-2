package organization

import "strings"

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
	Path      string
	Blocked   string
	Node      LibraryNode
	Link      string
	InputRoot string
	InputPath string
	Input     LibraryNode
}
type LibraryDefinition struct {
	Root                                                LibraryRoot
	Allowed                                             []LibraryRoot
	Selection                                           string
	Excludes                                            []string
	Mode, ConfigDigest, Settings, SettingsDigest, Actor string
	RequestID                                           string
	Count                                               int
}
type LibraryScan struct {
	Files   []LibraryMedia
	Skipped int
}
type LibraryOutputProof struct {
	Target, Stage, StageIdentity, ParentIdentity, NewIdentity, NewDigest string
	NewSize                                                              int64
	Old                                                                  LibraryNode
	OldDigest                                                            string
}

func LibraryMode(mode string) bool {
	return mode == "no_force" || mode == "force_nfo" || mode == "force_all"
}
func LibraryOverwrite(mode, name string) bool {
	return mode == "force_all" || mode == "force_nfo" && strings.HasSuffix(strings.ToLower(name), ".nfo")
}
