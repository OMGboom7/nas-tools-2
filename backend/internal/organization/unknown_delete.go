package organization

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"
)

// Nullable fields preserve the original row exactly, including NULL values.
type UnknownRawRecord struct {
	ID    int64   `json:"id,string"`
	Path  *string `json:"path"`
	Dest  *string `json:"dest"`
	Mode  *string `json:"mode"`
	State *string `json:"state"`
}
type UnknownDeleteReview struct {
	Record       UnknownRawRecord `json:"record"`
	Fingerprint  string           `json:"fingerprint"`
	PendingJobID string           `json:"pendingJobId,omitempty"`
	Eligible     bool             `json:"eligible"`
}
type UnknownRestoreReceipt struct {
	Actor   string `json:"actor"`
	Created string `json:"created"`
}
type UnknownDeleteReceipt struct {
	ID      string                 `json:"id"`
	Review  UnknownDeleteReview    `json:"review"`
	Actor   string                 `json:"actor"`
	Created string                 `json:"created"`
	Restore *UnknownRestoreReceipt `json:"restore,omitempty"`
}
type UnknownDeleteSelector struct {
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

func readUnknownRaw(ctx context.Context, reader unknownReader, id int64) (UnknownRawRecord, error) {
	row := UnknownRawRecord{}
	var pathType, destType, modeType, stateType string
	err := reader.QueryRowContext(ctx, `SELECT ID,PATH,DEST,MODE,STATE,typeof(PATH),typeof(DEST),typeof(MODE),typeof(STATE) FROM TRANSFER_UNKNOWN WHERE ID=?`, id).Scan(&row.ID, &row.Path, &row.Dest, &row.Mode, &row.State, &pathType, &destType, &modeType, &stateType)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil {
		for _, kind := range []string{pathType, destType, modeType, stateType} {
			if kind != "text" && kind != "null" {
				return row, ErrState
			}
		}
		for _, value := range []*string{row.Path, row.Dest, row.Mode, row.State} {
			if value != nil && !utf8.ValidString(*value) {
				return row, ErrState
			}
		}
	}
	return row, err
}
func reviewUnknownDelete(ctx context.Context, reader unknownReader, id int64) (UnknownDeleteReview, error) {
	record, err := readUnknownRaw(ctx, reader, id)
	if err != nil {
		return UnknownDeleteReview{}, err
	}
	review := UnknownDeleteReview{Record: record}
	err = reader.QueryRowContext(ctx, `SELECT JOB_ID FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE RECORD_ID=? AND STATE='pending'`, id).Scan(&review.PendingJobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return review, err
	}
	review.Eligible = review.PendingJobID == ""
	review.Fingerprint = Digest(struct {
		Record  UnknownRawRecord
		Pending string
	}{record, review.PendingJobID})
	return review, nil
}
func (s *Store) ReviewUnknownDelete(ctx context.Context, id int64) (UnknownDeleteReview, error) {
	return reviewUnknownDelete(ctx, s.db, id)
}

// Deletes only database rows. All selected rows are checked before any delete,
// and every full original row is journaled in the same transaction. An active
// association cannot be removed, cleared, detached or marked successful here.
func (s *Store) DeleteUnknown(ctx context.Context, selectors []UnknownDeleteSelector, actor string, requireReview bool) ([]UnknownDeleteReceipt, error) {
	if len(selectors) < 1 || len(selectors) > 100 || actor == "" {
		return nil, ErrState
	}
	seen := map[int64]bool{}
	for _, selection := range selectors {
		if selection.ID < 1 || seen[selection.ID] || requireReview && len(selection.Fingerprint) != 64 {
			return nil, ErrState
		}
		seen[selection.ID] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	receipts := []UnknownDeleteReceipt{}
	for _, selection := range selectors {
		review, err := reviewUnknownDelete(ctx, tx, selection.ID)
		if err != nil {
			return nil, err
		}
		if !review.Eligible || selection.Fingerprint != "" && selection.Fingerprint != review.Fingerprint {
			return nil, ErrState
		}
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		receipts = append(receipts, UnknownDeleteReceipt{ID: hex.EncodeToString(nonce[:]), Review: review, Actor: actor, Created: time.Now().UTC().Format(time.RFC3339Nano)})
	}
	for _, receipt := range receipts {
		raw, err := json.Marshal(receipt)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_UNKNOWN_DELETE (ID,RECORD_ID,RECEIPT) VALUES (?,?,?)`, receipt.ID, receipt.Review.Record.ID, string(raw)); err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM TRANSFER_UNKNOWN WHERE ID=?`, receipt.Review.Record.ID)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, ErrState
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return receipts, nil
}
func readUnknownDeleteReceipt(ctx context.Context, reader unknownReader, id string) (UnknownDeleteReceipt, error) {
	var raw, restore string
	var recordID int64
	err := reader.QueryRowContext(ctx, `SELECT RECORD_ID,RECEIPT,RESTORE FROM GO_ORGANIZATION_UNKNOWN_DELETE WHERE ID=?`, id).Scan(&recordID, &raw, &restore)
	if errors.Is(err, sql.ErrNoRows) {
		return UnknownDeleteReceipt{}, ErrNotFound
	}
	if err != nil {
		return UnknownDeleteReceipt{}, err
	}
	var receipt UnknownDeleteReceipt
	if json.Unmarshal([]byte(raw), &receipt) != nil || receipt.ID != id || receipt.Review.Record.ID < 1 || receipt.Review.Record.ID != recordID || !receipt.Review.Eligible || receipt.Review.PendingJobID != "" || receipt.Actor == "" || receipt.Created == "" {
		return receipt, ErrState
	}
	if receipt.Review.Fingerprint != Digest(struct {
		Record  UnknownRawRecord
		Pending string
	}{receipt.Review.Record, ""}) {
		return receipt, ErrState
	}
	if restore != "" {
		receipt.Restore = &UnknownRestoreReceipt{}
		if json.Unmarshal([]byte(restore), receipt.Restore) != nil {
			return receipt, ErrState
		}
		if receipt.Restore.Actor == "" || receipt.Restore.Created == "" {
			return receipt, ErrState
		}
	}
	return receipt, nil
}
func (s *Store) ListUnknownDeletes(ctx context.Context) ([]UnknownDeleteReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_ORGANIZATION_UNKNOWN_DELETE ORDER BY rowid DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := []UnknownDeleteReceipt{}
	for _, id := range ids {
		receipt, err := readUnknownDeleteReceipt(ctx, s.db, id)
		if err != nil {
			return nil, err
		}
		result = append(result, receipt)
	}
	return result, nil
}
func (s *Store) RestoreUnknownDelete(ctx context.Context, id, actor string) (UnknownDeleteReceipt, error) {
	if len(id) != 32 || actor == "" {
		return UnknownDeleteReceipt{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UnknownDeleteReceipt{}, err
	}
	defer tx.Rollback()
	receipt, err := readUnknownDeleteReceipt(ctx, tx, id)
	if err != nil {
		return receipt, err
	}
	if receipt.Restore != nil {
		return receipt, nil
	} // Receipt-only retry, no new row adoption.
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM TRANSFER_UNKNOWN WHERE ID=?`, receipt.Review.Record.ID).Scan(&count); err != nil {
		return receipt, err
	}
	if count != 0 {
		return receipt, ErrState
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE RECORD_ID=? AND STATE='pending'`, receipt.Review.Record.ID).Scan(&count); err != nil {
		return receipt, err
	}
	if count != 0 {
		return receipt, ErrState
	}
	r := receipt.Review.Record
	if _, err = tx.ExecContext(ctx, `INSERT INTO TRANSFER_UNKNOWN (ID,PATH,DEST,MODE,STATE) VALUES (?,?,?,?,?)`, r.ID, r.Path, r.Dest, r.Mode, r.State); err != nil {
		return receipt, err
	}
	receipt.Restore = &UnknownRestoreReceipt{Actor: actor, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, err := json.Marshal(receipt.Restore)
	if err != nil {
		return receipt, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_DELETE SET RESTORE=? WHERE ID=? AND RESTORE=''`, string(raw), id); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}
