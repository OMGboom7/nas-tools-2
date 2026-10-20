package organization

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// A reviewed existing disc is a readonly work item, not an invented completed
// intake. Critical directories and every non-metadata leaf remain frozen.
// Each symlink leaf retains its independently authorized regular input binding.
type LibraryDisc struct {
	Directories []DiscNode
	Files       []LibraryMedia
	Expanded    bool
	visited     int // Runtime-only scan budget, never part of the frozen document.
}

const libraryDiscNodes = 20000
const libraryDiscDocument = 16 << 20
const LibraryWorklistBytes = 64 << 20

func libraryMediaDocument(m LibraryMedia) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, ErrState
	}
	limit := 64 << 10
	if m.Disc != nil {
		limit = libraryDiscDocument
	}
	if len(raw) > limit {
		return nil, ErrLimit
	}
	return raw, nil
}

func LibraryInventoryBytes(files []LibraryMedia) (int, error) {
	if len(files) > 50_000 {
		return 0, ErrLimit
	}
	bytes := 0
	for _, m := range files {
		raw, err := libraryMediaDocument(m)
		if err != nil {
			return 0, err
		}
		bytes += len(raw)
		if bytes > LibraryWorklistBytes {
			return 0, ErrLimit
		}
	}
	return bytes, nil
}

func LibraryDiscSummary(m LibraryMedia) *DiscInfo {
	if m.Disc == nil {
		return nil
	}
	info := &DiscInfo{Files: len(m.Disc.Files), Directories: len(m.Disc.Directories) - 1, Expanded: m.Disc.Expanded, Fingerprint: Digest(m.Disc)}
	for _, f := range m.Disc.Files {
		modified := f.Node.Modified
		if f.Link != "" {
			modified = f.Input.Modified
		}
		if modified > info.LatestModified {
			info.LatestModified = modified
		}
	}
	return info
}
func LibraryDiscStem(d LibraryDefinition, m LibraryMedia) string {
	if m.Path == "." {
		return filepath.Base(d.Root.Path)
	}
	return filepath.Base(m.Path)
}
func libraryDiscDefinition(d LibraryDefinition, m LibraryMedia) error {
	if err := libraryDiscInventory(m); err != nil {
		return err
	}
	if m.Disc.Expanded && !d.AllowDiscExpansion {
		return ErrState
	}
	if m.Disc.Expanded {
		if !sourceUnder(d.Selection, m.Path) || d.Selection == m.Path {
			return ErrState
		}
	} else if !sourceUnder(m.Path, d.Selection) {
		return ErrState
	}
	return nil
}

func libraryDiscInventory(m LibraryMedia) error {
	if m.Disc == nil || m.Blocked != "" || !validRelative(m.Path) || m.Node.Identity == "" || m.Link != "" || len(m.Disc.Directories) == 0 || len(m.Disc.Files) == 0 || len(m.Disc.Files)+len(m.Disc.Directories) > libraryDiscNodes {
		return ErrState
	}
	seen := map[string]bool{}
	root := false
	var size int64
	for _, dir := range m.Disc.Directories {
		key := filepath.Join(m.Path, dir.Path)
		if !dir.Directory || !validRelative(dir.Path) || dir.Node.Identity == "" || seen[key] {
			return ErrState
		}
		seen[key] = true
		if dir.Path == "." {
			if root || dir.Node != m.Node {
				return ErrState
			}
			root = true
		}
	}
	for _, f := range m.Disc.Files {
		if f.Disc != nil || f.Blocked != "" || !validRelative(f.Path) || f.Path == m.Path || !sourceUnder(f.Path, m.Path) || seen[f.Path] || f.Node.Identity == "" || f.Node.Size < 0 {
			return ErrState
		}
		seen[f.Path] = true
		bytes := f.Node.Size
		if f.Link != "" {
			if f.InputRoot == "" || f.InputPath == "" || f.Input.Identity == "" {
				return ErrState
			}
			bytes = f.Input.Size
		}
		if bytes < 0 || bytes > 1<<50 || size > (1<<50)-bytes {
			return ErrLimit
		}
		size += bytes
	}
	if !root {
		return ErrState
	}
	return nil
}
func libraryDiscExcluded(d LibraryDefinition, path string) bool {
	for _, x := range d.Excludes {
		if sourceUnder(path, x) || sourceUnder(x, path) {
			return true
		}
	}
	return false
}
func libraryDiscMutable(d LibraryDefinition, path, name string) bool {
	if strings.HasPrefix(name, ".nastool-library-") || strings.HasPrefix(name, ".nastool-sidecar-") {
		return true
	}
	return discMetadataName(LibraryDiscStem(d, LibraryMedia{Path: path}), name)
}
