package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func LibraryRemoteMode(driver string) string {
	switch driver {
	case "rclone":
		return "rclonecopy"
	case "minio":
		return "miniocopy"
	}
	return ""
}
func libraryRemoteDefinition(d LibraryDefinition) bool {
	return d.Remote != nil && LibraryRemoteMode(d.Remote.Driver) != "" && d.Root.Identity == RemoteTargetIdentityFor(LibraryRemoteMode(d.Remote.Driver), d.Root.Path) && d.Remote.Protocol == 1 && d.Root.ID != "" && d.Root.Path != "/" && path.IsAbs(d.Root.Path) && path.Clean(d.Root.Path) == d.Root.Path && !strings.ContainsAny(d.Root.Path, "\\\x00\r\n") && len(d.Allowed) == 0
}

type libraryRemoteContextKey struct{}
type libraryRemoteScope struct {
	client  *remotestore.Client
	binding string
}

func LibraryRemoteContext(ctx context.Context, c *remotestore.Client, d LibraryDefinition) context.Context {
	return context.WithValue(ctx, libraryRemoteContextKey{}, libraryRemoteScope{c, Digest(d.Remote)})
}
func libraryRemoteClient(ctx context.Context, d LibraryDefinition) (*remotestore.Client, error) {
	s, ok := ctx.Value(libraryRemoteContextKey{}).(libraryRemoteScope)
	if !ok || s.client == nil || !libraryRemoteDefinition(d) || s.binding != Digest(d.Remote) || s.client.Binding() != *d.Remote {
		return nil, ErrState
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.client, s.client.Current()
}
func LibraryRemoteApproved(d LibraryDefinition) bool {
	return libraryRemoteDefinition(d) && d.RemoteApproval != nil && d.RemoteApproval.Valid() && d.RemoteApproval.Actor == d.Actor
}

func libraryRemoteNode(d LibraryDefinition, relative string, n remotestore.Node) LibraryNode {
	modified := int64(0)
	if t, err := time.Parse(time.RFC3339Nano, n.ModTime); err == nil {
		modified = t.UnixNano()
	}
	return LibraryNode{Identity: Digest([]any{"remote-library-file", path.Join(d.Root.Path, relative), n}), Parent: Digest([]string{"remote-library-parent", path.Join(d.Root.Path, path.Dir(relative))}), Size: n.Size, Modified: modified}
}
func libraryRemoteDirectory(d LibraryDefinition, relative string) LibraryNode {
	return LibraryNode{Identity: Digest([]string{"remote-library-directory", path.Join(d.Root.Path, relative)}), Parent: Digest([]string{"remote-library-parent", path.Join(d.Root.Path, path.Dir(relative))})}
}
func libraryRemoteFile(d LibraryDefinition, relative string, n remotestore.Node) LibraryMedia {
	n.Path, n.Name = path.Base(relative), path.Base(relative)
	node := libraryRemoteNode(d, relative, n)
	return LibraryMedia{Path: relative, Node: node, Input: node, InputRoot: d.Root.ID, InputPath: relative, Remote: &LibraryRemoteMedia{Node: n}}
}
func libraryRemoteHash(ctx context.Context, c *remotestore.Client, location string, size int64) (string, error) {
	h := sha256.New()
	counter := &hashCounter{h: h}
	if err := c.Read(ctx, location, size, counter); err != nil {
		return "", err
	}
	if counter.count != size {
		return "", ErrState
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func libraryRemoteInventory(ctx context.Context, c *remotestore.Client, d LibraryDefinition, relative string) (map[string]remotestore.Node, error) {
	result := map[string]remotestore.Node{}
	if relative != "." {
		n, err := c.Entry(ctx, d.Root.Path, relative)
		if err != nil || n == nil {
			return nil, ErrPath
		}
		if !n.IsDir {
			result[relative] = *n
			return result, nil
		}
	}
	result[relative] = remotestore.Node{Path: ".", Name: path.Base(relative), IsDir: true}
	nodes, err := c.List(ctx, path.Join(d.Root.Path, relative), true)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		result[path.Join(relative, n.Path)] = n
	}
	return result, nil
}
func libraryRemoteDirect(nodes map[string]remotestore.Node, relative string) bool {
	index, ok := nodes[path.Join(relative, "BDMV/index.bdmv")]
	if !ok || index.IsDir {
		return false
	}
	stream, ok := nodes[path.Join(relative, "BDMV/STREAM")]
	return ok && stream.IsDir
}
func libraryRemoteCandidate(nodes map[string]remotestore.Node, relative string) bool {
	if libraryRemoteDirect(nodes, relative) {
		return true
	}
	found := false
	for p, n := range nodes {
		if !n.IsDir || p == relative || path.Dir(p) != relative {
			continue
		}
		if libraryRemoteDirect(nodes, p) {
			if !discVolumeName.MatchString(path.Base(p)) {
				return false
			}
			found = true
		}
	}
	return found
}
func libraryRemoteAncestor(ctx context.Context, c *remotestore.Client, d LibraryDefinition) (string, error) {
	relative := d.Selection
	if relative != "." {
		n, err := c.Entry(ctx, d.Root.Path, relative)
		if err != nil || n == nil {
			return "", ErrPath
		}
		if !n.IsDir {
			relative = path.Dir(relative)
		}
	}
	// Ordinary ancestors need only shallow probes. A possible multi-volume
	// disc is checked as a whole before any internal selection can expand.
	candidateAt := func(relative string) (bool, error) {
		nodes, err := c.List(ctx, path.Join(d.Root.Path, relative), false)
		if err != nil {
			return false, err
		}
		volume := false
		for _, n := range nodes {
			if !n.IsDir {
				continue
			}
			if n.Path == "BDMV" {
				children, e := c.List(ctx, path.Join(d.Root.Path, relative, "BDMV"), false)
				if e != nil {
					return false, e
				}
				index, stream := false, false
				for _, child := range children {
					index = index || child.Path == "index.bdmv" && !child.IsDir
					stream = stream || child.Path == "STREAM" && child.IsDir
				}
				if index && stream {
					return true, nil
				}
			}
			volume = volume || discVolumeName.MatchString(n.Path)
		}
		if !volume {
			return false, nil
		}
		inventory, err := libraryRemoteInventory(ctx, c, d, relative)
		if err != nil {
			return false, err
		}
		return libraryRemoteCandidate(inventory, relative), nil
	}
	for {
		candidate, err := candidateAt(relative)
		if err != nil {
			return "", err
		}
		if candidate {
			if relative != "." && discVolumeName.MatchString(path.Base(relative)) {
				parent, err := candidateAt(path.Dir(relative))
				if err != nil {
					return "", err
				}
				if parent {
					return path.Dir(relative), nil
				}
			}
			return relative, nil
		}
		if relative == "." {
			return "", nil
		}
		relative = path.Dir(relative)
	}
}
func libraryRemoteDisc(d LibraryDefinition, relative string, nodes map[string]remotestore.Node) (LibraryMedia, error) {
	root := libraryRemoteDirectory(d, relative)
	m := LibraryMedia{Path: relative, Node: root, Remote: &LibraryRemoteMedia{Node: remotestore.Node{Path: ".", IsDir: true}}, Disc: &LibraryDisc{Directories: []DiscNode{{Path: ".", Directory: true, Node: root}}, Files: []LibraryMedia{}}}
	if libraryDiscExcluded(d, relative) {
		return m, ErrPath
	}
	for name, n := range nodes {
		if name == relative || !sourceUnder(name, relative) {
			continue
		}
		rel := strings.TrimPrefix(name, relative+"/")
		if relative == "." {
			rel = name
		}
		first := strings.Split(rel, "/")[0]
		if libraryDiscMutable(d, relative, first) {
			continue
		}
		lower := strings.ToLower(path.Base(rel))
		if strings.HasPrefix(path.Base(rel), ".nastool-") || strings.Contains(lower, ".!qb") || strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".aria2") || strings.HasSuffix(lower, ".tmp") {
			return m, ErrPath
		}
		if n.IsDir {
			m.Disc.Directories = append(m.Disc.Directories, DiscNode{Path: rel, Directory: true, Node: libraryRemoteDirectory(d, name)})
		} else {
			m.Disc.Files = append(m.Disc.Files, libraryRemoteFile(d, name, n))
		}
	}
	sort.Slice(m.Disc.Directories, func(i, j int) bool { return m.Disc.Directories[i].Path < m.Disc.Directories[j].Path })
	sort.Slice(m.Disc.Files, func(i, j int) bool { return m.Disc.Files[i].Path < m.Disc.Files[j].Path })
	if err := libraryDiscInventory(m); err != nil {
		return m, err
	}
	if err := libraryDiscLayout(relative, *m.Disc); err != nil {
		return m, err
	}
	return m, nil
}
func scanLibraryRemote(ctx context.Context, d LibraryDefinition) (LibraryScan, error) {
	result := LibraryScan{Files: []LibraryMedia{}}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return result, err
	}
	if !LibraryMode(d.Mode) || !validRelative(d.Selection) || len(d.Excludes) > 128 {
		return result, ErrPath
	}
	for _, x := range d.Excludes {
		if !validRelative(x) {
			return result, ErrPath
		}
	}
	base := path.Base(d.Root.Path)
	if strings.EqualFold(base, "BDMV") || strings.EqualFold(base, "STREAM") && strings.EqualFold(path.Base(path.Dir(d.Root.Path)), "BDMV") {
		return result, ErrPath
	}
	for _, x := range d.Excludes {
		if sourceUnder(d.Selection, x) {
			result.Skipped++
			return result, nil
		}
	}
	selection := d.Selection
	ancestor, err := libraryRemoteAncestor(ctx, c, d)
	if err != nil {
		return result, err
	}
	if ancestor != "" && ancestor != selection {
		if !d.AllowDiscExpansion {
			return result, ErrPath
		}
		selection = ancestor
	}
	nodes, err := libraryRemoteInventory(ctx, c, d, selection)
	if err != nil {
		return result, err
	}
	paths := make([]string, 0, len(nodes))
	for p := range nodes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	discs := []string{}
	appendMedia := func(m LibraryMedia) error {
		raw, err := libraryMediaDocument(m)
		if err != nil {
			return err
		}
		result.SnapshotBytes += len(raw)
		if result.SnapshotBytes > LibraryWorklistBytes || len(result.Files) >= 50000 {
			return ErrLimit
		}
		result.Files = append(result.Files, m)
		return nil
	}
	for _, relative := range paths {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		skip := false
		for _, x := range append(append([]string{}, d.Excludes...), discs...) {
			if sourceUnder(relative, x) {
				skip = true
				break
			}
		}
		if skip {
			result.Skipped++
			continue
		}
		for _, part := range strings.Split(relative, "/") {
			if part != "." && (strings.HasPrefix(part, ".") || part == "@eaDir" || part == "@Recycle" || part == "#recycle") {
				skip = true
				break
			}
		}
		if skip {
			result.Skipped++
			continue
		}
		n := nodes[relative]
		if n.IsDir {
			if !libraryRemoteCandidate(nodes, relative) {
				continue
			}
			discs = append(discs, relative)
			if libraryDiscExcluded(d, relative) {
				result.Skipped++
				continue
			}
			m, e := libraryRemoteDisc(d, relative, nodes)
			if e != nil {
				m = LibraryMedia{Path: relative, Blocked: "unsafe or incomplete remote Blu-ray inventory; no media changed"}
			} else {
				m.Disc.Expanded = selection != d.Selection
			}
			if err := appendMedia(m); err != nil {
				return result, err
			}
			continue
		}
		if FileKind(relative) != "media" {
			result.Skipped++
			continue
		}
		m := libraryRemoteFile(d, relative, n)
		for _, part := range strings.Split(relative, "/") {
			if strings.EqualFold(part, "BDMV") || strings.EqualFold(part, "CERTIFICATE") {
				m = LibraryMedia{Path: relative, Blocked: "incomplete Blu-ray inventory; STREAM clips are not individual media"}
				break
			}
		}
		if err := appendMedia(m); err != nil {
			return result, err
		}
	}
	return result, nil
}

func libraryRemoteSnapshotCurrent(ctx context.Context, d LibraryDefinition, m LibraryMedia) error {
	if m.Blocked != "" || m.Remote == nil || !validRelative(m.Path) || m.Link != "" {
		return ErrState
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return err
	}
	if m.Disc == nil {
		n, e := c.Entry(ctx, d.Root.Path, m.Path)
		if e != nil || n == nil || n.IsDir || *n != m.Remote.Node || libraryRemoteNode(d, m.Path, *n) != m.Node {
			return ErrState
		}
		return nil
	}
	nodes, err := libraryRemoteInventory(ctx, c, d, m.Path)
	if err != nil {
		return err
	}
	actual, err := libraryRemoteDisc(d, m.Path, nodes)
	if err != nil {
		return err
	}
	actual.Disc.Expanded = m.Disc.Expanded
	want := m
	disc := *m.Disc
	disc.Files = append([]LibraryMedia{}, m.Disc.Files...)
	want.Disc = &disc
	remote := *m.Remote
	remote.Digest = ""
	want.Remote = &remote
	for i, f := range disc.Files {
		if f.Remote == nil {
			return ErrState
		}
		r := *f.Remote
		r.Digest = ""
		disc.Files[i].Remote = &r
	}
	if Digest(want) != Digest(actual) {
		return ErrState
	}
	return nil
}
func LibraryRemotePrepared(m LibraryMedia) bool {
	if m.Remote == nil || len(m.Remote.Digest) != 64 {
		return false
	}
	if m.Disc != nil {
		if Digest(m.Disc) != m.Remote.Digest {
			return false
		}
		for _, f := range m.Disc.Files {
			if !LibraryRemotePrepared(f) {
				return false
			}
		}
	}
	return true
}
func verifyLibraryRemoteMedia(ctx context.Context, d LibraryDefinition, m LibraryMedia) error {
	if !LibraryRemoteApproved(d) || !LibraryRemotePrepared(m) {
		return ErrState
	}
	if err := libraryRemoteSnapshotCurrent(ctx, d, m); err != nil {
		return err
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return err
	}
	files := []LibraryMedia{m}
	if m.Disc != nil {
		files = m.Disc.Files
	}
	for _, f := range files {
		if err := remoteHash(ctx, c, path.Join(d.Root.Path, f.Path), f.Node.Size, f.Remote.Digest); err != nil {
			return err
		}
	}
	return libraryRemoteSnapshotCurrent(ctx, d, m)
}
func PrepareLibraryRemoteMedia(ctx context.Context, d LibraryDefinition, m LibraryMedia) (LibraryMedia, error) {
	if !LibraryRemoteApproved(d) || m.Remote == nil || m.Remote.Digest != "" {
		return m, ErrState
	}
	if err := libraryRemoteSnapshotCurrent(ctx, d, m); err != nil {
		return m, err
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return m, err
	}
	result := m
	remote := *m.Remote
	result.Remote = &remote
	if m.Disc == nil {
		result.Remote.Digest, err = libraryRemoteHash(ctx, c, path.Join(d.Root.Path, m.Path), m.Node.Size)
	} else {
		disc := *m.Disc
		disc.Files = append([]LibraryMedia{}, m.Disc.Files...)
		result.Disc = &disc
		for i, f := range disc.Files {
			r := *f.Remote
			disc.Files[i].Remote = &r
			r.Digest, err = libraryRemoteHash(ctx, c, path.Join(d.Root.Path, f.Path), f.Node.Size)
			if err != nil {
				return m, err
			}
		}
		result.Remote.Digest = Digest(result.Disc)
	}
	if err != nil {
		return m, err
	}
	if err := libraryRemoteSnapshotCurrent(ctx, d, result); err != nil {
		return m, err
	}
	return result, nil
}

// Reads at most one metadata object and distinguishes absence from an unsafe
// alias/directory. Exact content and inventory must survive the whole read.
func libraryRemoteReadMetadata(ctx context.Context, c *remotestore.Client, d LibraryDefinition, relative string, limit int64) ([]byte, *remotestore.Node, error) {
	n, err := c.Entry(ctx, d.Root.Path, relative)
	if err != nil {
		return nil, nil, err
	}
	if n == nil {
		return nil, nil, nil
	}
	if n.IsDir || n.Size < 0 || n.Size > limit {
		return nil, nil, ErrPath
	}
	var b strings.Builder
	if err := c.Read(ctx, path.Join(d.Root.Path, relative), n.Size, &b); err != nil {
		return nil, nil, err
	}
	after, err := c.Entry(ctx, d.Root.Path, relative)
	if err != nil || after == nil || *after != *n || int64(b.Len()) != n.Size {
		return nil, nil, ErrState
	}
	return []byte(b.String()), n, nil
}
