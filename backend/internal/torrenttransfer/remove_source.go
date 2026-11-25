package torrenttransfer

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// ProtectedSource uses the application's PT and profile checks and retains all
// local files. The raw protocol clients intentionally do not implement this.
type ProtectedSource interface {
	SourceClient
	CheckTransferRemoval(context.Context, Task) error
	RemoveTransferKeepingFiles(context.Context, Task) error
}

func (l *Ledger) RemoveSource(ctx context.Context, receipt string, resolve ResolveScope, authorize func(context.Context, string, Task, Task) error) error {
	if resolve == nil || authorize == nil {
		return ErrState
	}
	return l.removeOrObserveSource(ctx, receipt, resolve, authorize)
}

// ConfirmSourceAbsent cannot send a removal, even if no action was recorded.
func (l *Ledger) ConfirmSourceAbsent(ctx context.Context, receipt string, resolve ResolveScope) error {
	if resolve == nil {
		return ErrState
	}
	return l.removeOrObserveSource(ctx, receipt, resolve, nil)
}

func (l *Ledger) removeOrObserveSource(ctx context.Context, receipt string, resolve ResolveScope, authorize func(context.Context, string, Task, Task) error) error {
	p, err := l.LoadPlan(ctx, receipt)
	if err != nil {
		return err
	}
	settings, src, dst, grant, err := resolve(ctx)
	if err != nil {
		return err
	}
	client, ok := src.(ProtectedSource)
	if !ok || dst == nil || settings.Ready() != nil || !settings.DeleteSource || grant != p.GrantDigest || Hash(settings) != p.Source.SettingsDigest || Hash(client.Address()) != p.Source.Endpoint || Hash(dst.Address()) != p.Receiver {
		return ErrState
	}
	key := ActionKey{Receiver: p.Source.Endpoint, WireID: p.Source.Task.WireID, AddedAt: p.Source.Task.AddedAt, Action: "remove-source"}
	intent := AddIntent{Ledger: l, Receipt: receipt, Receiver: p.Receiver, WireID: p.WireID}
	frozen, stage, err := intent.target(ctx)
	if err != nil {
		return err
	}
	if stage != "verified" {
		return ErrState
	}
	proof := Hash([]string{Hash(p), Hash(frozen)})
	action, err := l.Lookup(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		if authorize == nil {
			return ErrState
		}
		review, receiver, err := p.live(ctx, resolve)
		if err != nil {
			return err
		}
		target, err := receiver.TransferTask(ctx, p.WireID)
		if err != nil {
			return err
		}
		if !target.SameInstance(frozen) || !target.FullComplete(review.Source.Prepared) {
			return ErrState
		}
		if settings.AutoStart {
			startKey := ActionKey{Receiver: p.Receiver, WireID: p.WireID, AddedAt: frozen.AddedAt, Action: "start"}
			start, err := l.Lookup(ctx, startKey)
			if err != nil {
				return err
			}
			if start.State != "acknowledged" || start.Receipt != receipt || !target.SourceSeeding {
				return ErrState
			}
		}
		if err := authorize(ctx, receipt, review.Task, target); err != nil {
			return err
		}
		if err := l.sourceHistory(ctx, p, settings, receipt, false); err != nil {
			return err
		}
		intent.Revalidate = func(ctx context.Context) error {
			_, current, err := p.live(ctx, resolve)
			if err != nil {
				return err
			}
			snapshot, err := current.TransferTask(ctx, p.WireID)
			if err != nil {
				return err
			}
			if !snapshot.SameInstance(frozen) || !snapshot.FullComplete(review.Source.Prepared) || settings.AutoStart && !snapshot.SourceSeeding {
				return ErrState
			}
			if err := client.CheckTransferRemoval(ctx, review.Task); err != nil {
				return err
			}
			return authorize(ctx, receipt, review.Task, snapshot)
		}
		state, err := intent.targetAction(ctx, key, proof, nil, func() error { return client.RemoveTransferKeepingFiles(ctx, review.Task) })
		if err != nil {
			return err
		}
		if state != "acknowledged" {
			return ErrState
		}
	} else if err != nil {
		return err
	} else if action.Receipt != receipt || action.Proof != proof || (action.State != "acknowledged" && (authorize != nil || (action.State != "pending" && action.State != "unknown"))) {
		return ErrState
	}
	target, err := dst.TransferTask(ctx, p.WireID)
	if err != nil {
		return err
	}
	if !target.SameInstance(frozen) || target.Error || !target.Complete {
		return ErrState
	}
	if _, err := client.TransferTask(ctx, key.WireID); !errors.Is(err, ErrTaskAbsent) {
		if err != nil {
			return err
		}
		return ErrState
	}
	current, currentSource, currentTarget, currentGrant, err := resolve(ctx)
	if err != nil {
		return err
	}
	if currentGrant != grant || Hash(current) != p.Source.SettingsDigest || currentSource == nil || currentTarget == nil || Hash(currentSource.Address()) != p.Source.Endpoint || Hash(currentTarget.Address()) != p.Receiver {
		return ErrState
	}
	return l.sourceHistory(ctx, p, settings, receipt, true)
}

func (l *Ledger) sourceHistory(ctx context.Context, p Plan, s Settings, receipt string, removed bool) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	var key, raw string
	err = tx.QueryRowContext(ctx, `SELECT h.ID,h.KEY,h.VALUE FROM GO_TORRENT_TRANSFER_HISTORY m JOIN PLUGIN_HISTORY h ON h.ID=m.row_id AND h.KEY=m.history_key WHERE m.receipt=? AND h.PLUGIN_ID=?`, receipt, Plugin).Scan(&id, &key, &raw)
	if err != nil {
		return err
	}
	to, err := strconv.ParseInt(s.To[0], 10, 64)
	if err != nil || key != s.From[0]+"-"+p.Source.Task.WireID {
		return ErrState
	}
	before, err := historyValue(to, p.WireID, false)
	if err != nil {
		return err
	}
	after, err := historyValue(to, p.WireID, true)
	if err != nil {
		return err
	}
	if raw != before && !(removed && raw == after) {
		return ErrState
	}
	if !removed {
		return nil
	}
	if raw != after {
		result, err := tx.ExecContext(ctx, "UPDATE PLUGIN_HISTORY SET VALUE=? WHERE ID=? AND VALUE=? AND KEY=? AND PLUGIN_ID=?", after, id, before, key, Plugin)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrState
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_TRANSFER_ABSENCE(receipt,observed) VALUES(?,?) ON CONFLICT(receipt) DO NOTHING`, receipt, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if err := completeTarget(ctx, tx, receipt, to); err != nil {
		return err
	}
	return tx.Commit()
}
