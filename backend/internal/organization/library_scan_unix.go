//go:build linux || darwin

package organization

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

func libraryNode(parent *os.File, name string) (LibraryNode, uint32, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return LibraryNode{}, 0, err
	}
	pi, err := parent.Stat()
	if err != nil {
		return LibraryNode{}, 0, err
	}
	return LibraryNode{Identity: statIdentity(st), Parent: Identity(pi), Size: st.Size, Modified: st.Mtim.Sec*1e9 + st.Mtim.Nsec, Changed: [2]int64{st.Ctim.Sec, st.Ctim.Nsec}, Links: uint64(st.Nlink)}, uint32(st.Mode) & unix.S_IFMT, nil
}
func libraryRoot(roots []LibraryRoot, id string) (LibraryRoot, bool) {
	for _, r := range roots {
		if r.ID == id {
			return r, true
		}
	}
	return LibraryRoot{}, false
}
func captureLibraryMedia(d LibraryDefinition, path string) (LibraryMedia, error) {
	m := LibraryMedia{Path: path}
	// Whole-disc intake has its own frozen directory proof. Bulk library
	// rewriting must not reinterpret its STREAM clips as standalone movies.
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.EqualFold(part, "BDMV") || strings.EqualFold(part, "CERTIFICATE") {
			return m, ErrMode
		}
	}
	if FileKind(path) != "media" {
		return m, ErrPath
	}
	return captureLibraryFile(d, path)
}
func captureLibraryFile(d LibraryDefinition, path string) (LibraryMedia, error) {
	m := LibraryMedia{Path: path}
	if !validRelative(path) || path == "." {
		return m, ErrPath
	}
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return m, err
	}
	defer r.Close()
	p, n, err := anchoredParent(r, path, false)
	if err != nil {
		return m, err
	}
	defer p.Close()
	node, kind, err := libraryNode(p, n)
	if err != nil {
		return m, err
	}
	m.Node = node
	if kind == unix.S_IFREG {
		m.InputRoot, m.InputPath, m.Input = d.Root.ID, path, node
		return m, nil
	}
	if kind != unix.S_IFLNK {
		return m, ErrPath
	}
	m.Link, err = readLinkAt(p, n)
	if err != nil {
		return m, err
	}
	target := m.Link
	if !filepath.IsAbs(target) {
		target = filepath.Join(d.Root.Path, filepath.Dir(path), target)
	}
	target = filepath.Clean(target)
	all := append([]LibraryRoot{d.Root}, d.Allowed...)
	sort.Slice(all, func(i, j int) bool { return len(all[i].Path) > len(all[j].Path) })
	for _, allowed := range all {
		rel, e := filepath.Rel(allowed.Path, target)
		if e != nil || !validRelative(rel) || rel == "." {
			continue
		}
		input, e := anchoredRoot(allowed.Path, allowed.Identity)
		if e != nil {
			return m, e
		}
		ip, in, e := anchoredParent(input, rel, false)
		input.Close()
		if e != nil {
			return m, e
		}
		v, k, e := libraryNode(ip, in)
		ip.Close()
		if e != nil || k != unix.S_IFREG {
			return m, ErrPath
		}
		m.InputRoot, m.InputPath, m.Input = allowed.ID, rel, v
		return m, nil
	}
	return m, ErrPath
}
func VerifyLibraryMedia(ctx context.Context, d LibraryDefinition, m LibraryMedia) error {
	if d.Remote != nil {
		return verifyLibraryRemoteMedia(ctx, d, m)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if m.Blocked != "" {
		return ErrState
	}
	if m.Disc != nil {
		return verifyLibraryDisc(ctx, d, m)
	}
	actual, err := captureLibraryMedia(d, m.Path)
	if err != nil {
		return err
	}
	if Digest(actual) != Digest(m) {
		return ErrState
	}
	return nil
}
func ReadLibraryMedia(ctx context.Context, d LibraryDefinition, m LibraryMedia, consume func(*os.File) ([]byte, error)) ([]byte, error) {
	if d.Remote != nil {
		return nil, ErrMode
	}
	if m.Disc != nil {
		return nil, ErrMode
	} // Never guess a representative STREAM clip.
	if consume == nil || VerifyLibraryMedia(ctx, d, m) != nil {
		return nil, ErrState
	}
	root := d.Root
	if m.InputRoot != root.ID {
		var ok bool
		root, ok = libraryRoot(d.Allowed, m.InputRoot)
		if !ok {
			return nil, ErrPath
		}
	}
	r, err := anchoredRoot(root.Path, root.Identity)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	p, n, err := anchoredParent(r, m.InputPath, false)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	f, err := openRegular(p, n)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	v, k, err := libraryNode(p, n)
	info, e := f.Stat()
	if err != nil || e != nil || k != unix.S_IFREG || Digest(v) != Digest(m.Input) || Identity(info) != m.Input.Identity {
		return nil, ErrState
	}
	b, err := consume(f)
	if err != nil {
		return nil, err
	}
	if err := VerifyLibraryMedia(ctx, d, m); err != nil {
		return nil, err
	}
	return b, nil
}
func LibraryReadNFO(ctx context.Context, d LibraryDefinition, m LibraryMedia, relative string) ([]byte, error) {
	if d.Remote != nil {
		if VerifyLibraryMedia(ctx, d, m) != nil || !validRelative(relative) || filepath.Ext(relative) != ".nfo" {
			return nil, ErrState
		}
		c, err := libraryRemoteClient(ctx, d)
		if err != nil {
			return nil, err
		}
		data, node, err := libraryRemoteReadMetadata(ctx, c, d, relative, 2<<20)
		if err != nil {
			return nil, err
		}
		if node == nil {
			return nil, os.ErrNotExist
		}
		if err := VerifyLibraryMedia(ctx, d, m); err != nil {
			return nil, err
		}
		return data, nil
	}
	if VerifyLibraryMedia(ctx, d, m) != nil || !validRelative(relative) || filepath.Ext(relative) != ".nfo" {
		return nil, ErrState
	}
	if m.Disc != nil {
		return libraryReadDiscNFO(ctx, d, m, relative)
	}
	r, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	p, n, err := anchoredParent(r, relative, false)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	before, k, err := libraryNode(p, n)
	if err != nil {
		return nil, err
	}
	if k != unix.S_IFREG || before.Size > 2<<20 {
		return nil, ErrPath
	}
	f, err := openRegular(p, n)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || Identity(info) != before.Identity {
		return nil, ErrState
	}
	b, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: f}, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return nil, ErrLimit
	}
	after, _, err := libraryNode(p, n)
	if err != nil || before != after || VerifyLibraryMedia(ctx, d, m) != nil {
		return nil, ErrState
	}
	return b, nil
}
func ScanLibrary(ctx context.Context, d LibraryDefinition) (LibraryScan, error) {
	if d.Remote != nil {
		return scanLibraryRemote(ctx, d)
	}
	result := LibraryScan{Files: []LibraryMedia{}}
	base := filepath.Base(d.Root.Path)
	if strings.EqualFold(base, "BDMV") || strings.EqualFold(base, "STREAM") && strings.EqualFold(filepath.Base(filepath.Dir(d.Root.Path)), "BDMV") {
		return result, ErrPath
	}
	if !LibraryMode(d.Mode) || !validRelative(d.Selection) || len(d.Excludes) > 128 {
		return result, ErrPath
	}
	for _, x := range d.Excludes {
		if !validRelative(x) {
			return result, ErrPath
		}
	}
	root, err := anchoredRoot(d.Root.Path, d.Root.Identity)
	if err != nil {
		return result, err
	}
	defer root.Close()
	visited := 0
	appendMedia := func(m LibraryMedia) error {
		raw, err := libraryMediaDocument(m)
		if err != nil {
			return err
		}
		result.SnapshotBytes += len(raw)
		if result.SnapshotBytes > LibraryWorklistBytes || len(result.Files) >= 50_000 {
			return ErrLimit
		}
		result.Files = append(result.Files, m)
		return nil
	}
	selection := d.Selection
	for _, excluded := range d.Excludes {
		if sourceUnder(selection, excluded) {
			result.Skipped++
			return result, nil
		}
	}
	ancestor, err := libraryDiscAncestor(ctx, d)
	if err != nil {
		return result, err
	}
	if ancestor != "" && ancestor != selection {
		if !d.AllowDiscExpansion {
			return result, ErrPath
		}
		selection = ancestor
	}
	var walk func(string, int) error
	walk = func(relative string, depth int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		visited++
		if visited > 1_000_000 || depth > 64 {
			return ErrLimit
		}
		for _, excluded := range d.Excludes {
			if excluded == "." || relative == excluded || strings.HasPrefix(relative, excluded+string(filepath.Separator)) {
				result.Skipped++
				return nil
			}
		}
		if relative != "." && (strings.HasPrefix(filepath.Base(relative), ".") || filepath.Base(relative) == "@eaDir" || filepath.Base(relative) == "@Recycle" || filepath.Base(relative) == "#recycle") {
			result.Skipped++
			return nil
		}
		var directory *os.File
		if relative == "." {
			fd, e := unix.Dup(int(root.Fd()))
			if e != nil {
				return e
			}
			unix.CloseOnExec(fd)
			directory = os.NewFile(uintptr(fd), "library root")
		} else {
			parent, name, e := anchoredParent(root, relative, false)
			if e != nil {
				return e
			}
			_, kind, e := libraryNode(parent, name)
			if e != nil {
				parent.Close()
				return e
			}
			if kind != unix.S_IFDIR {
				parent.Close()
				if FileKind(relative) != "media" {
					result.Skipped++
					return nil
				}
				media, e := captureLibraryMedia(d, relative)
				if e != nil {
					media = LibraryMedia{Path: relative, Blocked: "unsafe or unreadable media source; no file operations permitted"}
					if errors.Is(e, ErrMode) {
						media.Blocked = "incomplete or unsupported Blu-ray directory; STREAM clips are not standalone media"
					}
				}
				return appendMedia(media)
			}
			fd, e := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			parent.Close()
			if e != nil {
				return e
			}
			directory = os.NewFile(uintptr(fd), relative)
		}
		defer directory.Close()
		if libraryDiscCandidate(directory) {
			if libraryDiscExcluded(d, relative) {
				result.Skipped++
				return nil
			}
			media, err := captureLibraryDisc(ctx, d, relative)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(err, ErrLimit) {
					return err
				}
				media = LibraryMedia{Path: relative, Blocked: "unsafe, changed or incomplete Blu-ray inventory/input binding; no file operations permitted"}
			} else {
				media.Disc.Expanded = ancestor != "" && ancestor != d.Selection
				visited += media.Disc.visited
				if visited > 1_000_000 {
					return ErrLimit
				}
			}
			return appendMedia(media)
		}
		for {
			entries, e := directory.ReadDir(256)
			if e != nil && !errors.Is(e, io.EOF) {
				return e
			}
			for _, entry := range entries {
				if err := walk(filepath.Join(relative, entry.Name()), depth+1); err != nil {
					return err
				}
			}
			if errors.Is(e, io.EOF) {
				break
			}
		}
		return nil
	}
	if err := walk(selection, 0); err != nil {
		return LibraryScan{}, err
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	return result, nil
}
