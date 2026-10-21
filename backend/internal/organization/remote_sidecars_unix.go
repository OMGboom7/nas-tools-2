//go:build linux || darwin

package organization

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

// The parent intake approval also covers missing NFO/artwork publication under
// the same exclusive-namespace contract. Existing objects are always preserved;
// a writing intent is never automatically replayed or force-overwritten.
func publishRemoteSidecar(ctx context.Context, job Job, item JobItem, scope, name string, data []byte, prepare func(SidecarProof) error, current func() error) (p SidecarProof, state string, err error) {
	state = "blocked"
	if job.State != "completed" || item.State != "completed" || item.Reason != "" || len(data) == 0 || len(data) > 16<<20 || prepare == nil || current == nil || item.Proof.Remote == nil || !item.Proof.Remote.Approval.Valid() {
		return p, state, ErrState
	}
	c, err := remoteClient(ctx, job.Definition)
	if err != nil {
		return p, state, err
	}
	target, err := sidecarTarget(item, scope, name)
	if err != nil {
		return p, state, err
	}
	if err := VerifyCompletedTarget(ctx, job, item); err != nil {
		return p, state, err
	}
	preserved := func() (bool, error) {
		if item.MediaType == "电影" && strings.HasSuffix(name, ".nfo") {
			exists, err := c.Existing(ctx, job.TargetRoot, path.Join(path.Dir(target), "movie.nfo"))
			if err != nil || exists {
				return exists, err
			}
		}
		return c.Existing(ctx, job.TargetRoot, target)
	}
	if exists, err := preserved(); err != nil || exists {
		if exists && err == nil {
			state = "preserved"
		}
		return p, state, err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return p, state, err
	}
	nonce := hex.EncodeToString(token[:])
	h := sha256.Sum256(data)
	p = SidecarProof{Target: target, Temp: path.Join(".nastool-remote-sidecars", nonce), Identity: "remote:" + nonce, ParentIdentity: job.Definition.TargetIdentity, StagingIdentity: "remote:" + nonce, Digest: hex.EncodeToString(h[:]), Size: int64(len(data))}
	if err := current(); err != nil {
		return p, state, err
	}
	if err := prepare(p); err != nil {
		return p, state, err
	}
	stage := path.Join(job.TargetRoot, p.Temp, "payload")
	if n, err := c.Entry(ctx, job.TargetRoot, p.Temp); err != nil || n != nil {
		return p, state, ErrState
	}
	if err := current(); err != nil {
		return p, state, err
	}
	if err := c.Upload(ctx, stage, p.Size, bytes.NewReader(data)); err != nil {
		return p, "uncertain", err
	}
	if err := remoteHash(ctx, c, stage, p.Size, p.Digest); err != nil {
		return p, "uncertain", err
	}
	if err := current(); err != nil {
		return p, state, err
	}
	if err := VerifyCompletedTarget(ctx, job, item); err != nil {
		return p, state, err
	}
	if exists, err := preserved(); err != nil || exists {
		if exists && err == nil {
			state = "preserved"
		}
		return p, state, err
	}
	if err := c.Copy(ctx, stage, path.Join(job.TargetRoot, target)); err != nil {
		return p, "uncertain", err
	}
	if err := remoteHash(ctx, c, path.Join(job.TargetRoot, target), p.Size, p.Digest); err != nil {
		return p, "uncertain", err
	}
	if err := current(); err != nil {
		return p, "uncertain", err
	}
	if err := VerifyCompletedTarget(ctx, job, item); err != nil {
		return p, "uncertain", err
	}
	return p, "created", nil
}
