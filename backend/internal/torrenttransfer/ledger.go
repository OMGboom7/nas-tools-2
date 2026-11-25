package torrenttransfer

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ActionKey identifies the remote effect across schedule runs. Receiver is the
// digest of the downloader endpoint identity, independent of the current grant.
// For add, the remote task does not yet exist and AddedAt must be zero.
type ActionKey struct {
	Receiver string
	WireID   string
	AddedAt  int64
	Action   string
}

func (k ActionKey) valid() bool {
	if !Hex(k.Receiver, 64) || !Hex(k.WireID, 40) {
		return false
	}
	switch k.Action {
	case "add":
		return k.AddedAt == 0
	case "verify", "start", "remove-source":
		return k.AddedAt > 0
	}
	return false
}

// Ledger shares the caller's database. It owns no connection or worker.
type Ledger struct{ db *sql.DB }

func NewLedger(ctx context.Context, db *sql.DB) (*Ledger, error) {
	if db == nil {
		return nil, ErrState
	}
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_TORRENT_TRANSFER_ACTIONS (
		receiver TEXT NOT NULL, wire_id TEXT NOT NULL, added_at INTEGER NOT NULL,
		action TEXT NOT NULL, receipt TEXT NOT NULL, proof TEXT NOT NULL,
		state TEXT NOT NULL CHECK(state IN ('pending','acknowledged','unknown')),
		created TEXT NOT NULL, updated TEXT NOT NULL,
		PRIMARY KEY(receiver,wire_id,added_at,action))`)
	if err != nil {
		return nil, err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_TORRENT_TRANSFER_ADD_REPLIES (
		receiver TEXT NOT NULL, wire_id TEXT NOT NULL, receipt TEXT NOT NULL, duplicate INTEGER NOT NULL,
		created_known INTEGER NOT NULL, PRIMARY KEY(receiver,wire_id))`)
	if err != nil {
		return nil, err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_TORRENT_TRANSFER_TARGETS (
		receiver TEXT NOT NULL, wire_id TEXT NOT NULL, receipt TEXT NOT NULL, task TEXT NOT NULL,
		stage TEXT NOT NULL CHECK(stage IN ('bound','checking','verified')),
		PRIMARY KEY(receiver,wire_id))`)
	if err != nil {
		return nil, err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_TORRENT_TRANSFER_PLANS (
		receipt TEXT PRIMARY KEY NOT NULL, plan TEXT NOT NULL)`)
	if err != nil {
		return nil, err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_TORRENT_TRANSFER_HISTORY (receipt TEXT PRIMARY KEY NOT NULL, history_key TEXT NOT NULL, row_id INTEGER NOT NULL)`)
	if err != nil {
		return nil, err
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_TORRENT_TRANSFER_ABSENCE(receipt TEXT PRIMARY KEY NOT NULL,observed TEXT NOT NULL)`)
	if err != nil {
		return nil, err
	}
	return &Ledger{db}, nil
}

type ActionRecord struct {
	Receipt string
	Proof   string
	State   string
	Created string
	Updated string
}

type actionReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (k ActionKey) read(ctx context.Context, q actionReader) (ActionRecord, error) {
	var row ActionRecord
	err := q.QueryRowContext(ctx, `SELECT receipt,proof,state,created,updated FROM GO_TORRENT_TRANSFER_ACTIONS WHERE receiver=? AND wire_id=? AND added_at=? AND action=?`, k.Receiver, k.WireID, k.AddedAt, k.Action).Scan(&row.Receipt, &row.Proof, &row.State, &row.Created, &row.Updated)
	return row, err
}

func (l *Ledger) Lookup(ctx context.Context, k ActionKey) (ActionRecord, error) {
	if !k.valid() {
		return ActionRecord{}, ErrState
	}
	return k.read(ctx, l.db)
}

// Claim commits intent before returning permission to send. A second caller
// gets the original receipt and send=false even after process interruption.
func (l *Ledger) Claim(ctx context.Context, k ActionKey, receipt, proof string) (row ActionRecord, send bool, err error) {
	if !k.valid() || !Hex(receipt, 64) || !Hex(proof, 64) {
		return row, false, ErrState
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return row, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_TRANSFER_ACTIONS(receiver,wire_id,added_at,action,receipt,proof,state,created,updated) VALUES(?,?,?,?,?,?,'pending',?,?) ON CONFLICT(receiver,wire_id,added_at,action) DO NOTHING`, k.Receiver, k.WireID, k.AddedAt, k.Action, receipt, proof, now, now)
	if err != nil {
		return row, false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return row, false, err
	}
	row, err = k.read(ctx, tx)
	if err != nil {
		return row, false, err
	}
	if n == 1 && (row.Receipt != receipt || row.Proof != proof || row.State != "pending") {
		return row, false, ErrState
	}
	if err := tx.Commit(); err != nil {
		return row, false, err
	}
	return row, n == 1, nil
}

// Finish records only the RPC outcome. Acknowledgment does not establish
// completion of a file check, ownership of an added task, or permission for
// any subsequent action.
func (l *Ledger) Finish(ctx context.Context, k ActionKey, receipt, proof, state string) error {
	return l.finish(ctx, k, receipt, proof, state, nil)
}

func (l *Ledger) finish(ctx context.Context, k ActionKey, receipt, proof, state string, reply *AddReceipt) error {
	if !k.valid() || !Hex(receipt, 64) || !Hex(proof, 64) || state != "acknowledged" && state != "unknown" {
		return ErrState
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE GO_TORRENT_TRANSFER_ACTIONS SET state=?,updated=? WHERE receiver=? AND wire_id=? AND added_at=? AND action=? AND receipt=? AND proof=? AND state='pending'`, state, time.Now().UTC().Format(time.RFC3339Nano), k.Receiver, k.WireID, k.AddedAt, k.Action, receipt, proof)
	if err != nil {
		return err
	}
	row, err := k.read(ctx, tx)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrState
	}
	if err != nil {
		return err
	}
	if row.State != state || row.Receipt != receipt || row.Proof != proof {
		return ErrState
	}
	if reply != nil {
		if k.Action != "add" || state != "acknowledged" || reply.WireID != k.WireID {
			return ErrState
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_TRANSFER_ADD_REPLIES(receiver,wire_id,receipt,duplicate,created_known) VALUES(?,?,?,?,?) ON CONFLICT(receiver,wire_id) DO NOTHING`, k.Receiver, k.WireID, receipt, reply.Duplicate, reply.CreatedKnown)
		if err != nil {
			return err
		}
		stored, err := readAddReply(ctx, tx, k, receipt)
		if err != nil {
			return err
		}
		if stored != *reply {
			return ErrState
		}
	}
	return tx.Commit()
}

func readAddReply(ctx context.Context, q actionReader, k ActionKey, receipt string) (AddReceipt, error) {
	var reply AddReceipt
	err := q.QueryRowContext(ctx, `SELECT wire_id,duplicate,created_known FROM GO_TORRENT_TRANSFER_ADD_REPLIES WHERE receiver=? AND wire_id=? AND receipt=?`, k.Receiver, k.WireID, receipt).Scan(&reply.WireID, &reply.Duplicate, &reply.CreatedKnown)
	return reply, err
}

func (l *Ledger) AddReply(ctx context.Context, k ActionKey, receipt string) (AddReceipt, error) {
	if !k.valid() || k.Action != "add" || !Hex(receipt, 64) {
		return AddReceipt{}, ErrState
	}
	return readAddReply(ctx, l.db, k, receipt)
}
