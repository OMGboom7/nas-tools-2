package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type UnknownReview struct {
	JobID       string         `json:"jobId"`
	State       string         `json:"state"`
	Original    UnknownRecord  `json:"original"`
	Current     *UnknownRecord `json:"current"`
	Fingerprint string         `json:"fingerprint"`
	Eligible    bool           `json:"eligible"`
}
type UnknownDetachReceipt struct {
	Review  UnknownReview `json:"review"`
	Actor   string        `json:"actor"`
	Reason  string        `json:"reason"`
	Created string        `json:"created"`
}

func reviewUnknown(ctx context.Context, reader unknownReader, job Job) (UnknownReview, error) {
	if job.Definition.Unknown == nil {
		return UnknownReview{}, ErrState
	}
	review := UnknownReview{JobID: job.ID, Original: *job.Definition.Unknown}
	if err := reader.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE JOB_ID=? AND RECORD_ID=?`, job.ID, review.Original.ID).Scan(&review.State); err != nil {
		return review, err
	}
	current, err := readUnknown(ctx, reader, review.Original.ID)
	if err == nil {
		review.Current = &current
	} else if !errors.Is(err, ErrNotFound) {
		return review, err
	}
	var incomplete, total int
	if err = reader.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN STATE!='completed' OR REASON!='' THEN 1 ELSE 0 END),0) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, job.ID).Scan(&total, &incomplete); err != nil {
		return review, err
	}
	review.Eligible = review.State == "pending" && incomplete == 0 && total > 0 && total == len(job.Definition.Entries)
	review.Fingerprint = Digest(struct {
		Definition   string
		JobID, State string
		Original     UnknownRecord
		Current      *UnknownRecord
		Eligible     bool
	}{Digest(job.Definition), review.JobID, review.State, review.Original, review.Current, review.Eligible})
	return review, nil
}

func (s *Store) ReviewUnknown(ctx context.Context, id string) (UnknownReview, error) {
	job, err := s.Get(ctx, id)
	if err != nil {
		return UnknownReview{}, err
	}
	if job.LegacyBatch != nil && job.Definition.Unknown != nil {
		return UnknownReview{JobID: job.ID, State: "batch_managed", Original: *job.Definition.Unknown, Eligible: false, Fingerprint: Digest(job.Definition)}, nil
	}
	return reviewUnknown(ctx, s.db, job)
}

// Requires the per-job executor guard and separate operator confirmation.
// This releases ONLY a completed job's legacy-record reservation. Target claims,
// history, sources, staging, the legacy row and its state are never modified.
func (s *Store) DetachUnknown(ctx context.Context, job Job, fingerprint, actor, reason string) error {
	if job.Definition.LegacyBatch != nil {
		return ErrState
	}
	reason = strings.TrimSpace(reason)
	if job.Definition.Unknown == nil || len(fingerprint) != 64 || actor == "" || reason == "" || len(reason) > 512 {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw, state string
	if err = tx.QueryRowContext(ctx, `SELECT DEFINITION,STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&raw, &state); err != nil {
		return err
	}
	var definition Definition
	if state != "active" || json.Unmarshal([]byte(raw), &definition) != nil || Digest(definition) != Digest(job.Definition) {
		return ErrState
	}
	var saved string
	err = tx.QueryRowContext(ctx, `SELECT RECEIPT FROM GO_ORGANIZATION_UNKNOWN_DETACH WHERE JOB_ID=?`, job.ID).Scan(&saved)
	if err == nil {
		var receipt UnknownDetachReceipt
		if json.Unmarshal([]byte(saved), &receipt) != nil || receipt.Review.Fingerprint != fingerprint || receipt.Reason != reason {
			return ErrState
		}
		if err = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE JOB_ID=?`, job.ID).Scan(&state); err != nil || state != "detached" {
			return ErrState
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	review, err := reviewUnknown(ctx, tx, job)
	if err != nil {
		return err
	}
	if !review.Eligible || review.Fingerprint != fingerprint {
		return ErrState
	}
	rows, err := tx.QueryContext(ctx, `SELECT PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, job.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var encoded string
		var proof Proof
		if err = rows.Scan(&encoded); err != nil {
			rows.Close()
			return err
		}
		if json.Unmarshal([]byte(encoded), &proof) != nil || proof.Incomplete || proof.Identity == "" || len(proof.Digest) != 64 || proof.ParentIdentity == "" || proof.StagingIdentity == "" || MoveMode(definition.Mode) && (proof.SourceHold == "" || proof.SourceHoldIdentity == "" || proof.SourceParentIdentity == "") {
			rows.Close()
			return ErrState
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	receipt := UnknownDetachReceipt{Review: review, Actor: actor, Reason: reason, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_UNKNOWN_DETACH VALUES (?,?)`, job.ID, string(encoded)); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE='detached' WHERE JOB_ID=? AND RECORD_ID=? AND STATE='pending'`, job.ID, review.Original.ID)
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
	return tx.Commit()
}
