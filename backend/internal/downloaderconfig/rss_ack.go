package downloaderconfig

import (
	"context"
	"database/sql"
	"errors"
)

// An actual client acknowledgement survives a later failure in the history
// transaction. The checkpoint contains only the receipt and remote task IDs.
func (s *Store) AcknowledgeRSS(ctx context.Context, r ManualSubmission, task string) error {
	if r.Definition.RSSKey == "" || task == "" || len(task) > 128 {
		return ErrManualPending
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_CUSTOM_RSS_ACKS(SUBMISSION_ID,TASK)
 SELECT ID,? FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS WHERE ID=? AND STATE='pending' AND DOCUMENT=?`, task, r.ID, manualDocument(r))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	return tx.Commit()
}

// A retry may use the immutable payload of an acknowledged original request,
// including its original manual/scheduled mode. It cannot adopt a pending
// request whose downloader never supplied a task identity.
func (s *Store) RestoreAcknowledgedRSS(ctx context.Context, key string, taskID int64, receiptID, actor string) (ManualSubmission, bool, error) {
	var r ManualSubmission
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_CUSTOM_RSS_ACKS')`).Scan(&exists); err != nil || !exists {
		return r, false, err
	}
	var id, task string
	err := s.database.QueryRowContext(ctx, `SELECT a.SUBMISSION_ID,a.TASK FROM GO_CUSTOM_RSS_ACKS a
 JOIN GO_MANUAL_DOWNLOAD_SUBMISSIONS m ON m.ID=a.SUBMISSION_ID
 JOIN GO_RSS_DOWNLOAD_CLAIMS c ON c.RESOURCE_KEY=? AND c.TASK_ID=? AND c.STATE='pending'
 WHERE m.STATE='pending' AND json_extract(m.DOCUMENT,'$.rssKey')=c.RESOURCE_KEY AND json_extract(m.DOCUMENT,'$.rssTask')=c.TASK_ID
 AND (?='' OR m.ID=?)`, key, taskID, receiptID, receiptID).Scan(&id, &task)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	r, err = s.ManualSubmission(ctx, id)
	if err != nil {
		return r, false, err
	}
	h, err := s.ClassicPendingHistory(ctx, id)
	if err != nil {
		return r, false, err
	}
	if err := s.CompleteWithHistory(ctx, r, task, "acknowledged", actor, &h); err != nil {
		return r, false, ErrManualPending
	}
	r, err = s.ManualSubmission(ctx, id)
	return r, err == nil, err
}
