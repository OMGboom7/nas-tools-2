package organization

import (
	"context"
	"encoding/json"
)

// AbandonUnpublished requires the caller's per-job guard, explicit staging
// disposal consent and stopped old executors. Never use it to reset an uncertain
// running item, infer publication from absence, or recover unjournaled objects.
func (s *Store) AbandonUnpublished(ctx context.Context, job Job, validate func() error) error {
	if validate == nil || (job.State != "needs_review" && job.State != "abandoning") {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	resuming := job.State == "abandoning"
	prepared := false
	for _, item := range job.Items {
		switch item.State {
		case "planned":
			if resuming || item.Proof != (Proof{}) {
				return ErrState
			}
		case "prepared":
			if resuming {
				return ErrState
			}
			prepared = true
			if item.Proof.Temp != TempName(job.ID, item.Index) {
				return ErrState
			}
			if err := inspectUnpublished(ctx, job.Definition, item.Entry, item.Proof, false, nil); err != nil {
				return err
			}
		case "discarding":
			if !resuming || item.Proof.Temp != TempName(job.ID, item.Index) {
				return ErrState
			}
			if err := inspectUnpublished(ctx, job.Definition, item.Entry, item.Proof, true, nil); err != nil {
				return err
			}
		case "abandoned":
			if !resuming {
				return ErrState
			}
			if item.Proof != (Proof{}) {
				if item.Proof.Temp != TempName(job.ID, item.Index) {
					return ErrState
				}
				if err := inspectDiscarded(ctx, job.Definition, item.Entry, item.Proof); err != nil {
					return err
				}
			}
		default:
			return ErrState
		}
	}
	if !resuming {
		if !prepared {
			return ErrState
		}
		if err := s.beginAbandon(ctx, job); err != nil {
			return err
		}
		var err error
		job, err = s.Get(ctx, job.ID)
		if err != nil {
			return err
		}
	}
	for _, item := range job.Items {
		if item.State == "abandoned" {
			continue
		}
		if item.State != "discarding" {
			return ErrState
		}
		if err := validate(); err != nil {
			return err
		}
		if err := inspectUnpublished(ctx, job.Definition, item.Entry, item.Proof, true, func() error {
			return s.verifyDiscard(ctx, job.ID, item.Index, item.Proof)
		}); err != nil {
			return err
		}
		if err := s.finishDiscard(ctx, job.ID, item.Index, item.Proof); err != nil {
			return err
		}
	}
	// Recheck the cleanup receipts before releasing the whole job reservation.
	for _, item := range job.Items {
		if item.Proof != (Proof{}) {
			if err := inspectDiscarded(ctx, job.Definition, item.Entry, item.Proof); err != nil {
				return err
			}
		}
	}
	if err := validate(); err != nil {
		return err
	}
	return s.finishAbandon(ctx, job.ID)
}

// The whole-job intent is atomic: no subtitle or later media item remains
// executable after the operator chooses abandonment. Proofs remain as audit.
func (s *Store) beginAbandon(ctx context.Context, job Job) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, raw string
	if err = tx.QueryRowContext(ctx, `SELECT STATE,DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&state, &raw); err != nil {
		return err
	}
	var definition Definition
	if state != "active" || json.Unmarshal([]byte(raw), &definition) != nil || Digest(definition) != Digest(job.Definition) {
		return ErrState
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, job.ID).Scan(&count); err != nil {
		return err
	}
	if count != len(job.Items) {
		return ErrState
	}
	for _, item := range job.Items {
		if err = tx.QueryRowContext(ctx, `SELECT STATE,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, job.ID, item.Index).Scan(&state, &raw); err != nil {
			return err
		}
		var proof Proof
		if state != item.State || (state != "planned" && state != "prepared") || json.Unmarshal([]byte(raw), &proof) != nil || Digest(proof) != Digest(item.Proof) {
			return ErrState
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_JOBS SET STATE='abandoning' WHERE ID=?`, job.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE=CASE STATE WHEN 'prepared' THEN 'discarding' ELSE 'abandoned' END,REASON='Explicit unpublished staging disposal requested; no successful transfer recorded' WHERE JOB_ID=?`, job.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) verifyDiscard(ctx context.Context, id string, index int, p Proof) error {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT I.PROOF FROM GO_ORGANIZATION_ITEMS I JOIN GO_ORGANIZATION_JOBS J ON J.ID=I.JOB_ID WHERE I.JOB_ID=? AND I.ORDINAL=? AND I.STATE='discarding' AND J.STATE='abandoning'`, id, index).Scan(&raw); err != nil {
		return err
	}
	var saved Proof
	if json.Unmarshal([]byte(raw), &saved) != nil || Digest(saved) != Digest(p) {
		return ErrState
	}
	return nil
}

func (s *Store) finishDiscard(ctx context.Context, id string, index int, p Proof) error {
	if err := s.verifyDiscard(ctx, id, index, p); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='abandoned' WHERE JOB_ID=? AND ORDINAL=? AND STATE='discarding' AND EXISTS (SELECT 1 FROM GO_ORGANIZATION_JOBS WHERE ID=? AND STATE='abandoning')`, id, index, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	return err
}

func (s *Store) finishAbandon(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_JOBS SET STATE='abandoned' WHERE ID=? AND STATE='abandoning' AND NOT EXISTS (SELECT 1 FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND STATE!='abandoned')`, id, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
