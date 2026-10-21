package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func remoteFrozen(p Proof) Proof {
	if p.Remote != nil {
		r := *p.Remote
		p.Remote = &r
		r.StageReady = false
		r.PublicationIntent = false
		r.TargetVerified = false
	}
	p.Incomplete = true
	return p
}
func (s *Store) PrepareRemote(ctx context.Context, id string, index int, p Proof) error {
	if p.Remote == nil || p.Incomplete || !p.Remote.StageReady || p.Remote.PublicationIntent || p.Remote.TargetVerified {
		return ErrState
	}
	return s.remoteTransition(ctx, id, index, "staging", "prepared", p, func(old Proof) bool {
		return old.Incomplete && old.Remote != nil && !old.Remote.StageReady && Digest(old) == Digest(remoteFrozen(p))
	})
}
func (s *Store) BeginRemotePublish(ctx context.Context, id string, index int, p Proof) error {
	if p.Remote == nil || p.Incomplete || !p.Remote.StageReady || !p.Remote.PublicationIntent || p.Remote.TargetVerified {
		return ErrState
	}
	return s.remoteTransition(ctx, id, index, "prepared", "publishing", p, func(old Proof) bool {
		if old.Remote == nil || old.Remote.PublicationIntent || old.Remote.TargetVerified {
			return false
		}
		r := *p.Remote
		expected := p
		expected.Remote = &r
		r.PublicationIntent = false
		return Digest(old) == Digest(expected)
	})
}
func (s *Store) FinishRemotePublish(ctx context.Context, id string, index int, p Proof) error {
	if p.Remote == nil || p.Incomplete || !p.Remote.StageReady || !p.Remote.PublicationIntent || !p.Remote.TargetVerified {
		return ErrState
	}
	return s.remoteTransition(ctx, id, index, "publishing", "prepared", p, func(old Proof) bool {
		r := *p.Remote
		expected := p
		expected.Remote = &r
		r.TargetVerified = false
		return Digest(old) == Digest(expected)
	})
}
func (s *Store) remoteTransition(ctx context.Context, id string, index int, from, to string, p Proof, match func(Proof) bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw, old, stage string
	if err := tx.QueryRowContext(ctx, `SELECT J.DEFINITION,I.STATE,I.PROOF FROM GO_ORGANIZATION_JOBS J JOIN GO_ORGANIZATION_ITEMS I ON I.JOB_ID=J.ID WHERE J.ID=? AND J.STATE='active' AND I.ORDINAL=?`, id, index).Scan(&raw, &stage, &old); err != nil {
		return err
	}
	var d Definition
	var previous Proof
	if json.Unmarshal([]byte(raw), &d) != nil || index < 0 || index >= len(d.Entries) || stage != from || json.Unmarshal([]byte(old), &previous) != nil || !match(previous) || remoteProofValid(d, d.Entries[index], p) != nil {
		return ErrState
	}
	if err := verifyRemoteExecution(ctx, tx, d, &p); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > 16<<20 {
		return ErrLimit
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE=?,PROOF=? WHERE JOB_ID=? AND ORDINAL=?`, to, string(data), id, index); err != nil {
		return err
	}
	var saved string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, id, index).Scan(&stage, &saved); err != nil || stage != to || saved != string(data) {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) VerifyRemotePhase(ctx context.Context, id string, index int, p Proof, state string) error {
	if err := s.verifyRemoteJobExecution(ctx, s.db, id, &p); err != nil {
		return err
	}
	if state != "staging" && state != "prepared" && state != "publishing" {
		return ErrState
	}
	var raw, stored string
	if err := s.db.QueryRowContext(ctx, `SELECT I.STATE,I.PROOF FROM GO_ORGANIZATION_ITEMS I JOIN GO_ORGANIZATION_JOBS J ON J.ID=I.JOB_ID WHERE I.JOB_ID=? AND I.ORDINAL=? AND J.STATE='active'`, id, index).Scan(&stored, &raw); err != nil {
		return err
	}
	var actual Proof
	if stored != state || json.Unmarshal([]byte(raw), &actual) != nil || Digest(actual) != Digest(p) {
		return ErrState
	}
	return nil
}
func reserveRemoteTargets(ctx context.Context, tx *sql.Tx, id string, d Definition) error {
	if d.Remote == nil {
		return nil
	}
	fold := func(s string) string { return cases.Fold().String(norm.NFD.String(s)) }
	for _, e := range d.Entries {
		absolute := fold(strings.TrimRight(d.TargetRoot, "/") + "/" + e.Target)
		for _, part := range strings.Split(absolute, "/") {
			if strings.HasPrefix(part, ".nastool-") {
				return ErrPath
			}
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_ORGANIZATION_REMOTE_TARGETS WHERE TARGET=? OR substr(TARGET,1,length(?)+1)=?||'/' OR substr(?,1,length(TARGET)+1)=TARGET||'/')`, absolute, absolute, absolute, absolute).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return ErrClaimed
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_REMOTE_TARGETS(TARGET,JOB_ID) VALUES(?,?)`, absolute, id); err != nil {
			return err
		}
	}
	return nil
}

type RemoteClosure struct {
	Actor   string `json:"actor"`
	Reason  string `json:"reason"`
	Created string `json:"created"`
}

func (s *Store) CloseRemoteJob(ctx context.Context, job Job, actor, reason string) error {
	if !RemoteMode(job.Mode) || job.Definition.Remote == nil || actor == "" || strings.TrimSpace(reason) == "" || len(reason) > 512 || job.State == "completed" || job.State == "cancelled" || job.State == SourceRestoring || job.State == SourceRestored {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if job.State == "remote_closed" {
		var raw string
		var receipt RemoteClosure
		if err := tx.QueryRowContext(ctx, `SELECT RECEIPT FROM GO_ORGANIZATION_REMOTE_CLOSURES WHERE JOB_ID=?`, job.ID).Scan(&raw); err != nil {
			return err
		}
		if json.Unmarshal([]byte(raw), &receipt) != nil || receipt.Actor != actor || receipt.Reason != reason {
			return ErrState
		}
		return nil
	}
	if err := sourceRestoreTxSnapshot(ctx, tx, job); err != nil {
		return err
	}
	for _, item := range job.Items {
		if item.State == "completed" {
			if item.Reason != "" {
				return ErrState
			}
			continue
		}
		if item.Proof.SourceHold != "" {
			return ErrState
		}
	}
	receipt := RemoteClosure{actor, reason, time.Now().UTC().Format(time.RFC3339Nano)}
	data, _ := json.Marshal(receipt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_REMOTE_CLOSURES(JOB_ID,RECEIPT) VALUES(?,?)`, job.ID, string(data)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_JOBS SET STATE='remote_closed' WHERE ID=?`, job.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='remote_stopped',REASON='Remote work explicitly stopped; every target, recovery object and reservation retained' WHERE JOB_ID=? AND STATE!='completed'`, job.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE='cancelled' WHERE JOB_ID=? AND STATE='pending'`, job.ID); err != nil {
		return err
	}
	var state, raw string
	if err := tx.QueryRowContext(ctx, `SELECT J.STATE,C.RECEIPT FROM GO_ORGANIZATION_JOBS J JOIN GO_ORGANIZATION_REMOTE_CLOSURES C ON C.JOB_ID=J.ID WHERE J.ID=?`, job.ID).Scan(&state, &raw); err != nil || state != "remote_closed" || raw != string(data) {
		return ErrState
	}
	return tx.Commit()
}
