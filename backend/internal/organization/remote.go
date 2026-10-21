package organization

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func RemoteMode(mode string) bool { return mode == "rclonecopy" || mode == "rclone" }
func MoveMode(mode string) bool   { return mode == "move" || mode == "rclone" }

type RemoteApproval struct {
	Origin                  string `json:"origin,omitempty"`
	Generation              string `json:"generation,omitempty"`
	Actor, Created          string
	Exclusive, RecoveryCopy bool
}

func (a RemoteApproval) Valid() bool {
	_, err := time.Parse(time.RFC3339Nano, a.Created)
	if a.Actor == "" || !a.Exclusive || !a.RecoveryCopy || err != nil {
		return false
	}
	if a.Origin == "sync-monitor" || a.Origin == "download-monitor" {
		_, err := hex.DecodeString(a.Generation)
		return err == nil && len(a.Generation) == 32
	}
	return a.Generation == ""
}

type RemoteFile struct {
	Path   string
	Size   int64
	Digest string
	Source LibraryNode
}
type RemoteProof struct {
	Operation, Stage, Manifest                    string
	Files                                         []RemoteFile
	Directories                                   []string
	Approval                                      RemoteApproval
	StageReady, PublicationIntent, TargetVerified bool
}
type remoteManifest struct {
	Operation, Target, Mode string
	Files                   []RemoteFile
	Directories             []string
}

func remoteManifestBytes(e Entry, mode string, p RemoteProof) []byte {
	b, _ := json.Marshal(remoteManifest{p.Operation, e.Target, mode, p.Files, p.Directories})
	return b
}
func remoteFileTarget(e Entry, f RemoteFile) string {
	if e.Disc == nil {
		return e.Target
	}
	return path.Join(e.Target, f.Path)
}
func RemoteTargetIdentity(root string) string {
	return Digest([]string{"rclone:NASTOOL", path.Clean(root)})
}
func remoteProfileValid(d Definition) bool {
	return RemoteMode(d.Mode) && d.Remote != nil && d.Remote.Driver == "rclone" && d.Remote.Protocol == 1 && d.Remote.ConfigFile != "" && len(d.Remote.ConfigDigest) == 64 && path.IsAbs(d.TargetRoot) && path.Clean(d.TargetRoot) == d.TargetRoot && d.TargetRoot != "/" && !strings.ContainsAny(d.TargetRoot, "\\\x00\r\n") && d.TargetIdentity == RemoteTargetIdentity(d.TargetRoot)
}
func remoteProofValid(d Definition, e Entry, p Proof) error {
	if !remoteProfileValid(d) || p.Remote == nil || p.Remote.Operation == "" || len(p.Remote.Operation) != 32 || p.Remote.Stage != path.Join(".nastool-remote", p.Remote.Operation) || !p.Remote.Approval.Valid() || p.Remote.Manifest == "" || p.LinkTarget != "" || !p.Anchor || p.Identity != "remote:"+p.Remote.Operation || p.ParentIdentity != d.TargetIdentity || p.StagingIdentity != p.Remote.Manifest || len(p.Remote.Files) == 0 || len(p.Remote.Files)+len(p.Remote.Directories) > 20000 {
		return ErrState
	}
	if p.Remote.Manifest != Digest(json.RawMessage(remoteManifestBytes(e, d.Mode, *p.Remote))) {
		return ErrState
	}
	names := map[string]bool{}
	nameKey := func(s string) string { return cases.Fold().String(norm.NFD.String(s)) }
	if _, err := hex.DecodeString(p.Remote.Operation); err != nil {
		return ErrState
	}
	for _, f := range p.Remote.Files {
		if f.Path != "." && !validRelative(f.Path) || f.Size < 0 || f.Size > 1<<50 || len(f.Digest) != 64 || f.Source.Size != f.Size || f.Source.Identity == "" || names[nameKey(f.Path)] || strings.Contains(f.Path, ".rclonelink") {
			return ErrState
		}
		if _, err := hex.DecodeString(f.Digest); err != nil {
			return ErrState
		}
		names[nameKey(f.Path)] = true
	}
	dirs := map[string]bool{}
	for _, dir := range p.Remote.Directories {
		if dir != "." && !validRelative(dir) || names[nameKey(dir)] || strings.Contains(dir, ".rclonelink") {
			return ErrState
		}
		dirs[dir] = true
		names[nameKey(dir)] = true
	}
	if e.Disc == nil {
		if len(p.Remote.Files) != 1 || p.Remote.Files[0].Path != "." || p.Remote.Files[0].Source.Identity != e.Identity || p.Remote.Files[0].Size != e.Size || len(p.Remote.Directories) != 0 || p.Digest != p.Remote.Files[0].Digest || p.Kind != "remote" {
			return ErrState
		}
	} else if p.Disc == nil || p.Digest != Digest(p.Disc) || len(p.Disc.Files) != len(p.Remote.Files) || p.Kind != "directory" {
		return ErrState
	} else {
		m, err := discManifestDefinition(d, e)
		if err != nil || !dirs["."] || len(dirs) != len(p.Disc.Directories) {
			return ErrState
		}
		files := map[string]RemoteFile{}
		for _, f := range p.Remote.Files {
			files[f.Path] = f
		}
		for _, node := range m.Nodes {
			if node.Directory {
				if !dirs[node.Path] {
					return ErrState
				}
			} else {
				f, ok := files[node.Path]
				if !ok || f.Source != node.Node {
					return ErrState
				}
			}
		}
		if len(m.Nodes)+1 != len(dirs)+len(files) {
			return ErrState
		}
		for _, f := range p.Disc.Files {
			if want, ok := files[f.Path]; !ok || f.Digest != want.Digest {
				return ErrState
			}
		}
	}
	return nil
}
func RemoteTargetStatus(ctx context.Context, c *remotestore.Client, root, relative string) (string, error) {
	n, err := c.Entry(ctx, root, relative)
	if err != nil {
		return "", err
	}
	if n == nil {
		return "available", nil
	}
	return "conflict", nil
}

type hashCounter struct {
	h     io.Writer
	count int64
}
type remoteContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r remoteContextReader) Read(b []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	return r.r.Read(b)
}

func (w *hashCounter) Write(p []byte) (int, error) {
	n, err := w.h.Write(p)
	w.count += int64(n)
	return n, err
}
func hashRemoteReader(ctx context.Context, r io.Reader, size int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(remoteContextReader{ctx: ctx, r: r}, size+1))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", ErrState
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func remoteHash(ctx context.Context, c *remotestore.Client, location string, size int64, want string) error {
	h := sha256.New()
	w := &hashCounter{h: h}
	if err := c.Read(ctx, location, size, w); err != nil {
		return err
	}
	if w.count != size || hex.EncodeToString(h.Sum(nil)) != want {
		return ErrState
	}
	return nil
}
func verifyRemoteManifest(ctx context.Context, c *remotestore.Client, d Definition, e Entry, p Proof) error {
	if err := remoteProofValid(d, e, p); err != nil {
		return err
	}
	b := remoteManifestBytes(e, d.Mode, *p.Remote)
	if err := remoteHash(ctx, c, path.Join(d.TargetRoot, p.Remote.Stage, "manifest.json"), int64(len(b)), p.Remote.Manifest); err != nil {
		return err
	}
	nodes, err := c.List(ctx, path.Join(d.TargetRoot, p.Remote.Stage), true)
	if err != nil {
		return err
	}
	wanted := map[string]bool{"manifest.json": true, "files": true}
	for i := range p.Remote.Files {
		wanted["files/"+strconv.Itoa(i)] = true
	}
	found := map[string]bool{}
	for _, n := range nodes {
		if !wanted[n.Path] || n.IsDir && n.Path != "files" || !n.IsDir && n.Path == "files" {
			return ErrState
		}
		found[n.Path] = true
	}
	if !found["manifest.json"] {
		return ErrState
	}
	for i, f := range p.Remote.Files {
		rel := "files/" + strconv.Itoa(i)
		if !found[rel] {
			return ErrState
		}
		if err := remoteHash(ctx, c, path.Join(d.TargetRoot, p.Remote.Stage, rel), f.Size, f.Digest); err != nil {
			return err
		}
	}
	return nil
}

// VerifyRemoteTarget is full-content verification under the explicitly
// approved exclusive-namespace contract. It is not a claim of inode identity,
// cross-provider atomic publication or automatic rollback capability.
func VerifyRemoteTarget(ctx context.Context, c *remotestore.Client, d Definition, e Entry, p Proof, metadata bool) error {
	if err := remoteProofValid(d, e, p); err != nil {
		return err
	}
	if p.Incomplete || !p.Remote.StageReady || !p.Remote.PublicationIntent {
		return ErrState
	}
	if e.Disc != nil {
		nodes, err := c.List(ctx, path.Join(d.TargetRoot, e.Target), true)
		if err != nil {
			return err
		}
		files := map[string]RemoteFile{}
		dirs := map[string]bool{}
		for _, f := range p.Remote.Files {
			files[f.Path] = f
		}
		for _, dir := range p.Remote.Directories {
			dirs[dir] = true
		}
		found := map[string]bool{}
		for _, n := range nodes {
			if n.IsDir {
				if !dirs[n.Path] {
					return ErrState
				}
				continue
			}
			f, ok := files[n.Path]
			if !ok {
				if metadata && discMetadataName(path.Base(e.Target), n.Path) && n.Size <= 16<<20 {
					continue
				}
				return ErrState
			}
			if n.Size != f.Size {
				return ErrState
			}
			found[n.Path] = true
		}
		if len(found) != len(files) {
			return ErrState
		}
		if c.Binding().Directories {
			for _, dir := range p.Remote.Directories {
				if dir == "." {
					continue
				}
				n, err := c.Entry(ctx, path.Join(d.TargetRoot, e.Target), dir)
				if err != nil || n == nil || !n.IsDir {
					return ErrState
				}
			}
		}
	}
	for _, f := range p.Remote.Files {
		rel := remoteFileTarget(e, f)
		n, err := c.Entry(ctx, d.TargetRoot, rel)
		if err != nil || n == nil || n.IsDir || n.Size != f.Size {
			return ErrState
		}
		if err := remoteHash(ctx, c, path.Join(d.TargetRoot, rel), f.Size, f.Digest); err != nil {
			return err
		}
		after, err := c.Entry(ctx, d.TargetRoot, rel)
		if err != nil || after == nil || *after != *n {
			return ErrState
		}
	}
	return c.Current()
}
func BuildRemoteProof(ctx context.Context, d Definition, e Entry, temp string, a RemoteApproval) (Proof, error) {
	if !remoteProfileValid(d) || !a.Valid() {
		return Proof{}, ErrState
	}
	files, dirs, err := remoteSources(ctx, d, e)
	if err != nil {
		return Proof{}, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Proof{}, err
	}
	r := &RemoteProof{Operation: hex.EncodeToString(token[:]), Files: files, Directories: dirs, Approval: a}
	r.Stage = path.Join(".nastool-remote", r.Operation)
	r.Manifest = Digest(json.RawMessage(remoteManifestBytes(e, d.Mode, *r)))
	p := Proof{Remote: r, Incomplete: true, Anchor: true, Temp: temp, Identity: "remote:" + r.Operation, ParentIdentity: d.TargetIdentity, StagingIdentity: r.Manifest, Kind: "remote"}
	if e.Disc == nil {
		p.Digest = files[0].Digest
	} else {
		p.Kind = "directory"
		p.Disc = &DiscProof{Directories: map[string]string{}, WitnessDirectories: map[string]string{}, Files: []DiscFileProof{}}
		for _, dir := range dirs {
			p.Disc.Directories[dir] = "remote-dir:" + Digest(dir)
			p.Disc.WitnessDirectories[dir] = "remote-evidence:" + r.Operation + ":" + Digest(dir)
		}
		for _, f := range files {
			p.Disc.Files = append(p.Disc.Files, DiscFileProof{Path: f.Path, Identity: "remote-file:" + Digest(f.Path), Digest: f.Digest})
		}
		p.Digest = Digest(p.Disc)
	}
	return p, remoteProofValid(d, e, p)
}

// UploadRemoteStage is invoked only after the initial immutable source hashes,
// operation namespace and approval are durably staged. It never uploads a
// mutable local pathname through a child process or writes a final target.
func UploadRemoteStage(ctx context.Context, c *remotestore.Client, d Definition, e Entry, p Proof, current func() error) error {
	if current == nil || !p.Incomplete || remoteProofValid(d, e, p) != nil {
		return ErrState
	}
	if n, err := c.Entry(ctx, d.TargetRoot, p.Remote.Stage); err != nil || n != nil {
		return ErrState
	}
	if err := current(); err != nil {
		return err
	}
	b := remoteManifestBytes(e, d.Mode, *p.Remote)
	if err := c.Upload(ctx, path.Join(d.TargetRoot, p.Remote.Stage, "manifest.json"), int64(len(b)), bytes.NewReader(b)); err != nil {
		return err
	}
	for i, f := range p.Remote.Files {
		if err := current(); err != nil {
			return err
		}
		if err := readRemoteSource(ctx, d, e, f, func(file io.Reader) error {
			return c.Upload(ctx, path.Join(d.TargetRoot, p.Remote.Stage, "files", strconv.Itoa(i)), f.Size, file)
		}); err != nil {
			return err
		}
	}
	return verifyRemoteManifest(ctx, c, d, e, p)
}

// PublishRemote is explicit continuation as well as initial publication. Only
// missing files are copied. Already present files must match the entire reviewed
// content; their creation identity cannot be inferred on generic remote APIs.
func PublishRemote(ctx context.Context, c *remotestore.Client, d Definition, e Entry, p Proof, current func() error) error {
	if current == nil || p.Incomplete || p.Remote == nil || !p.Remote.StageReady || !p.Remote.PublicationIntent {
		return ErrState
	}
	if err := verifyRemoteManifest(ctx, c, d, e, p); err != nil {
		return err
	}
	if e.Disc != nil {
		n, err := c.Entry(ctx, d.TargetRoot, e.Target)
		if err != nil {
			return err
		}
		if n != nil {
			if !n.IsDir {
				return ErrState
			}
			nodes, err := c.List(ctx, path.Join(d.TargetRoot, e.Target), true)
			if err != nil {
				return err
			}
			allowed := map[string]bool{}
			for _, f := range p.Remote.Files {
				allowed[f.Path] = true
			}
			for _, dir := range p.Remote.Directories {
				allowed[dir] = true
			}
			for _, node := range nodes {
				if !allowed[node.Path] {
					return ErrState
				}
			}
		}
		for _, dir := range p.Remote.Directories {
			if err := current(); err != nil {
				return err
			}
			if err := c.Mkdir(ctx, path.Join(d.TargetRoot, e.Target, dir)); err != nil {
				return err
			}
		}
	}
	for i, f := range p.Remote.Files {
		if err := current(); err != nil {
			return err
		}
		rel := remoteFileTarget(e, f)
		n, err := c.Entry(ctx, d.TargetRoot, rel)
		if err != nil {
			return err
		}
		if n == nil {
			if err := current(); err != nil {
				return err
			}
			if err := c.Copy(ctx, path.Join(d.TargetRoot, p.Remote.Stage, "files", strconv.Itoa(i)), path.Join(d.TargetRoot, rel)); err != nil {
				return err
			}
		} else if n.IsDir || n.Size != f.Size {
			return ErrState
		}
		if err := remoteHash(ctx, c, path.Join(d.TargetRoot, rel), f.Size, f.Digest); err != nil {
			return err
		}
	}
	return VerifyRemoteTarget(ctx, c, d, e, p, false)
}

type remoteVerificationKey struct{}
type remoteVerification struct {
	client  *remotestore.Client
	binding string
	current func() error
}

func RemoteContext(ctx context.Context, c *remotestore.Client, d Definition) context.Context {
	return context.WithValue(ctx, remoteVerificationKey{}, remoteVerification{client: c, binding: Digest(d.Remote)})
}
func RemoteOperationContext(ctx context.Context, current func() error) context.Context {
	g, ok := ctx.Value(remoteVerificationKey{}).(remoteVerification)
	if !ok {
		return ctx
	}
	g.current = current
	return context.WithValue(ctx, remoteVerificationKey{}, g)
}
func remoteClient(ctx context.Context, d Definition) (*remotestore.Client, error) {
	g, ok := ctx.Value(remoteVerificationKey{}).(remoteVerification)
	if !ok || g.client == nil || d.Remote == nil || g.binding != Digest(d.Remote) || g.client.Binding() != *d.Remote {
		return nil, ErrMode
	}
	if g.current != nil {
		if err := g.current(); err != nil {
			return nil, err
		}
	}
	return g.client, g.client.Current()
}
func verifyRemoteMove(ctx context.Context, d Definition, e Entry, p Proof) error {
	c, err := remoteClient(ctx, d)
	if err != nil {
		return err
	}
	if p.Remote == nil || !p.Remote.TargetVerified {
		return ErrState
	}
	if err := verifyRemoteManifest(ctx, c, d, e, p); err != nil {
		return err
	}
	return VerifyRemoteTarget(ctx, c, d, e, p, true)
}

// VerifyRemotePublication never writes or infers source removal. The recovery
// payload must remain intact as well as the complete final content.
func VerifyRemotePublication(ctx context.Context, c *remotestore.Client, d Definition, e Entry, p Proof) error {
	if err := verifyRemoteManifest(ctx, c, d, e, p); err != nil {
		return err
	}
	return VerifyRemoteTarget(ctx, c, d, e, p, false)
}
