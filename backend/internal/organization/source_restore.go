package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const SourceRestoring = "source_restoring"
const SourceRestored = "source_restored"
const sourceRestorePending = "Source restoration intent saved; no further transfer or source cleanup permitted"
const sourceRestoreStopped = "Source restoration closed this unfinished transfer; target, staging and recovery evidence retained"

type SourceRestoreItem struct {
	Index   int    `json:"index"`
	Source  string `json:"source"`
	Outcome string `json:"outcome"`
}
type SourceRestoreReceipt struct {
	JobID           string              `json:"jobId"`
	Actor           string              `json:"actor"`
	Reason          string              `json:"reason"`
	Created         string              `json:"created"`
	Updated         string              `json:"updated"`
	State           string              `json:"state"`
	TargetsRetained bool                `json:"targetsRetained"`
	Items           []SourceRestoreItem `json:"items"`
}
type SourceRestoreReview struct {
	JobID       string              `json:"jobId"`
	State       string              `json:"state"`
	Fingerprint string              `json:"fingerprint"`
	CanRestore  bool                `json:"canRestore"`
	Reason      string              `json:"reason"`
	Items       []SourceRestoreItem `json:"items"`
}
type sourceRestoreFrozen struct {
	Index                        int
	FromState, FromReason, Proof string
	Policy, Outcome              string
	Done                         bool
}
type sourceRestoreDocument struct {
	Receipt                   SourceRestoreReceipt
	InitialReview, Definition string
	Items                     []sourceRestoreFrozen
}

func sourceRestorePolicy(item JobItem) string {
	switch item.State {
	case "planned":
		if item.Proof == (Proof{}) {
			return "not_started"
		}
	case "completed":
		if item.Reason == "" {
			return "completed_retained"
		}
	case "prepared", "staging", "publishing":
		if item.Proof.SourceHold == "" && item.Proof.Identity != "" && len(item.Proof.Digest) == 64 && (item.State == "staging") == item.Proof.Incomplete {
			return "source_retained"
		}
	case "moving", "quarantined":
		if !item.Proof.Incomplete && item.Proof.SourceHold != "" {
			return "restore"
		}
	}
	return ""
}
func (d sourceRestoreDocument) expectedState(i int) string {
	f := d.Items[i]
	if f.Policy == "completed_retained" {
		return "completed"
	}
	if !f.Done {
		return "source_restore_pending"
	}
	if f.Policy == "restore" {
		return SourceRestored
	}
	return "source_restore_stopped"
}
func (s *Store) sourceRestoreDocument(ctx context.Context, job Job) (*sourceRestoreDocument, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_ORGANIZATION_SOURCE_RESTORE WHERE JOB_ID=?`, job.ID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d sourceRestoreDocument
	if len(raw) > 8<<20 || json.Unmarshal([]byte(raw), &d) != nil || d.Definition != Digest(job.Definition) || d.Receipt.JobID != job.ID || d.Receipt.Actor == "" || strings.TrimSpace(d.Receipt.Reason) == "" || len(d.Receipt.Reason) > 512 || !d.Receipt.TargetsRetained || len(d.InitialReview) != 64 || len(d.Items) != len(job.Items) || len(d.Receipt.Items) != len(job.Items) || d.Receipt.State != job.State || (job.State != SourceRestoring && job.State != SourceRestored) {
		return nil, ErrState
	}
	if _, err := time.Parse(time.RFC3339Nano, d.Receipt.Created); err != nil {
		return nil, ErrState
	}
	if _, err := time.Parse(time.RFC3339Nano, d.Receipt.Updated); err != nil {
		return nil, ErrState
	}
	for i, item := range job.Items {
		f := d.Items[i]
		before := item
		before.State, before.Reason = f.FromState, f.FromReason
		if f.Index != i || item.Index != i || f.Proof != Digest(item.Proof) || f.Policy == "" || sourceRestorePolicy(before) != f.Policy || item.State != d.expectedState(i) || d.Receipt.Items[i].Index != i || d.Receipt.Items[i].Source != item.Source || d.Receipt.Items[i].Outcome != f.Outcome || job.State == SourceRestored && !f.Done {
			return nil, ErrState
		}
		if f.Policy == "completed_retained" {
			if item.Reason != f.FromReason || !f.Done {
				return nil, ErrState
			}
		} else if !f.Done && item.Reason != sourceRestorePending || f.Done && item.Reason != sourceRestoreStopped {
			return nil, ErrState
		}
	}
	return &d, nil
}

// Review does not acquire/create a filesystem lock or modify any journal or
// recovery object. Fingerprints bind both the saved intent and current source
// evidence; targets are intentionally not adopted, verified or changed here.
func (s *Store) ReviewSourceRestore(ctx context.Context, job Job) (SourceRestoreReview, error) {
	r := SourceRestoreReview{JobID: job.ID, State: job.State, Items: []SourceRestoreItem{}, CanRestore: true, Fingerprint: Digest([]any{job.ID, job.State, job.Definition})}
	if !MoveMode(job.Mode) || (job.State != "needs_review" && job.State != SourceRestoring && job.State != SourceRestored) {
		r.CanRestore = false
		r.Reason = "only an interrupted move with durable original-source evidence can be restored"
		return r, nil
	}
	doc, err := s.sourceRestoreDocument(ctx, job)
	if err != nil {
		return r, err
	}
	if job.State != "needs_review" && doc == nil {
		return r, ErrState
	}
	if job.State == SourceRestored {
		r.CanRestore = false
		r.Reason = "saved restoration receipt; no transfer or restoration is replayed"
		r.Items = doc.Receipt.Items
		r.Fingerprint = Digest(doc)
		return r, nil
	}
	parts := []any{job.ID, job.Fingerprint, job.State, job.Definition, doc}
	restores := 0
	for i, item := range job.Items {
		err = nil
		policy := sourceRestorePolicy(item)
		if doc != nil {
			policy = doc.Items[i].Policy
		}
		outcome, evidence := "blocked", ""
		if policy == "restore" && item.Proof.SourceHold != MoveHoldName(job.ID, item.Index) || policy != "not_started" && policy != "completed_retained" && item.Proof.Temp != TempName(job.ID, item.Index) {
			policy = ""
		}
		if policy == "not_started" || policy == "completed_retained" {
			outcome = policy
		} else if policy != "" {
			outcome, evidence, err = inspectSourceRestore(ctx, job.Definition, item.Entry, item.Proof, false, nil)
		} else {
			err = ErrState
		}
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		if err != nil {
			r.CanRestore = false
			r.Reason = "original source, recovery namespace or saved item state requires review; nothing will be overwritten"
			outcome = "blocked"
		}
		if policy == "restore" {
			restores++
		}
		parts = append(parts, []any{item.Index, item.State, item.Reason, item.Proof, policy, outcome, evidence})
		r.Items = append(r.Items, SourceRestoreItem{Index: item.Index, Source: item.Source, Outcome: outcome})
	}
	if restores == 0 {
		r.CanRestore = false
		r.Reason = "no journaled isolated original source to restore"
	}
	r.Fingerprint = Digest(parts)
	return r, nil
}

// RestoreSource requires the caller's per-job guard and separate confirmation
// of source restoration and stopped external writers. It never deletes targets,
// recovery/staging objects, history or claims, and never completes an intake.
func (s *Store) RestoreSource(ctx context.Context, job Job, fingerprint, actor, reason string, validate func() error) error {
	if err := verifyLegacyDisposition(ctx, s.db, job.ID); err != nil {
		return err
	}
	if actor == "" || strings.TrimSpace(reason) == "" || len(reason) > 512 || len(fingerprint) != 64 || validate == nil {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	doc, err := s.sourceRestoreDocument(ctx, job)
	if err != nil {
		return err
	}
	if doc != nil && doc.Receipt.State == SourceRestored {
		if doc.InitialReview == fingerprint && doc.Receipt.Actor == actor && doc.Receipt.Reason == reason {
			return nil
		}
		return ErrState
	}
	review, err := s.ReviewSourceRestore(ctx, job)
	if err != nil {
		return err
	}
	if !review.CanRestore || review.Fingerprint != fingerprint {
		return ErrState
	}
	if doc == nil {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		doc = &sourceRestoreDocument{Definition: Digest(job.Definition), InitialReview: fingerprint, Receipt: SourceRestoreReceipt{JobID: job.ID, Actor: actor, Reason: reason, Created: now, Updated: now, State: SourceRestoring, TargetsRetained: true, Items: []SourceRestoreItem{}}, Items: []sourceRestoreFrozen{}}
		for _, item := range job.Items {
			policy := sourceRestorePolicy(item)
			done := policy == "completed_retained"
			outcome := "pending"
			if done {
				outcome = policy
			}
			doc.Items = append(doc.Items, sourceRestoreFrozen{Index: item.Index, FromState: item.State, FromReason: item.Reason, Proof: Digest(item.Proof), Policy: policy, Done: done, Outcome: outcome})
			doc.Receipt.Items = append(doc.Receipt.Items, SourceRestoreItem{Index: item.Index, Source: item.Source, Outcome: outcome})
		}
		if err := s.beginSourceRestore(ctx, job, *doc); err != nil {
			return err
		}
		job, err = s.Get(ctx, job.ID)
		if err != nil {
			return err
		}
	} else if doc.Receipt.Actor != actor || doc.Receipt.Reason != reason {
		return ErrState
	}
	for i, item := range job.Items {
		f := doc.Items[i]
		if f.Done {
			continue
		}
		if err := validate(); err != nil {
			return err
		}
		outcome := f.Policy
		if f.Policy == "restore" || f.Policy == "source_retained" {
			outcome, _, err = inspectSourceRestore(ctx, job.Definition, item.Entry, item.Proof, true, func() error {
				if err := validate(); err != nil {
					return err
				}
				return s.verifySourceRestore(ctx, job.ID, *doc)
			})
			if err != nil {
				return err
			}
		}
		if err := s.finishSourceRestoreItem(ctx, job.ID, doc, i, outcome); err != nil {
			return err
		}
		job, err = s.Get(ctx, job.ID)
		if err != nil {
			return err
		}
	}
	for i, item := range job.Items {
		if doc.Items[i].Policy == "restore" || doc.Items[i].Policy == "source_retained" {
			outcome, _, err := inspectSourceRestore(ctx, job.Definition, item.Entry, item.Proof, false, nil)
			if err != nil || outcome != "source_in_place" {
				return ErrState
			}
		}
	}
	if err := validate(); err != nil {
		return err
	}
	return s.finishSourceRestore(ctx, job.ID, *doc)
}

func sourceRestoreTxSnapshot(ctx context.Context, tx *sql.Tx, job Job) error {
	var raw, state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&state, &raw); err != nil {
		return err
	}
	var definition Definition
	if state != "active" || json.Unmarshal([]byte(raw), &definition) != nil || Digest(definition) != Digest(job.Definition) {
		return ErrState
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, job.ID).Scan(&count); err != nil || count != len(job.Items) {
		return ErrState
	}
	for _, item := range job.Items {
		var reason string
		var proof Proof
		if err := tx.QueryRowContext(ctx, `SELECT STATE,REASON,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, job.ID, item.Index).Scan(&state, &reason, &raw); err != nil {
			return err
		}
		if state != item.State || reason != item.Reason || json.Unmarshal([]byte(raw), &proof) != nil || Digest(proof) != Digest(item.Proof) {
			return ErrState
		}
	}
	return nil
}
func (s *Store) beginSourceRestore(ctx context.Context, job Job, doc sourceRestoreDocument) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sourceRestoreTxSnapshot(ctx, tx, job); err != nil {
		return err
	}
	raw, err := json.Marshal(doc)
	if err != nil || len(raw) > 8<<20 {
		return ErrState
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_SOURCE_RESTORE(JOB_ID,DOCUMENT) VALUES(?,?)`, job.ID, string(raw)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_JOBS SET STATE=? WHERE ID=?`, SourceRestoring, job.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='source_restore_pending',REASON=? WHERE JOB_ID=? AND STATE!='completed'`, sourceRestorePending, job.ID); err != nil {
		return err
	}
	if err := verifySourceRestoreTx(ctx, tx, job.ID, doc); err != nil {
		return err
	}
	return tx.Commit()
}
func verifySourceRestoreTx(ctx context.Context, tx *sql.Tx, id string, doc sourceRestoreDocument) error {
	var raw, state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&state, &raw); err != nil {
		return err
	}
	var definition Definition
	if state != doc.Receipt.State || json.Unmarshal([]byte(raw), &definition) != nil || Digest(definition) != doc.Definition {
		return ErrState
	}
	if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_ORGANIZATION_SOURCE_RESTORE WHERE JOB_ID=?`, id).Scan(&raw); err != nil {
		return err
	}
	var saved sourceRestoreDocument
	if json.Unmarshal([]byte(raw), &saved) != nil || Digest(saved) != Digest(doc) {
		return ErrState
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, id).Scan(&count); err != nil || count != len(doc.Items) {
		return ErrState
	}
	for i, f := range doc.Items {
		var reason string
		var p Proof
		if err := tx.QueryRowContext(ctx, `SELECT STATE,REASON,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, id, i).Scan(&state, &reason, &raw); err != nil {
			return err
		}
		if state != doc.expectedState(i) || json.Unmarshal([]byte(raw), &p) != nil || Digest(p) != f.Proof {
			return ErrState
		}
		expected := sourceRestorePending
		if f.Done {
			expected = sourceRestoreStopped
		}
		if f.Policy == "completed_retained" {
			expected = f.FromReason
		}
		if reason != expected {
			return ErrState
		}
	}
	return nil
}
func (s *Store) verifySourceRestore(ctx context.Context, id string, doc sourceRestoreDocument) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return verifySourceRestoreTx(ctx, tx, id, doc)
}
func (s *Store) finishSourceRestoreItem(ctx context.Context, id string, doc *sourceRestoreDocument, i int, outcome string) error {
	if i < 0 || i >= len(doc.Items) || doc.Items[i].Done {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifySourceRestoreTx(ctx, tx, id, *doc); err != nil {
		return err
	}
	next := *doc
	next.Items = append([]sourceRestoreFrozen(nil), doc.Items...)
	next.Receipt.Items = append([]SourceRestoreItem(nil), doc.Receipt.Items...)
	next.Items[i].Done, next.Items[i].Outcome = true, outcome
	next.Receipt.Items[i].Outcome = outcome
	next.Receipt.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE=?,REASON=? WHERE JOB_ID=? AND ORDINAL=?`, next.expectedState(i), sourceRestoreStopped, id, i); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_SOURCE_RESTORE SET DOCUMENT=? WHERE JOB_ID=?`, string(data), id); err != nil {
		return err
	}
	if err := verifySourceRestoreTx(ctx, tx, id, next); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	*doc = next
	return nil
}
func (s *Store) finishSourceRestore(ctx context.Context, id string, doc sourceRestoreDocument) error {
	for _, f := range doc.Items {
		if !f.Done {
			return ErrState
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifySourceRestoreTx(ctx, tx, id, doc); err != nil {
		return err
	}
	doc.Receipt.State, doc.Receipt.Updated = SourceRestored, time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_JOBS SET STATE=? WHERE ID=?`, SourceRestored, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_SOURCE_RESTORE SET DOCUMENT=? WHERE JOB_ID=?`, string(data), id); err != nil {
		return err
	}
	if err := cancelUnknownLink(ctx, tx, id); err != nil {
		return err
	}
	if err := verifySourceRestoreTx(ctx, tx, id, doc); err != nil {
		return err
	}
	return tx.Commit()
}
