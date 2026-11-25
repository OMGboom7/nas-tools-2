package torrenttransfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

type TargetClient interface {
	Address() string
	TransferTask(context.Context, string) (Task, error)
	RecheckTransfer(context.Context, Task, torrentmeta.TransferMetadata) error
	StartTransfer(context.Context, Task, torrentmeta.TransferMetadata) error
}

// BindTarget requires the caller to establish ownership of this incarnation.
// A qB acknowledgment alone cannot distinguish a new task from a duplicate.
func (i AddIntent) BindTarget(ctx context.Context, c TargetClient, m torrentmeta.TransferMetadata, confirmOwnership func(context.Context, AddReceipt, Task) error) error {
	if err := i.checkTarget(ctx, c, m); err != nil {
		return err
	}
	key := ActionKey{Receiver: i.Receiver, WireID: i.WireID, Action: "add"}
	reply, err := i.Ledger.AddReply(ctx, key, i.Receipt)
	if err != nil {
		return err
	}
	if reply.Duplicate || confirmOwnership == nil {
		return ErrState
	}
	task, err := c.TransferTask(ctx, i.WireID)
	if err != nil {
		return err
	}
	dir, err := NormalizeRemote(task.Directory)
	if err != nil || dir != i.Directory || task.WireID != i.WireID || !task.Matches(m) || task.Error || task.AutoManaged || (!task.Paused && !task.Checking) || !slices.Contains(task.Labels, "转移做种") || !slices.Contains(task.Labels, "已整理") {
		return ErrState
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	if err := confirmOwnership(ctx, reply, task); err != nil {
		return err
	}
	_, err = i.Ledger.db.ExecContext(ctx, `INSERT INTO GO_TORRENT_TRANSFER_TARGETS(receiver,wire_id,receipt,task,stage) VALUES(?,?,?,?,'bound') ON CONFLICT(receiver,wire_id) DO NOTHING`, i.Receiver, i.WireID, i.Receipt, string(raw))
	if err != nil {
		return err
	}
	frozen, _, err := i.target(ctx)
	if err != nil {
		return err
	}
	if !frozen.SameInstance(task) {
		return ErrState
	}
	return nil
}

func (i AddIntent) checkTarget(ctx context.Context, c TargetClient, m torrentmeta.TransferMetadata) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if c == nil || Hash(c.Address()) != i.Receiver || contentDigest(m.Content) != i.ContentDigest {
		return ErrState
	}
	action, err := i.Ledger.Lookup(ctx, ActionKey{Receiver: i.Receiver, WireID: i.WireID, Action: "add"})
	if err != nil {
		return err
	}
	if action.State != "acknowledged" || action.Receipt != i.Receipt || action.Proof != Hash([]string{i.ScopeProof, i.Directory, i.ContentDigest}) {
		return ErrState
	}
	return i.Revalidate(ctx)
}

func (i AddIntent) target(ctx context.Context) (Task, string, error) {
	var raw, stage string
	err := i.Ledger.db.QueryRowContext(ctx, `SELECT task,stage FROM GO_TORRENT_TRANSFER_TARGETS WHERE receiver=? AND wire_id=? AND receipt=?`, i.Receiver, i.WireID, i.Receipt).Scan(&raw, &stage)
	if err != nil {
		return Task{}, "", err
	}
	var task Task
	if json.Unmarshal([]byte(raw), &task) != nil || !task.Valid() || task.WireID != i.WireID {
		return Task{}, "", ErrState
	}
	return task, stage, nil
}

// AdvanceTarget observes or issues at most one mutation per call. Read the
// durable stage before sampling the daemon so an older snapshot cannot prove
// completion of a check observed by a concurrent worker.
func (i AddIntent) AdvanceTarget(ctx context.Context, c TargetClient, m torrentmeta.TransferMetadata, allowStart bool) (string, error) {
	if err := i.checkTarget(ctx, c, m); err != nil {
		return "", err
	}
	frozen, stage, err := i.target(ctx)
	if err != nil {
		return "", err
	}
	key := ActionKey{Receiver: i.Receiver, WireID: i.WireID, AddedAt: frozen.AddedAt, Action: "verify"}
	proof := Hash([]string{i.Receipt, i.ScopeProof, i.ContentDigest, Hash(frozen)})
	action, err := i.Ledger.Lookup(ctx, key)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return "", err
	}
	if !missing && (action.Receipt != i.Receipt || action.Proof != proof || action.State != "acknowledged") {
		return action.State, ErrState
	}
	task, err := c.TransferTask(ctx, i.WireID)
	if err != nil {
		return stage, err
	}
	if !frozen.SameInstance(task) || !task.Matches(m) || task.Error {
		return stage, ErrState
	}
	if missing {
		if stage != "bound" || !task.Paused {
			return stage, nil
		}
		return i.targetAction(ctx, key, proof, func() error { return c.RecheckTransfer(ctx, task, m) })
	}
	next := stage
	switch stage {
	case "bound":
		if task.Checking {
			next = "checking"
		}
	case "checking":
		if task.Paused && task.FullComplete(m) {
			next = "verified"
		}
	case "verified":
		key.Action = "start"
		start, err := i.Ledger.Lookup(ctx, key)
		if err == nil {
			if start.Receipt != i.Receipt || start.Proof != proof {
				return stage, ErrState
			}
			return start.State, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return stage, err
		}
		if allowStart && task.Paused && task.HasTracker && task.FullComplete(m) {
			return i.targetAction(ctx, key, proof, func() error { return c.StartTransfer(ctx, task, m) })
		}
	default:
		return "", ErrState
	}
	if next != stage {
		result, err := i.Ledger.db.ExecContext(ctx, `UPDATE GO_TORRENT_TRANSFER_TARGETS SET stage=? WHERE receiver=? AND wire_id=? AND receipt=? AND stage=?`, next, i.Receiver, i.WireID, i.Receipt, stage)
		if err != nil {
			return stage, err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return stage, ErrState
		}
	}
	return next, nil
}

func (i AddIntent) targetAction(ctx context.Context, k ActionKey, proof string, send func() error) (string, error) {
	if err := i.Revalidate(ctx); err != nil {
		return "", err
	}
	row, won, err := i.Ledger.Claim(ctx, k, i.Receipt, proof)
	if err != nil {
		return "", err
	}
	if !won {
		if row.Receipt != i.Receipt || row.Proof != proof {
			return "", ErrState
		}
		return row.State, nil
	}
	err = send()
	state := "acknowledged"
	if err != nil {
		state = "unknown"
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return state, errors.Join(err, i.Ledger.Finish(save, k, i.Receipt, proof, state))
}
