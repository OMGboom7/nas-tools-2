package organization

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// One media entry represents an entire reviewed disc, not each STREAM clip.
// The manifest lives only in the private definition; public entries carry a
// bounded summary. Every file (including menus and metadata) is preserved.
type DiscInfo struct {
	Files          int    `json:"files"`
	Directories    int    `json:"directories"`
	Expanded       bool   `json:"expanded"`
	Fingerprint    string `json:"fingerprint"`
	LatestModified int64  `json:"latestModified"`
}
type DiscNode struct {
	Path      string
	Directory bool
	Node      LibraryNode
}
type DiscManifest struct {
	Root  LibraryNode
	Nodes []DiscNode
}
type DiscFileProof struct{ Path, Identity, Digest, LinkTarget string }
type DiscProof struct {
	Directories        map[string]string
	WitnessDirectories map[string]string
	Files              []DiscFileProof
}

func (m DiscManifest) Summary() (*DiscInfo, int64) {
	s := &DiscInfo{}
	var bytes int64
	versions := []any{m.Root.Identity, m.Root.Modified}
	for _, n := range m.Nodes {
		versions = append(versions, []any{n.Path, n.Directory, n.Node.Identity, n.Node.Size, n.Node.Modified})
		if !n.Directory && n.Node.Modified > s.LatestModified {
			s.LatestModified = n.Node.Modified
		}
		if n.Directory {
			s.Directories++
		} else {
			s.Files++
			bytes += n.Node.Size
		}
	}
	// Like ordinary scan snapshots, this excludes link counts and ctime: our
	// own hard-link publication/cleanup must not manufacture a new arrival.
	s.Fingerprint = Digest(versions)
	return s, bytes
}

var discVolumeName = regexp.MustCompile(`(?i)^(disc|disk|bd|cd)[ ._-]*[0-9]+$`)

func directDisc(root *os.Root, p string) bool {
	index, err := noLinks(root, filepath.Join(p, "BDMV", "index.bdmv"))
	if err != nil || !index.Mode().IsRegular() {
		return false
	}
	stream, err := noLinks(root, filepath.Join(p, "BDMV", "STREAM"))
	return err == nil && stream.IsDir()
}
func discCandidate(root *os.Root, p string) bool {
	if directDisc(root, p) {
		return true
	}
	f, err := root.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	names, err := f.ReadDir(257)
	if err != nil || len(names) > 256 {
		return false
	}
	found := false
	for _, n := range names {
		if !n.IsDir() || n.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if directDisc(root, filepath.Join(p, n.Name())) {
			if !discVolumeName.MatchString(n.Name()) {
				return false
			}
			found = true
		}
	}
	return found
}
func discAncestor(root *os.Root, p string) string {
	info, err := noLinks(root, p)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		p = filepath.Dir(p)
	}
	for {
		if discCandidate(root, p) {
			parent := filepath.Dir(p)
			if p != "." && discVolumeName.MatchString(filepath.Base(p)) && discCandidate(root, parent) {
				return parent
			}
			return p
		}
		if p == "." {
			return ""
		}
		p = filepath.Dir(p)
	}
}
func discFile(ctx context.Context, base, path string, expanded bool) (File, error) {
	m, err := readDiscManifest(ctx, base, path)
	if err != nil {
		return File{}, err
	}
	summary, size := m.Summary()
	summary.Expanded = expanded
	return File{Path: path, Kind: "media", Size: size, Modified: m.Root.Modified, Identity: m.Root.Identity, Disc: &m, DiscInfo: summary}, nil
}
func IsDisc(item Entry) bool { return item.Disc != nil }
func DiscContaining(ctx context.Context, base, path string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	root, err := OpenRoot(base)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if _, err := noLinks(root, path); err != nil {
		return "", err
	}
	disc := discAncestor(root, path)
	if disc == "" {
		return "", ErrMode
	}
	return disc, nil
}
func discManifestDefinition(d Definition, e Entry) (DiscManifest, error) {
	m, ok := d.Discs[e.Source]
	if !ok || e.Disc == nil || len(m.Nodes) == 0 || len(m.Nodes) > 20000 || m.Root.Identity != e.Identity || strconv.FormatInt(m.Root.Modified, 10) != e.Modified {
		return m, ErrState
	}
	seen := map[string]bool{}
	var total int64
	files, dirs := 0, 0
	for _, n := range m.Nodes {
		if !validRelative(n.Path) || n.Path == "." || seen[n.Path] || n.Node.Identity == "" || n.Node.Size < 0 {
			return m, ErrState
		}
		seen[n.Path] = true
		if n.Directory {
			dirs++
		} else {
			files++
			if n.Node.Size > 1<<50 || total > (1<<50)-n.Node.Size {
				return m, ErrLimit
			}
			total += n.Node.Size
		}
	}
	if e.Size != total || files != e.Disc.Files || dirs != e.Disc.Directories {
		return m, ErrState
	}
	summary, _ := m.Summary()
	if e.Disc.Fingerprint != summary.Fingerprint || e.Disc.LatestModified != summary.LatestModified {
		return m, ErrState
	}
	return m, nil
}
func DiscTargetOverlap(a, b string) bool { return discPathOverlap(a, b) }
func sourceUnder(path, scope string) bool {
	return scope == "." || path == scope || strings.HasPrefix(path, scope+string(filepath.Separator))
}
