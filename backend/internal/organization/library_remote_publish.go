package organization

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func libraryRemoteParent(root, target string) string {
	parent := cases.Fold().String(norm.NFD.String(path.Dir(path.Join(root, target))))
	return Digest([]string{"remote-library:NASTOOL", parent})
}
func libraryRemoteManifest(d LibraryDefinition, p LibraryOutputProof) []byte {
	copy := p
	remote := *p.Remote
	remote.Manifest = ""
	copy.Remote = &remote
	b, _ := json.Marshal(struct {
		Root, Binding string
		Proof         LibraryOutputProof
	}{d.Root.Path, Digest(d.Remote), copy})
	return b
}
func libraryRemoteOutputValid(d LibraryDefinition, p LibraryOutputProof) bool {
	if !libraryRemoteDefinition(d) || p.Remote == nil || p.Remote.Driver != d.Remote.Driver || len(p.Remote.Operation) != 32 || p.Stage != path.Join(".nastool-library-remote", p.Remote.Operation) || p.StageIdentity != "remote-stage:"+p.Remote.Operation || p.NewIdentity != "remote-new:"+p.Remote.Operation || !validRelative(p.Target) || p.Target == "." || p.ParentIdentity != libraryRemoteParent(d.Root.Path, p.Target) || p.NewSize < 1 || p.NewSize > 16<<20 || len(p.NewDigest) != 64 || p.OldLink != "" {
		return false
	}
	if _, err := hex.DecodeString(p.Remote.Operation); err != nil {
		return false
	}
	if _, err := hex.DecodeString(p.NewDigest); err != nil {
		return false
	}
	switch strings.ToLower(path.Ext(p.Target)) {
	case ".nfo", ".jpg", ".jpeg", ".png":
	default:
		return false
	}
	for _, part := range strings.Split(p.Target, "/") {
		if strings.EqualFold(part, "BDMV") || strings.EqualFold(part, "STREAM") || strings.EqualFold(part, "CERTIFICATE") {
			return false
		}
	}
	if p.Remote.Old != nil {
		if p.Remote.Old.IsDir || p.Old.Identity != "remote-old:"+p.Remote.Operation || p.Old.Size != p.Remote.Old.Size || p.Old.Size < 0 || p.Old.Size > 16<<20 || len(p.OldDigest) != 64 {
			return false
		}
		if _, err := hex.DecodeString(p.OldDigest); err != nil {
			return false
		}
	} else if p.Old.Identity != "" || p.OldDigest != "" {
		return false
	}
	return p.Remote.Manifest == Digest(json.RawMessage(libraryRemoteManifest(d, p)))
}
func libraryRemoteStage(ctx context.Context, d LibraryDefinition, p LibraryOutputProof) error {
	if !libraryRemoteOutputValid(d, p) {
		return ErrState
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return err
	}
	nodes, err := c.List(ctx, path.Join(d.Root.Path, p.Stage), true)
	if err != nil {
		return err
	}
	wanted := map[string]bool{"new": true, "manifest.json": true}
	if p.Remote.Old != nil {
		wanted["old"] = true
	}
	if len(nodes) != len(wanted) {
		return ErrState
	}
	for _, n := range nodes {
		if n.IsDir || !wanted[n.Path] {
			return ErrState
		}
		delete(wanted, n.Path)
	}
	if len(wanted) != 0 {
		return ErrState
	}
	manifest := libraryRemoteManifest(d, p)
	if err := remoteHash(ctx, c, path.Join(d.Root.Path, p.Stage, "manifest.json"), int64(len(manifest)), p.Remote.Manifest); err != nil {
		return err
	}
	if err := remoteHash(ctx, c, path.Join(d.Root.Path, p.Stage, "new"), p.NewSize, p.NewDigest); err != nil {
		return err
	}
	if p.Remote.Old != nil {
		return remoteHash(ctx, c, path.Join(d.Root.Path, p.Stage, "old"), p.Old.Size, p.OldDigest)
	}
	return nil
}

func publishLibraryRemoteSidecar(ctx context.Context, d LibraryDefinition, m LibraryMedia, scope, name string, data []byte, prepare func(LibraryOutputProof) error, current func() error) (p LibraryOutputProof, state string, err error) {
	state = "blocked"
	if !LibraryRemoteApproved(d) || !LibraryMode(d.Mode) || len(data) == 0 || len(data) > 16<<20 || prepare == nil || current == nil || verifyLibraryRemoteMedia(ctx, d, m) != nil {
		return p, state, ErrState
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return p, state, err
	}
	p.Target, err = LibraryTarget(d, m, scope, name)
	if err != nil {
		return p, state, err
	}
	overwrite := LibraryOverwrite(d.Mode, name)
	if !overwrite {
		for _, relative := range []string{p.Target} {
			exists, e := c.Existing(ctx, d.Root.Path, relative)
			if e != nil {
				return p, state, e
			}
			if exists {
				return p, "preserved", nil
			}
		}
		if d.Root.Kind == "movie" && strings.HasSuffix(name, ".nfo") {
			exists, e := c.Existing(ctx, d.Root.Path, path.Join(path.Dir(p.Target), "movie.nfo"))
			if e != nil {
				return p, state, e
			}
			if exists {
				return p, "preserved", nil
			}
		}
	}
	old, node, err := libraryRemoteReadMetadata(ctx, c, d, p.Target, 16<<20)
	if err != nil {
		return p, state, err
	}
	if node != nil && !overwrite {
		return p, "preserved", nil
	}
	if node != nil && bytes.Equal(old, data) {
		return p, "preserved", nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return p, state, err
	}
	token := hex.EncodeToString(nonce[:])
	sum := sha256.Sum256(data)
	p.Remote = &LibraryRemoteOutput{Driver: d.Remote.Driver, Operation: token, Old: node}
	p.Stage = path.Join(".nastool-library-remote", token)
	p.StageIdentity = "remote-stage:" + token
	p.NewIdentity = "remote-new:" + token
	p.ParentIdentity = libraryRemoteParent(d.Root.Path, p.Target)
	p.NewDigest = hex.EncodeToString(sum[:])
	p.NewSize = int64(len(data))
	if node != nil {
		sum := sha256.Sum256(old)
		p.Old = LibraryNode{Identity: "remote-old:" + token, Parent: p.ParentIdentity, Size: node.Size}
		p.OldDigest = hex.EncodeToString(sum[:])
	}
	p.Remote.Manifest = Digest(json.RawMessage(libraryRemoteManifest(d, p)))
	if !libraryRemoteOutputValid(d, p) {
		return p, state, ErrState
	}
	if err := current(); err != nil {
		return p, state, err
	}
	if err := prepare(p); err != nil {
		return p, state, err
	}
	if existing, e := c.Entry(ctx, d.Root.Path, p.Stage); e != nil || existing != nil {
		return p, "uncertain", ErrState
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"old", old}, {"new", data}, {"manifest.json", libraryRemoteManifest(d, p)}} {
		if file.name == "old" && node == nil {
			continue
		}
		if err := current(); err != nil {
			return p, "uncertain", err
		}
		if err := c.Upload(ctx, path.Join(d.Root.Path, p.Stage, file.name), int64(len(file.data)), bytes.NewReader(file.data)); err != nil {
			return p, "uncertain", err
		}
	}
	if err := libraryRemoteStage(ctx, d, p); err != nil {
		return p, "uncertain", err
	}
	if err := verifyLibraryRemoteMedia(ctx, d, m); err != nil {
		return p, "uncertain", err
	}
	fresh, now, e := libraryRemoteReadMetadata(ctx, c, d, p.Target, 16<<20)
	if e != nil || node == nil && now != nil || node != nil && (now == nil || *now != *node || !bytes.Equal(fresh, old)) {
		return p, "uncertain", ErrState
	}
	if err := current(); err != nil {
		return p, "uncertain", err
	}
	if err := c.WriteMetadata(ctx, path.Join(d.Root.Path, p.Target), data, node); err != nil {
		return p, "uncertain", err
	}
	if err := remoteHash(ctx, c, path.Join(d.Root.Path, p.Target), p.NewSize, p.NewDigest); err != nil {
		return p, "uncertain", err
	}
	if err := libraryRemoteStage(ctx, d, p); err != nil {
		return p, "uncertain", err
	}
	if err := verifyLibraryRemoteMedia(ctx, d, m); err != nil {
		return p, "uncertain", err
	}
	if err := current(); err != nil {
		return p, "uncertain", err
	}
	if node != nil {
		return p, "replaced", nil
	}
	return p, "created", nil
}

func observeLibraryRemoteSidecar(ctx context.Context, d LibraryDefinition, p LibraryOutputProof) (string, error) {
	if err := libraryRemoteStage(ctx, d, p); err != nil {
		return "uncertain", err
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return "uncertain", err
	}
	data, node, err := libraryRemoteReadMetadata(ctx, c, d, p.Target, 16<<20)
	if err != nil {
		return "uncertain", err
	}
	if node == nil {
		if p.Remote.Old == nil {
			return "not_published", nil
		}
		return "uncertain", ErrState
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if int64(len(data)) == p.NewSize && digest == p.NewDigest {
		if p.Remote.Old != nil {
			return "replaced", nil
		}
		return "created", nil
	}
	if p.Remote.Old != nil && int64(len(data)) == p.Old.Size && digest == p.OldDigest {
		return "not_published", nil
	}
	return "uncertain", ErrState
}

func restoreLibraryRemoteSidecar(ctx context.Context, d LibraryDefinition, p LibraryOutputProof, intent func() error) (string, error) {
	if intent == nil || p.Remote == nil || p.Remote.Old == nil {
		return "uncertain", ErrState
	}
	state, err := observeLibraryRemoteSidecar(ctx, d, p)
	if err != nil || state != "replaced" && state != "not_published" {
		return "uncertain", ErrState
	}
	c, err := libraryRemoteClient(ctx, d)
	if err != nil {
		return "uncertain", err
	}
	_, current, err := libraryRemoteReadMetadata(ctx, c, d, p.Target, 16<<20)
	if err != nil || current == nil {
		return "uncertain", ErrState
	}
	// A previous restore with a lost receipt is positively verified, never
	// replayed. Both old and replacement backups remain immutable in the stage.
	if state == "not_published" {
		if err := intent(); err != nil {
			return "uncertain", err
		}
		return "restored", nil
	}
	data, backup, err := libraryRemoteReadMetadata(ctx, c, d, path.Join(p.Stage, "old"), 16<<20)
	if err != nil || backup == nil {
		return "uncertain", ErrState
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != p.OldDigest || int64(len(data)) != p.Old.Size {
		return "uncertain", ErrState
	}
	if err := intent(); err != nil {
		return "uncertain", err
	}
	if err := libraryRemoteStage(ctx, d, p); err != nil {
		return "uncertain", err
	}
	fresh, node, e := libraryRemoteReadMetadata(ctx, c, d, p.Target, 16<<20)
	sum = sha256.Sum256(fresh)
	if e != nil || node == nil || *node != *current || hex.EncodeToString(sum[:]) != p.NewDigest {
		return "uncertain", ErrState
	}
	if err := c.WriteMetadata(ctx, path.Join(d.Root.Path, p.Target), data, current); err != nil {
		return "uncertain", err
	}
	if err := remoteHash(ctx, c, path.Join(d.Root.Path, p.Target), p.Old.Size, p.OldDigest); err != nil {
		return "uncertain", err
	}
	if err := libraryRemoteStage(ctx, d, p); err != nil {
		return "uncertain", err
	}
	return "restored", nil
}
