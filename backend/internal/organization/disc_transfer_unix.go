//go:build linux || darwin

package organization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func discManifest(d Definition, e Entry) (DiscManifest, error) {
	return discManifestDefinition(d, e)
}
func discSource(ctx context.Context, d Definition, e Entry) (*os.File, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Callers validate the manifest/proof at their operation boundary. This
	// helper only pins the source binding; repeating a full manifest digest
	// inside each leaf check would make large discs quadratic in file count.
	if !IsDisc(e) || e.Identity == "" || !validRelative(e.Source) {
		return nil, ErrState
	}
	r, err := anchoredRoot(d.SourceRoot, d.SourceIdentity)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := discRoot(r, e.Source)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || Identity(info) != e.Identity {
		f.Close()
		return nil, ErrState
	}
	return f, nil
}
func discFiles(p Proof) map[string]DiscFileProof {
	m := map[string]DiscFileProof{}
	if p.Disc != nil {
		for _, f := range p.Disc.Files {
			m[f.Path] = f
		}
	}
	return m
}
func discLinkCounts(m DiscManifest) map[string]uint64 {
	counts := map[string]uint64{}
	for _, n := range m.Nodes {
		if !n.Directory {
			counts[n.Node.Identity]++
		}
	}
	return counts
}
func discSourceStamp(ctx context.Context, d Definition, e Entry, p *Proof, hash bool, completed ...bool) (string, error) {
	m, err := discManifest(d, e)
	if err != nil {
		return "", err
	}
	f, err := discSource(ctx, d, e)
	if err != nil {
		return "", err
	}
	defer f.Close()
	actual, err := discWalk(ctx, f, false)
	if err != nil {
		return "", err
	}
	if len(actual) != len(m.Nodes) {
		return "", ErrState
	}
	proofs := map[string]DiscFileProof{}
	linkCounts := discLinkCounts(m)
	if p != nil {
		proofs = discFiles(*p)
	}
	for i, n := range actual {
		want := m.Nodes[i]
		if n.Path != want.Path || n.Directory != want.Directory {
			return "", ErrState
		}
		if !n.Directory && d.Mode == "link" && p != nil {
			linksOK := n.Node.Links == want.Node.Links+2*linkCounts[want.Node.Identity] || len(completed) == 1 && completed[0] && n.Node.Links == want.Node.Links+linkCounts[want.Node.Identity]
			if n.Node.Identity != want.Node.Identity || n.Node.Parent != want.Node.Parent || n.Node.Size != want.Node.Size || n.Node.Modified != want.Node.Modified || !linksOK {
				return "", ErrState
			}
		} else if n.Node != want.Node {
			return "", ErrState
		}
		if hash && !n.Directory {
			fp, ok := proofs[n.Path]
			if !ok {
				return "", ErrState
			}
			if err := discHashFile(ctx, f, n.Path, n.Node.Identity, n.Node.Size, fp.Digest); err != nil {
				return "", err
			}
		}
	}
	root, _, err := libraryNode(f, ".")
	if err != nil || root.Identity != m.Root.Identity || root.Modified != m.Root.Modified || root.Changed != m.Root.Changed || root.Links != m.Root.Links {
		return "", ErrState
	}
	return Digest([]any{root, actual}), nil
}
func discHashFile(ctx context.Context, root *os.File, path, id string, size int64, want string) error {
	p, n, err := anchoredParent(root, path, false)
	if err != nil {
		return err
	}
	defer p.Close()
	f, err := openRegular(p, n)
	if err != nil {
		return err
	}
	defer f.Close()
	before, kind, err := libraryNode(p, n)
	info, e := f.Stat()
	if err != nil || e != nil || kind != unix.S_IFREG || before.Identity != id || Identity(info) != id || before.Size != size {
		return ErrState
	}
	hash, err := hashSource(ctx, f, size)
	if err != nil {
		return err
	}
	after, _, err := libraryNode(p, n)
	if err != nil || before != after || hash != want {
		return ErrState
	}
	return nil
}
func discHashes(ctx context.Context, source *os.File, m DiscManifest) (map[string]string, error) {
	result := map[string]string{}
	for _, n := range m.Nodes {
		if n.Directory {
			continue
		}
		p, leaf, err := anchoredParent(source, n.Path, false)
		if err != nil {
			return nil, err
		}
		node, _, err := libraryNode(p, leaf)
		if err != nil || node != n.Node {
			p.Close()
			return nil, ErrState
		}
		f, err := openRegular(p, leaf)
		if err != nil {
			p.Close()
			return nil, err
		}
		info, err := f.Stat()
		if err != nil || Identity(info) != n.Node.Identity {
			f.Close()
			p.Close()
			return nil, ErrState
		}
		hash, err := hashSource(ctx, f, n.Node.Size)
		f.Close()
		after, _, e := libraryNode(p, leaf)
		p.Close()
		if err != nil {
			return nil, err
		}
		if e != nil || after != node {
			return nil, ErrState
		}
		result[n.Path] = hash
	}
	return result, nil
}
func discPrivate(parent *os.File, name, id string) (*os.File, error) {
	if filepath.Base(name) != name || !validRelative(name) {
		return nil, ErrPath
	}
	f, err := discDir(parent, name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	var st unix.Stat_t
	if err != nil || Identity(info) != id || info.Mode().Perm() != 0700 || unix.Fstat(int(f.Fd()), &st) != nil || int(st.Uid) != os.Geteuid() {
		f.Close()
		return nil, ErrState
	}
	return f, nil
}
func discTargetParent(d Definition, e Entry, p *Proof, create bool) (*os.File, string, error) {
	r, err := anchoredRoot(d.TargetRoot, d.TargetIdentity)
	if err != nil {
		return nil, "", err
	}
	defer r.Close()
	parent, name, err := anchoredParent(r, e.Target, create)
	if err != nil {
		return nil, "", err
	}
	if p != nil {
		info, e := parent.Stat()
		if e != nil || Identity(info) != p.ParentIdentity {
			parent.Close()
			return nil, "", ErrState
		}
	}
	return parent, name, nil
}
func discProofValid(d Definition, e Entry, p Proof) error {
	if !SupportedMode(d.Mode) || e.Kind != "media" {
		return ErrState
	}
	m, err := discManifest(d, e)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(p.Temp, ".nastool-copy-") || filepath.Base(p.Temp) != p.Temp || !validRelative(p.Temp) || p.StagingIdentity == "" || p.ParentIdentity == "" {
		return ErrState
	}
	if p.Disc == nil || p.Identity == "" || p.Kind != "directory" || p.LinkTarget != "" || p.Digest != Digest(p.Disc) || !p.Anchor || len(p.Disc.Files) != e.Disc.Files || len(p.Disc.Directories) != e.Disc.Directories+1 || len(p.Disc.WitnessDirectories) != e.Disc.Directories+1 || p.Disc.Directories["."] != p.Identity || p.Disc.WitnessDirectories["."] == "" {
		return ErrState
	}
	files := discFiles(p)
	if len(files) != len(p.Disc.Files) {
		return ErrState
	}
	for _, n := range m.Nodes {
		if n.Directory {
			if p.Disc.Directories[n.Path] == "" || p.Disc.WitnessDirectories[n.Path] == "" {
				return ErrState
			}
		} else {
			fp, ok := files[n.Path]
			if !ok || fp.Identity == "" || len(fp.Digest) != 64 {
				return ErrState
			}
			if d.Mode == "softlink" {
				if fp.LinkTarget != filepath.Join(d.SourceRoot, e.Source, n.Path) {
					return ErrState
				}
			} else if fp.LinkTarget != "" || d.Mode == "link" && fp.Identity != n.Node.Identity || (d.Mode == "copy" || d.Mode == "move") && fp.Identity == n.Node.Identity {
				return ErrState
			}
		}
	}
	return nil
}
func discDirectories(root *os.File, nodes []DiscNode) (map[string]string, error) {
	result := map[string]string{}
	info, err := root.Stat()
	if err != nil {
		return nil, err
	}
	result["."] = Identity(info)
	for _, n := range nodes {
		if !n.Directory {
			continue
		}
		p, leaf, err := anchoredParent(root, n.Path, false)
		if err != nil {
			return nil, err
		}
		err = unix.Mkdirat(int(p.Fd()), leaf, 0755)
		if err != nil {
			p.Close()
			return nil, err
		}
		node, kind, err := libraryNode(p, leaf)
		p.Close()
		if err != nil || kind != unix.S_IFDIR {
			return nil, ErrState
		}
		result[n.Path] = node.Identity
	}
	return result, nil
}
func discSyncDirs(root *os.File, dirs map[string]string) error {
	for name, id := range dirs {
		f, err := discRoot(root, name)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil || Identity(info) != id {
			f.Close()
			return ErrState
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func transferDisc(ctx context.Context, d Definition, e Entry, temp string, staging, prepared func(Proof) error) (Proof, error) {
	p := Proof{Temp: temp, Kind: "directory", Anchor: true, Incomplete: true}
	if d.Mode == "move" || !SupportedMode(d.Mode) || prepared == nil || !strings.HasPrefix(temp, ".nastool-copy-") || filepath.Base(temp) != temp {
		return p, ErrState
	}
	m, err := discManifest(d, e)
	if err != nil {
		return p, err
	}
	if err := ValidateMode(ctx, d, e); err != nil {
		return p, err
	}
	for _, n := range m.Nodes {
		if !n.Directory && time.Since(time.Unix(0, n.Node.Modified)) < 30*time.Second {
			return p, ErrState
		}
	}
	if _, err := discSourceStamp(ctx, d, e, nil, false); err != nil {
		return p, err
	}
	source, err := discSource(ctx, d, e)
	if err != nil {
		return p, err
	}
	defer source.Close()
	hashes, err := discHashes(ctx, source, m)
	if err != nil {
		return p, err
	}
	parent, leaf, err := discTargetParent(d, e, nil, true)
	if err != nil {
		return p, err
	}
	defer parent.Close()
	pi, err := parent.Stat()
	if err != nil {
		return p, err
	}
	p.ParentIdentity = Identity(pi)
	if _, err := lstatAt(parent, leaf); !errors.Is(err, unix.ENOENT) {
		return p, ErrClaimed
	}
	if err := unix.Mkdirat(int(parent.Fd()), temp, 0700); err != nil {
		return p, err
	}
	stage, err := discDir(parent, temp)
	if err != nil {
		return p, err
	}
	defer stage.Close()
	si, err := stage.Stat()
	if err != nil {
		return p, err
	}
	p.StagingIdentity = Identity(si)
	owned, err := discPrivate(parent, temp, p.StagingIdentity)
	if err != nil {
		return p, err
	}
	owned.Close()
	for _, name := range []string{"payload", "witness"} {
		if err := unix.Mkdirat(int(stage.Fd()), name, 0755); err != nil {
			return p, err
		}
	}
	payload, err := discDir(stage, "payload")
	if err != nil {
		return p, err
	}
	defer payload.Close()
	witness, err := discDir(stage, "witness")
	if err != nil {
		return p, err
	}
	defer witness.Close()
	pd, err := discDirectories(payload, m.Nodes)
	if err != nil {
		return p, err
	}
	wd, err := discDirectories(witness, m.Nodes)
	if err != nil {
		return p, err
	}
	p.Identity = pd["."]
	tree := &DiscProof{Directories: pd, WitnessDirectories: wd, Files: []DiscFileProof{}}
	linked := map[string]uint64{}
	for _, n := range m.Nodes {
		if n.Directory {
			continue
		}
		if ctx.Err() != nil {
			return p, ctx.Err()
		}
		dp, dl, err := anchoredParent(payload, n.Path, false)
		if err != nil {
			return p, err
		}
		wp, wl, err := anchoredParent(witness, n.Path, false)
		if err != nil {
			dp.Close()
			return p, err
		}
		link := ""
		switch d.Mode {
		case "copy":
			fd, e := unix.Openat(int(dp.Fd()), dl, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
			err = e
			if fd >= 0 {
				unix.Close(fd)
			}
		case "link":
			sp, sl, e := anchoredParent(source, n.Path, false)
			if e == nil {
				node, _, e := libraryNode(sp, sl)
				if e != nil || node.Identity != n.Node.Identity || node.Parent != n.Node.Parent || node.Size != n.Node.Size || node.Modified != n.Node.Modified || node.Links != n.Node.Links+2*linked[n.Node.Identity] {
					err = ErrState
				} else {
					err = unix.Linkat(int(sp.Fd()), sl, int(dp.Fd()), dl, 0)
				}
				sp.Close()
			} else {
				err = e
			}
		case "softlink":
			link = filepath.Join(d.SourceRoot, e.Source, n.Path)
			err = unix.Symlinkat(link, int(dp.Fd()), dl)
		}
		if err == nil {
			err = unix.Linkat(int(dp.Fd()), dl, int(wp.Fd()), wl, 0)
		}
		if err == nil && d.Mode != "softlink" {
			file, syncErr := openRegular(dp, dl)
			if syncErr == nil {
				syncErr = file.Sync()
				file.Close()
			}
			err = syncErr
		}
		if d.Mode == "link" && err == nil {
			linked[n.Node.Identity]++
		}
		node, _, statErr := libraryNode(dp, dl)
		dp.Close()
		wp.Close()
		if err != nil {
			return p, modeOperationError(err)
		}
		if statErr != nil {
			return p, statErr
		}
		tree.Files = append(tree.Files, DiscFileProof{Path: n.Path, Identity: node.Identity, Digest: hashes[n.Path], LinkTarget: link})
	}
	p.Disc, p.Digest = tree, Digest(tree)
	if err := discSyncDirs(payload, pd); err != nil {
		return p, err
	}
	if err := discSyncDirs(witness, wd); err != nil {
		return p, err
	}
	if err := stage.Sync(); err != nil {
		return p, err
	}
	if err := parent.Sync(); err != nil {
		return p, err
	}
	if staging != nil {
		if err := staging(p); err != nil {
			return p, err
		}
	}
	if d.Mode == "copy" {
		files := discFiles(p)
		for _, n := range m.Nodes {
			if n.Directory {
				continue
			}
			if err := discCopy(ctx, source, payload, n, files[n.Path]); err != nil {
				return p, err
			}
		}
	}
	p.Incomplete = false
	stamp, err := discSourceStamp(ctx, d, e, &p, true)
	if err != nil {
		return p, err
	}
	if err := discVerifyTree(ctx, d, e, p, payload, false); err != nil {
		return p, err
	}
	if err := discVerifyWitness(ctx, d, e, p, witness, false); err != nil {
		return p, err
	}
	if err := prepared(p); err != nil {
		return p, err
	}
	after, err := discSourceStamp(ctx, d, e, &p, false)
	if err != nil || stamp != after {
		return p, ErrState
	}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	current, _, err := discTargetParent(d, e, &p, false)
	if err != nil {
		return p, err
	}
	current.Close()
	check, err := discPrivate(parent, temp, p.StagingIdentity)
	if err != nil {
		return p, err
	}
	check.Close()
	if err := renameNoReplace(int(stage.Fd()), "payload", int(parent.Fd()), leaf); err != nil {
		return p, modeOperationError(err)
	}
	if err := stage.Sync(); err != nil {
		return p, err
	}
	if err := parent.Sync(); err != nil {
		return p, err
	}
	return p, nil
}
func discCopy(ctx context.Context, source, payload *os.File, n DiscNode, fp DiscFileProof) error {
	sp, sn, err := anchoredParent(source, n.Path, false)
	if err != nil {
		return err
	}
	defer sp.Close()
	before, _, err := libraryNode(sp, sn)
	if err != nil || before != n.Node {
		return ErrState
	}
	src, err := openRegular(sp, sn)
	if err != nil {
		return err
	}
	defer src.Close()
	sourceInfo, err := src.Stat()
	if err != nil || Identity(sourceInfo) != n.Node.Identity {
		return ErrState
	}
	dp, dn, err := anchoredParent(payload, n.Path, false)
	if err != nil {
		return err
	}
	defer dp.Close()
	fd, err := unix.Openat(int(dp.Fd()), dn, unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	dst := os.NewFile(uintptr(fd), dn)
	defer dst.Close()
	info, err := dst.Stat()
	if err != nil || !info.Mode().IsRegular() || Identity(info) != fp.Identity || info.Size() != 0 {
		return ErrState
	}
	h := sha256.New()
	size, err := io.CopyBuffer(io.MultiWriter(dst, h), io.LimitReader(contextReader{ctx: ctx, reader: src}, n.Node.Size+1), make([]byte, 256<<10))
	if err != nil {
		return err
	}
	if size != n.Node.Size || hex.EncodeToString(h.Sum(nil)) != fp.Digest {
		return ErrState
	}
	if err := dst.Chmod(sourceInfo.Mode().Perm() & 0777); err != nil {
		return err
	}
	if err := unix.Futimes(int(dst.Fd()), []unix.Timeval{unix.NsecToTimeval(time.Now().UnixNano()), unix.NsecToTimeval(n.Node.Modified)}); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	after, _, err := libraryNode(sp, sn)
	if err != nil || before != after {
		return ErrState
	}
	return nil
}
