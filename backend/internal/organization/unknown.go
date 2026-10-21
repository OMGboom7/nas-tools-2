package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type UnknownRecord struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	Dest  string `json:"dest"`
	Mode  string `json:"mode"`
	State string `json:"state"`
}
type unknownReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readUnknown(ctx context.Context, reader unknownReader, id int64) (UnknownRecord, error) {
	var record UnknownRecord
	err := reader.QueryRowContext(ctx, `SELECT ID,COALESCE(PATH,''),COALESCE(DEST,''),COALESCE(MODE,''),COALESCE(STATE,'') FROM TRANSFER_UNKNOWN WHERE ID=?`, id).Scan(&record.ID, &record.Path, &record.Dest, &record.Mode, &record.State)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}
func (s *Store) GetUnknown(ctx context.Context, id int64) (UnknownRecord, error) {
	return readUnknown(ctx, s.db, id)
}

// Never marks a row based on a target name, missing source, or operator guess.
// All media/attachments and move cleanup receipts must already be committed.
func (s *Store) ResolveUnknown(ctx context.Context, job Job) error {
	if job.Definition.LegacyBatch != nil {
		return nil
	}
	if job.Definition.Unknown == nil {
		return nil
	}
	currentJob, err := s.Get(ctx, job.ID)
	if err != nil {
		return err
	}
	if currentJob.UnknownState == "resolved" || currentJob.UnknownState == "preserved" {
		return nil
	}
	if currentJob.UnknownState == "detached" {
		return nil
	}
	for _, item := range currentJob.Items {
		if item.State != "completed" || item.Reason != "" {
			return nil
		}
	}
	if currentJob.Definition.Remote != nil {
		for _, item := range currentJob.Items {
			if err := VerifyCompletedTarget(ctx, currentJob, item); err != nil {
				return err
			}
		}
	}
	preserve := currentJob.Definition.LegacyRename != nil && currentJob.Definition.LegacyRename.PreserveUnknown
	if !preserve {
		if err = verifyUnknownScope(ctx, currentJob); err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE JOB_ID=? AND RECORD_ID=?`, job.ID, job.Definition.Unknown.ID).Scan(&state); err != nil {
		return err
	}
	if state == "resolved" || state == "preserved" {
		return nil
	}
	if state != "pending" {
		return ErrState
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND (STATE!='completed' OR REASON!='')`, job.ID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&raw); err != nil {
		return err
	}
	var definition Definition
	if json.Unmarshal([]byte(raw), &definition) != nil || definition.Unknown == nil || Digest(definition) != Digest(job.Definition) {
		return ErrState
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, job.ID).Scan(&count); err != nil {
		return err
	}
	if count < 1 || count != len(definition.Entries) {
		return ErrState
	}
	if err = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&state); err != nil {
		return err
	}
	if state != "active" {
		return ErrState
	}
	current, err := readUnknown(ctx, tx, job.Definition.Unknown.ID)
	if err != nil {
		return err
	}
	actualState := current.State
	if preserve && Digest(current) != Digest(*job.Definition.Unknown) {
		return ErrState
	}
	if job.Definition.Unknown.State == "N" {
		current.State = "N"
	} else if !job.Definition.UnknownAllowProcessed || job.Definition.Unknown.State != "Y" || actualState != "Y" {
		return ErrState
	}
	if (actualState != "N" && actualState != "Y") || Digest(current) != Digest(*job.Definition.Unknown) {
		return ErrState
	}
	if actualState == "N" && !preserve {
		if _, err = tx.ExecContext(ctx, `UPDATE TRANSFER_UNKNOWN SET STATE='Y' WHERE ID=? AND STATE='N'`, current.ID); err != nil {
			return err
		}
	}
	resolution := "resolved"
	if preserve {
		resolution = "preserved"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE=? WHERE JOB_ID=? AND STATE='pending'`, resolution, job.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) VerifyUnknown(ctx context.Context, job Job) error {
	if job.Definition.Unknown == nil {
		return nil
	}
	// Finished item receipts may still need the independent compatibility update;
	// allow that explicit DB-only retry even if the legacy record was changed.
	complete := true
	for _, item := range job.Items {
		if item.State != "completed" || item.Reason != "" {
			complete = false
		}
	}
	if complete {
		return nil
	}
	var state string
	owner, err := legacyOwner(ctx, s.db, job.Definition, job.ID)
	if err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE JOB_ID=? AND RECORD_ID=?`, owner, job.Definition.Unknown.ID).Scan(&state); err != nil {
		return err
	}
	if state != "pending" {
		return ErrState
	}
	current, err := s.GetUnknown(ctx, job.Definition.Unknown.ID)
	if err != nil {
		return err
	}
	if current.State != "N" && !(job.Definition.UnknownAllowProcessed && current.State == "Y") || Digest(current) != Digest(*job.Definition.Unknown) {
		return ErrState
	}
	return nil
}
