//go:build linux || darwin

package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

func favoriteParent(d FavoriteMoveDefinition, location string, create bool) (*os.File, string, error) {
	r, e := anchoredRoot(d.Scope.Root.Path, d.Scope.Root.Identity)
	if e != nil {
		return nil, "", e
	}
	defer r.Close()
	return anchoredParent(r, location, create)
}

// Each category may be a distinct mounted volume. Pin that exact parent from
// the approved library descriptor before using the ordinary complete manifest.
// Internal mounts inside the movie payload remain refused by the tree scanner.
func favoriteScope(d FavoriteMoveDefinition, location string) (RecoveryScope, string, error) {
	parent, leaf, e := favoriteParent(d, location, false)
	if e != nil {
		return RecoveryScope{}, "", e
	}
	defer parent.Close()
	info, e := parent.Stat()
	if e != nil {
		return RecoveryScope{}, "", e
	}
	s := d.Scope
	s.Root.Path = path.Join(d.Scope.Root.Path, path.Dir(location))
	s.Root.Identity = Identity(info)
	return s, leaf, nil
}
func favoriteSnapshot(ctx context.Context, d FavoriteMoveDefinition, location string) ([]RecoveryNode, error) {
	s, leaf, e := favoriteScope(d, location)
	if e != nil {
		return nil, e
	}
	return recoveryLocalSnapshot(ctx, s, leaf)
}
func favoritePresence(d FavoriteMoveDefinition, location string) (bool, error) {
	_, present, e := favoriteStat(d.Scope.Root, location)
	return present, e
}
func favoriteStat(root LibraryRoot, relative string) (unix.Stat_t, bool, error) {
	var empty unix.Stat_t
	if !validRelative(relative) || relative == "." {
		return empty, false, ErrPath
	}
	base, e := anchoredRoot(root.Path, root.Identity)
	if e != nil {
		return empty, false, e
	}
	defer base.Close()
	fd, e := unix.Dup(int(base.Fd()))
	if e != nil {
		return empty, false, e
	}
	defer func() { unix.Close(fd) }()
	parts := strings.Split(relative, "/")
	for i, part := range parts {
		var st unix.Stat_t
		e := unix.Fstatat(fd, part, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(e, unix.ENOENT) {
			return empty, false, nil
		}
		if e != nil {
			return empty, false, e
		}
		if i == len(parts)-1 {
			return st, true, nil
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return empty, false, ErrPath
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return empty, false, e
		}
		unix.Close(fd)
		fd = next
	}
	return empty, false, ErrPath
}
func FavoriteDirectoryExists(root LibraryRoot, relative string) (bool, error) {
	st, present, e := favoriteStat(root, relative)
	if e != nil || !present {
		return false, e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false, ErrPath
	}
	return true, nil
}
func FavoriteMovieDirectory(scope RecoveryScope, relative string) (string, error) {
	st, present, e := favoriteStat(scope.Root, relative)
	if e != nil || !present {
		return "", ErrPath
	}
	source := relative
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFREG, unix.S_IFLNK:
		source = path.Dir(relative)
	default:
		return "", ErrMode
	}
	if len(strings.Split(source, "/")) != 2 {
		return "", ErrPath
	}
	return source, nil
}
func favoriteProofParent(d FavoriteMoveDefinition, p FavoriteMoveProof) error {
	for _, check := range []struct{ location, identity string }{{d.Source, p.SourceParent}, {d.Target, p.TargetParent}} {
		if check.identity == "" {
			continue
		}
		parent, _, e := favoriteParent(d, check.location, false)
		if e != nil {
			return e
		}
		info, e := parent.Stat()
		parent.Close()
		if e != nil || Identity(info) != check.identity {
			return ErrState
		}
	}
	return nil
}
func favoriteCapture(d FavoriteMoveDefinition) string {
	return path.Join(path.Dir(d.Source), ".nastool-move-"+d.Receipt[:32])
}
func favoriteStage(d FavoriteMoveDefinition) string {
	return path.Join(path.Dir(d.Target), ".nastool-copy-"+d.Receipt[:32])
}
func favoriteTargetExact(ctx context.Context, d FavoriteMoveDefinition, nodes []RecoveryNode, location string) error {
	actual, e := favoriteSnapshot(ctx, d, location)
	if e != nil {
		return e
	}
	if !favoriteExact(actual, nodes) {
		return ErrState
	}
	return nil
}
func favoriteSourceExact(ctx context.Context, d FavoriteMoveDefinition) error {
	actual, e := favoriteSnapshot(ctx, d, d.Source)
	if e != nil {
		return e
	}
	if Digest(actual) != Digest(d.Nodes) {
		return ErrState
	}
	return nil
}
func favoritePayload(d FavoriteMoveDefinition, p FavoriteMoveProof) (*os.File, error) {
	if p.Stage != favoriteStage(d) || p.StageIdentity == "" {
		return nil, ErrState
	}
	parent, leaf, e := favoriteParent(d, p.Stage, false)
	if e != nil {
		return nil, e
	}
	defer parent.Close()
	stage, e := discPrivate(parent, leaf, p.StageIdentity)
	if e != nil {
		return nil, e
	}
	return stage, nil
}
func favoriteRemaining(ctx context.Context, d FavoriteMoveDefinition, p FavoriteMoveProof) error {
	present, e := favoritePresence(d, favoriteCapture(d))
	if e != nil {
		return e
	}
	deleted := map[string]bool{}
	for _, v := range p.Deleted {
		if deleted[v] {
			return ErrState
		}
		deleted[v] = true
	}
	if !present {
		if len(deleted) == len(d.Nodes) || len(deleted) == len(d.Nodes)-1 && p.DeleteIntent == "." {
			return nil
		}
		return ErrState
	}
	actual, e := favoriteSnapshot(ctx, d, favoriteCapture(d))
	if e != nil {
		return e
	}
	expected := map[string]RecoveryNode{}
	for _, n := range d.Nodes {
		expected[n.Path] = n
	}
	found := map[string]bool{}
	for _, n := range actual {
		want, ok := expected[n.Path]
		if !ok || deleted[n.Path] || !recoveryNodeSame(want, n) {
			return ErrState
		}
		found[n.Path] = true
	}
	for _, n := range d.Nodes {
		if !found[n.Path] && !deleted[n.Path] && p.DeleteIntent != n.Path {
			return ErrState
		}
	}
	return nil
}
func favoriteCopyEvidence(ctx context.Context, d FavoriteMoveDefinition, p FavoriteMoveProof) error {
	if p.StageIdentity == "" {
		present, e := favoritePresence(d, favoriteStage(d))
		if e != nil || present {
			return ErrState
		}
		return nil
	}
	stage, e := favoritePayload(d, p)
	if e != nil {
		return e
	}
	defer stage.Close()
	// Only the payload is allowed inside the pinned private directory.
	names, e := stage.Readdirnames(-1)
	if e != nil {
		return e
	}
	for _, n := range names {
		if n != "payload" {
			return ErrState
		}
	}
	if len(p.Copies) == 0 {
		if len(names) > 0 {
			return ErrState
		}
		return nil
	}
	source := map[string]RecoveryNode{}
	for _, n := range d.Nodes {
		source[n.Path] = n
	}
	copies := map[string]FavoriteCopyNode{}
	for _, n := range p.Copies {
		if _, ok := source[n.Path]; !ok || copies[n.Path].Node.Node.Identity != "" || n.Node.Path != n.Path || n.Node.Node.Identity == "" {
			return ErrState
		}
		copies[n.Path] = n
	}
	scope := d.Scope
	scope.Root.Path = path.Join(d.Scope.Root.Path, p.Stage)
	scope.Root.Identity = p.StageIdentity
	actual, e := recoveryLocalInventory(ctx, scope, "payload")
	if e != nil {
		return e
	}
	if len(actual) != len(copies) {
		return ErrState
	}
	for _, n := range actual {
		c, ok := copies[n.Path]
		want := source[n.Path]
		if !ok || n.Kind != want.Kind || n.Node.Identity != c.Node.Node.Identity {
			return ErrState
		}
		if n.Kind == "link" && (n.Link != want.Link || n.Digest != want.Digest) {
			return ErrState
		}
		if n.Kind == "file" {
			if n.Node.Size < 0 || n.Node.Size > want.Node.Size {
				return ErrState
			}
			if e := favoritePrefix(ctx, d, p, n.Path, n.Node.Size); e != nil {
				return e
			}
			if c.Done && n.Node.Size != want.Node.Size {
				return ErrState
			}
		}
	}
	return nil
}
func favoriteReviewFiles(ctx context.Context, d FavoriteMoveDefinition, p FavoriteMoveProof, phase string) error {
	if e := favoriteProofParent(d, p); e != nil {
		return e
	}
	switch phase {
	case "planned", "preparing":
		if e := favoriteSourceExact(ctx, d); e != nil {
			return e
		}
		present, e := favoritePresence(d, d.Target)
		if e != nil || present {
			return ErrState
		}
		return nil
	case "rename":
		present, e := favoritePresence(d, d.Source)
		if e != nil {
			return e
		}
		if present {
			if e := favoriteSourceExact(ctx, d); e != nil {
				return e
			}
			target, e := favoritePresence(d, d.Target)
			if e != nil || target {
				return ErrState
			}
			return nil
		}
		return favoriteTargetExact(ctx, d, d.Nodes, d.Target)
	case "copying":
		if e := favoriteSourceExact(ctx, d); e != nil {
			return e
		}
		present, e := favoritePresence(d, d.Target)
		if e != nil || present {
			return ErrState
		}
		return favoriteCopyEvidence(ctx, d, p)
	case "publish":
		if e := favoriteSourceExact(ctx, d); e != nil {
			return e
		}
		target, e := favoritePresence(d, d.Target)
		if e != nil {
			return e
		}
		if target {
			return favoriteTargetExact(ctx, d, p.Output, d.Target)
		}
		return favoriteTargetExact(ctx, d, p.Output, path.Join(p.Stage, "payload"))
	case "capture":
		if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
			return e
		}
		held, e := favoritePresence(d, favoriteCapture(d))
		if e != nil {
			return e
		}
		if held {
			return favoriteTargetExact(ctx, d, d.Nodes, favoriteCapture(d))
		}
		return favoriteSourceExact(ctx, d)
	case "cleaning":
		if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
			return e
		}
		return favoriteRemaining(ctx, d, p)
	case "cleanup":
		if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
			return e
		}
		present, e := favoritePresence(d, favoriteCapture(d))
		if e != nil || present {
			return ErrState
		}
		return nil
	case "moved":
		return favoriteTargetExact(ctx, d, d.Nodes, d.Target)
	default:
		return ErrState
	}
}
func favoritePrefix(ctx context.Context, d FavoriteMoveDefinition, p FavoriteMoveProof, relative string, size int64) error {
	sourceParent, sourceLeaf, e := favoriteParent(d, path.Join(d.Source, relative), false)
	if e != nil {
		return e
	}
	defer sourceParent.Close()
	source, e := openRegular(sourceParent, sourceLeaf)
	if e != nil {
		return e
	}
	defer source.Close()
	stage, e := favoritePayload(d, p)
	if e != nil {
		return e
	}
	defer stage.Close()
	parent, leaf, e := anchoredParent(stage, path.Join("payload", relative), false)
	if e != nil {
		return e
	}
	defer parent.Close()
	output, e := openRegular(parent, leaf)
	if e != nil {
		return e
	}
	defer output.Close()
	h := sha256.New()
	n, e := io.CopyBuffer(h, contextReader{ctx, io.NewSectionReader(source, 0, size)}, make([]byte, 256<<10))
	if e != nil || n != size {
		return ErrState
	}
	sourceDigest := hex.EncodeToString(h.Sum(nil))
	outDigest, e := hashSource(ctx, output, size)
	if e != nil || sourceDigest != outDigest {
		return ErrState
	}
	return nil
}
func favoriteCopyOne(ctx context.Context, d FavoriteMoveDefinition, p *FavoriteMoveProof, node RecoveryNode, current func() error, save func(string) error) error {
	stage, e := favoritePayload(d, *p)
	if e != nil {
		return e
	}
	defer stage.Close()
	relative := path.Join("payload", node.Path)
	parent, leaf, e := anchoredParent(stage, relative, false)
	if e != nil {
		return e
	}
	defer parent.Close()
	ordinal := -1
	for i, c := range p.Copies {
		if c.Path == node.Path {
			ordinal = i
			break
		}
	}
	if ordinal < 0 {
		if e := current(); e != nil {
			return e
		}
		switch node.Kind {
		case "directory":
			e = unix.Mkdirat(int(parent.Fd()), leaf, 0700)
		case "link":
			e = unix.Symlinkat(node.Link, int(parent.Fd()), leaf)
		case "file":
			fd, err := unix.Openat(int(parent.Fd()), leaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
			e = err
			if err == nil {
				e = unix.Close(fd)
			}
		default:
			return ErrMode
		}
		if e != nil {
			return e
		}
		if e := parent.Sync(); e != nil {
			return e
		}
		created, e := recoveryLocalHeader(parent, leaf, node.Path)
		if e != nil {
			return e
		}
		p.Copies = append(p.Copies, FavoriteCopyNode{Path: node.Path, Node: created})
		ordinal = len(p.Copies) - 1
		// A crash before this receipt leaves an unadopted object: review refuses
		// it; a name alone is never used to claim a new staging node.
		if e := save("copying"); e != nil {
			return e
		}
	}
	known := p.Copies[ordinal]
	actual, e := recoveryLocalHeader(parent, leaf, node.Path)
	if e != nil || actual.Kind != node.Kind || actual.Node.Identity != known.Node.Node.Identity {
		return ErrState
	}
	if known.Done {
		return nil
	}
	if node.Kind == "file" {
		if actual.Node.Links != 0 || actual.Node.Size > node.Node.Size {
			return ErrState
		}
		if e := favoritePrefix(ctx, d, *p, node.Path, actual.Node.Size); e != nil {
			return e
		}
		sourceParent, sourceLeaf, e := favoriteParent(d, path.Join(d.Source, node.Path), false)
		if e != nil {
			return e
		}
		defer sourceParent.Close()
		source, e := openRegular(sourceParent, sourceLeaf)
		if e != nil {
			return e
		}
		defer source.Close()
		sourceInfo, e := source.Stat()
		if e != nil || Identity(sourceInfo) != node.Node.Identity || sourceInfo.Size() != node.Node.Size {
			return ErrState
		}
		fd, e := unix.Openat(int(parent.Fd()), leaf, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return e
		}
		output := os.NewFile(uintptr(fd), leaf)
		defer output.Close()
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || statIdentity(st) != known.Node.Node.Identity || st.Nlink != 1 {
			return ErrState
		}
		if _, e := output.Seek(actual.Node.Size, io.SeekStart); e != nil {
			return e
		}
		if _, e := source.Seek(actual.Node.Size, io.SeekStart); e != nil {
			return e
		}
		left := node.Node.Size - actual.Node.Size
		buffer := make([]byte, 1<<20)
		for left > 0 {
			if e := current(); e != nil {
				return e
			}
			n, e := source.Read(buffer[:min(int64(len(buffer)), left)])
			if n > 0 {
				written, werr := output.Write(buffer[:n])
				if werr != nil {
					return werr
				}
				if written != n {
					return io.ErrShortWrite
				}
				left -= int64(n)
			}
			if e != nil {
				return e
			}
			if n == 0 {
				return io.ErrNoProgress
			}
		}
		if e := unix.Fchmod(fd, node.Mode&07777); e != nil {
			return e
		}
		times := []unix.Timeval{unix.NsecToTimeval(node.Node.Modified), unix.NsecToTimeval(node.Node.Modified)}
		if e := unix.Futimes(fd, times); e != nil {
			return e
		}
		if e := output.Sync(); e != nil {
			return e
		}
		if e := favoritePrefix(ctx, d, *p, node.Path, node.Node.Size); e != nil {
			return e
		}
	}
	if node.Kind == "link" && (actual.Link != node.Link || actual.Digest != node.Digest) {
		return ErrState
	}
	p.Copies[ordinal].Done = true
	return save("copying")
}
func favoriteMakeStage(d FavoriteMoveDefinition, p *FavoriteMoveProof, save func(string) error) error {
	if p.StageIdentity != "" {
		stage, e := favoritePayload(d, *p)
		if e == nil {
			stage.Close()
		}
		return e
	}
	parent, leaf, e := favoriteParent(d, favoriteStage(d), false)
	if e != nil {
		return e
	}
	defer parent.Close()
	if e := unix.Mkdirat(int(parent.Fd()), leaf, 0700); e != nil {
		return e
	}
	stage, e := discDir(parent, leaf)
	if e != nil {
		return e
	}
	defer stage.Close()
	info, e := stage.Stat()
	if e != nil {
		return e
	}
	p.Stage, p.StageIdentity = favoriteStage(d), Identity(info)
	if e := stage.Sync(); e != nil {
		return e
	}
	if e := parent.Sync(); e != nil {
		return e
	}
	return save("copying")
}
func favoriteRename(ctx context.Context, d FavoriteMoveDefinition, p FavoriteMoveProof, from, to string, current func() error) error {
	a, leaf, e := favoriteParent(d, from, false)
	if e != nil {
		return e
	}
	defer a.Close()
	b, name, e := favoriteParent(d, to, false)
	if e != nil {
		return e
	}
	defer b.Close()
	if e := current(); e != nil {
		return e
	}
	if e := favoriteProofParent(d, p); e != nil {
		return e
	}
	if path.Dir(from) == p.Stage {
		info, e := a.Stat()
		if e != nil || Identity(info) != p.StageIdentity {
			return ErrState
		}
	}
	before, e := lstatAt(a, leaf)
	if e != nil {
		return e
	}
	wanted := d.Nodes[0].Node.Identity
	if from == path.Join(p.Stage, "payload") {
		if len(p.Output) == 0 {
			return ErrState
		}
		wanted = p.Output[0].Node.Identity
	}
	if statIdentity(before) != wanted {
		return ErrState
	}
	if e := renameNoReplace(int(a.Fd()), leaf, int(b.Fd()), name); e != nil {
		return e
	}
	after, e := lstatAt(b, name)
	if e != nil {
		return e
	}
	if statIdentity(before) != statIdentity(after) {
		// Never delete or adopt a substituted directory. A no-overwrite return is
		// attempted only to the empty original name; an occupied name is retained.
		_ = renameNoReplace(int(b.Fd()), name, int(a.Fd()), leaf)
		return ErrState
	}
	if e := a.Sync(); e != nil {
		return e
	}
	if e := b.Sync(); e != nil {
		return e
	}
	return ctx.Err()
}
func favoriteExecute(ctx context.Context, d FavoriteMoveDefinition, p *FavoriteMoveProof, phase *string, current func() error, save func(string) error) error {
	if *phase == "planned" {
		if e := save("preparing"); e != nil {
			return e
		}
	}
	if *phase == "preparing" {
		if e := favoriteSourceExact(ctx, d); e != nil {
			return e
		}
		if e := current(); e != nil {
			return e
		}
		targetParent, _, e := favoriteParent(d, d.Target, true)
		if e != nil {
			return e
		}
		defer targetParent.Close()
		sourceParent, _, e := favoriteParent(d, d.Source, false)
		if e != nil {
			return e
		}
		defer sourceParent.Close()
		a, e := sourceParent.Stat()
		if e != nil {
			return e
		}
		b, e := targetParent.Stat()
		if e != nil {
			return e
		}
		p.SourceParent, p.TargetParent = Identity(a), Identity(b)
		p.Method = "rename"
		if e := save("rename"); e != nil {
			return e
		}
	}
	if *phase == "rename" {
		exists, e := favoritePresence(d, d.Source)
		if e != nil {
			return e
		}
		if exists {
			if e := favoriteSourceExact(ctx, d); e != nil {
				return e
			}
			e = favoriteRename(ctx, d, *p, d.Source, d.Target, current)
			if errors.Is(e, unix.EXDEV) {
				p.Method = "copy"
				if e := save("copying"); e != nil {
					return e
				}
			} else if e != nil {
				return e
			}
		}
		if *phase == "rename" {
			if e := favoriteTargetExact(ctx, d, d.Nodes, d.Target); e != nil {
				return e
			}
			return save("moved")
		}
	}
	if *phase == "copying" {
		if e := favoriteSourceExact(ctx, d); e != nil {
			return e
		}
		if e := favoriteMakeStage(d, p, save); e != nil {
			return e
		}
		if e := favoriteCopyEvidence(ctx, d, *p); e != nil {
			return e
		}
		for _, n := range d.Nodes {
			if e := favoriteCopyOne(ctx, d, p, n, current, save); e != nil {
				return e
			}
		}
		// Apply final directory permissions only after all children exist.
		stage, e := favoritePayload(d, *p)
		if e != nil {
			return e
		}
		for i := len(d.Nodes) - 1; i >= 0; i-- {
			n := d.Nodes[i]
			if n.Kind != "directory" {
				continue
			}
			parent, leaf, e := anchoredParent(stage, path.Join("payload", n.Path), false)
			if e != nil {
				stage.Close()
				return e
			}
			dir, e := discDir(parent, leaf)
			parent.Close()
			if e != nil {
				stage.Close()
				return e
			}
			e = unix.Fchmod(int(dir.Fd()), n.Mode&07777)
			if e == nil {
				e = dir.Sync()
			}
			dir.Close()
			if e != nil {
				stage.Close()
				return e
			}
		}
		stage.Close()
		if e := favoriteSourceExact(ctx, d); e != nil {
			return e
		}
		output, e := favoriteSnapshot(ctx, d, path.Join(p.Stage, "payload"))
		if e != nil || favoriteContent(output) != favoriteContent(d.Nodes) {
			return ErrState
		}
		p.Output = output
		if e := save("publish"); e != nil {
			return e
		}
	}
	if *phase == "publish" {
		if e := favoriteSourceExact(ctx, d); e != nil {
			return e
		}
		target, e := favoritePresence(d, d.Target)
		if e != nil {
			return e
		}
		if !target {
			if e := favoriteTargetExact(ctx, d, p.Output, path.Join(p.Stage, "payload")); e != nil {
				return e
			}
			if e := favoriteRename(ctx, d, *p, path.Join(p.Stage, "payload"), d.Target, current); e != nil {
				return e
			}
		}
		if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
			return e
		}
		if e := save("capture"); e != nil {
			return e
		}
	}
	if *phase == "capture" {
		if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
			return e
		}
		held, e := favoritePresence(d, favoriteCapture(d))
		if e != nil {
			return e
		}
		if !held {
			if e := favoriteSourceExact(ctx, d); e != nil {
				return e
			}
			if e := favoriteRename(ctx, d, *p, d.Source, favoriteCapture(d), current); e != nil {
				return e
			}
		}
		if e := favoriteTargetExact(ctx, d, d.Nodes, favoriteCapture(d)); e != nil {
			return e
		}
		if e := save("cleaning"); e != nil {
			return e
		}
	}
	if *phase == "cleaning" {
		if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
			return e
		}
		if e := favoriteRemaining(ctx, d, *p); e != nil {
			return e
		}
		nodes := append([]RecoveryNode{}, d.Nodes...)
		sort.Slice(nodes, func(i, j int) bool {
			a, b := strings.Count(nodes[i].Path, "/"), strings.Count(nodes[j].Path, "/")
			if nodes[i].Path == "." {
				a = -1
			}
			if nodes[j].Path == "." {
				b = -1
			}
			if a != b {
				return a > b
			}
			return nodes[i].Path > nodes[j].Path
		})
		done := map[string]bool{}
		for _, s := range p.Deleted {
			done[s] = true
		}
		for _, n := range nodes {
			if done[n.Path] {
				continue
			}
			if e := current(); e != nil {
				return e
			}
			if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
				return e
			}
			if e := favoriteRemaining(ctx, d, *p); e != nil {
				return e
			}
			location := path.Join(favoriteCapture(d), n.Path)
			present, e := favoritePresence(d, location)
			if e != nil {
				return e
			}
			if !present && p.DeleteIntent != n.Path {
				return ErrState
			}
			p.DeleteIntent = n.Path
			if e := save("cleaning"); e != nil {
				return e
			}
			if present {
				scope, leaf, e := favoriteScope(d, location)
				if e != nil {
					return e
				}
				if e := recoveryLocalRemove(ctx, scope, leaf, n, current); e != nil {
					return e
				}
			}
			p.Deleted = append(p.Deleted, n.Path)
			p.DeleteIntent = ""
			if e := save("cleaning"); e != nil {
				return e
			}
		}
		if e := save("cleanup"); e != nil {
			return e
		}
	}
	if *phase == "cleanup" {
		if e := favoriteTargetExact(ctx, d, p.Output, d.Target); e != nil {
			return e
		}
		held, e := favoritePresence(d, favoriteCapture(d))
		if e != nil || held {
			return ErrState
		}
		exists, e := favoritePresence(d, p.Stage)
		if e != nil {
			return e
		}
		if exists {
			stage, e := favoritePayload(d, *p)
			if e != nil {
				return e
			}
			names, e := stage.Readdirnames(1)
			stage.Close()
			if e != io.EOF || len(names) != 0 {
				return ErrState
			}
			parent, leaf, e := favoriteParent(d, p.Stage, false)
			if e != nil {
				return e
			}
			defer parent.Close()
			if e := current(); e != nil {
				return e
			}
			if e := unix.Unlinkat(int(parent.Fd()), leaf, unix.AT_REMOVEDIR); e != nil {
				return e
			}
			if e := parent.Sync(); e != nil {
				return e
			}
		}
		return nil
	}
	if *phase == "moved" {
		return favoriteTargetExact(ctx, d, d.Nodes, d.Target)
	}
	return ErrState
}
