package organization

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// Monitor leases contain only opaque account/executable/environment digests
// and public capabilities, never private Binding/configuration paths.
type RemoteProfileSeal struct {
	Driver      string `json:"driver,omitempty"`
	Digest      string `json:"digest"`
	Directories bool   `json:"directories"`
}
type RemoteAutomation struct {
	Protocol     int                          `json:"protocol"`
	Profiles     map[string]RemoteProfileSeal `json:"profiles"`
	Exclusive    bool                         `json:"exclusive"`
	RecoveryCopy bool                         `json:"recoveryCopy"`
	ApprovedAt   string                       `json:"approvedAt,omitempty"`
}

func (a *RemoteAutomation) Valid() bool {
	if a == nil || a.Protocol != 1 || len(a.Profiles) == 0 || len(a.Profiles) > 128 || !a.Exclusive || !a.RecoveryCopy {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, a.ApprovedAt); err != nil {
		return false
	}
	for id, p := range a.Profiles {
		if id == "" || len(p.Digest) != 64 || p.Driver != "" && p.Driver != "minio" {
			return false
		}
		if _, err := hex.DecodeString(p.Digest); err != nil {
			return false
		}
	}
	return true
}
func (a *RemoteAutomation) Allows(d Definition) bool {
	if !a.Valid() || !RemoteMode(d.Mode) || d.Remote == nil || d.Remote.Driver != RemoteDriver(d.Mode) {
		return false
	}
	p, ok := a.Profiles[RemoteProfileKey(d.TargetID, d.Remote.Driver)]
	driver := p.Driver
	if driver == "" {
		driver = "rclone"
	}
	return ok && driver == d.Remote.Driver && p.Digest == Digest(d.Remote) && p.Directories == d.Remote.Directories
}

// Existing Rclone lease keys and JSON stay unchanged. MinIO is a distinct
// authorization even when a configured logical root has the same spelling.
func RemoteProfileKey(targetID, driver string) string {
	if driver == "minio" {
		return "minio/" + targetID
	}
	return targetID
}
func (a *RemoteAutomation) Approval(actor, origin, generation string) RemoteApproval {
	if !a.Valid() || actor == "" || generation == "" {
		return RemoteApproval{}
	}
	return RemoteApproval{Actor: "monitor:" + actor, Created: a.ApprovedAt, Exclusive: true, RecoveryCopy: true, Origin: origin, Generation: generation}
}

type automaticRemoteKey struct{}
type automaticRemoteExecution struct{ Origin, Generation string }

func AutomaticRemoteContext(ctx context.Context, origin, generation string) context.Context {
	return context.WithValue(ctx, automaticRemoteKey{}, automaticRemoteExecution{origin, generation})
}

// Manual recovery supplies fresh independent consent. It has no automatic
// context and never re-enables or inherits an old monitor lease.
func verifyRemoteExecution(ctx context.Context, r unknownReader, d Definition, p *Proof) error {
	g, automatic := ctx.Value(automaticRemoteKey{}).(automaticRemoteExecution)
	if d.Remote == nil || !automatic {
		return nil
	}
	var grant *RemoteAutomation
	var actor string
	switch g.Origin {
	case "sync-monitor":
		c, err := readMonitorControl(ctx, r)
		if err != nil {
			return err
		}
		if c.State != "enabled" || c.Lease.Generation != g.Generation || d.Sync == nil || d.Sync.MonitorGeneration != g.Generation {
			return ErrState
		}
		selected := false
		for _, id := range c.Lease.IDs {
			selected = selected || id == strconv.FormatInt(d.Sync.Record.ID, 10)
		}
		if !selected || d.Sync.Actor != "monitor:"+c.Lease.Actor || MoveMode(d.Mode) && !c.Lease.SourceRemoval {
			return ErrState
		}
		grant, actor = c.Lease.Remote, c.Lease.Actor
	case "download-monitor":
		c, err := readDownloadMonitor(ctx, r)
		if err != nil {
			return err
		}
		if c.State != "enabled" || c.Lease.Generation != g.Generation || d.Download == nil || d.Download.MonitorGeneration != g.Generation {
			return ErrState
		}
		selected := false
		for _, id := range c.Lease.IDs {
			selected = selected || id == d.Download.DownloaderID
		}
		if !selected || d.Download.Actor != "monitor:"+c.Lease.Actor || c.Lease.Downloaders[d.Download.DownloaderID] != d.Download.ConfigDigest || MoveMode(d.Mode) && !c.Lease.SourceRemoval {
			return ErrState
		}
		grant, actor = c.Lease.Remote, c.Lease.Actor
	default:
		return ErrState
	}
	if !grant.Allows(d) {
		return ErrState
	}
	if p != nil && (p.Remote == nil || p.Remote.Approval != grant.Approval(actor, g.Origin, g.Generation)) {
		return ErrState
	}
	return nil
}
func (s *Store) VerifyRemoteExecution(ctx context.Context, d Definition) error {
	return verifyRemoteExecution(ctx, s.db, d, nil)
}
func readExecutionDefinition(ctx context.Context, r unknownReader, id string) (Definition, error) {
	var raw string
	var d Definition
	if err := r.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=? AND STATE='active'`, id).Scan(&raw); err != nil {
		return d, err
	}
	if json.Unmarshal([]byte(raw), &d) != nil {
		return d, ErrState
	}
	return d, nil
}
func (s *Store) verifyRemoteJobExecution(ctx context.Context, r unknownReader, id string, p *Proof) error {
	if _, ok := ctx.Value(automaticRemoteKey{}).(automaticRemoteExecution); !ok {
		return nil
	}
	d, err := readExecutionDefinition(ctx, r, id)
	if err != nil {
		return err
	}
	return verifyRemoteExecution(ctx, r, d, p)
}
