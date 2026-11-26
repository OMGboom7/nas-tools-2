package downloaderconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/rssreceipt"
)

// The original subscription version and inventory are frozen before contacting
// the downloader. The remote task ID is checkpointed separately from history.
type OrdinaryRSS struct {
	Kind, Token, Mode     string
	ID                    int64
	Season, Rank, Highest int
	Missing, Covered      []int
	Notice                SubscriptionNotice
	Poster, Overview      *string
	Marker                SubscriptionMarker
}
type SubscriptionNotice struct{ Title, Season, Type, Vote, Image, Link string }
type SubscriptionMarker struct{ Name, Enclosure, Type, Title, Year, Season, Episode string }

func reserveOrdinaryRSS(ctx context.Context, tx *sql.Tx, receipt string, h *OrdinaryRSS) error {
	for _, q := range []string{rssreceipt.Schema,
		`CREATE TABLE IF NOT EXISTS GO_ORDINARY_RSS_RECEIPTS(ID TEXT PRIMARY KEY,RESOURCE_KEY TEXT NOT NULL UNIQUE,KIND TEXT NOT NULL,SUB_ID INTEGER NOT NULL,TOKEN TEXT NOT NULL,DOCUMENT TEXT NOT NULL,ACK TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS GO_ORDINARY_RSS_SOURCE ON GO_ORDINARY_RSS_RECEIPTS(KIND,SUB_ID,TOKEN)`,
		`CREATE TABLE IF NOT EXISTS GO_ORDINARY_RSS_CONTINUATIONS(TOKEN TEXT PRIMARY KEY,CURRENT_TOKEN TEXT NOT NULL)`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	key := rssreceipt.Key(h.Marker.Enclosure)
	// An explicit history reset can remove the shared marker while the native
	// subscription still retains its owner/proof. Reusing that key would let
	// native reconciliation mistake this submission for its original task.
	var nativeLedger bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SUBSCRIPTION_DOWNLOAD_CLAIMS')`).Scan(&nativeLedger); err != nil {
		return err
	}
	if nativeLedger {
		var owned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBSCRIPTION_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=?)`, key).Scan(&owned); err != nil {
			return err
		}
		if owned {
			return ErrManualPending
		}
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	r, err := tx.ExecContext(ctx, `INSERT INTO GO_RSS_DOWNLOAD_CLAIMS(RESOURCE_KEY,TASK_ID,STATE,UPDATED_AT)
	 SELECT ?,0,'pending',? WHERE NOT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?) ON CONFLICT DO NOTHING`, key, time.Now().UTC().Format(time.RFC3339Nano), h.Marker.Enclosure)
	if err != nil {
		return err
	}
	if n, err := r.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_ORDINARY_RSS_RECEIPTS(ID,RESOURCE_KEY,KIND,SUB_ID,TOKEN,DOCUMENT) VALUES(?,?,?,?,?,?)`, receipt, key, h.Kind, h.ID, h.Token, string(raw))
	return err
}

func releaseOrdinaryRSS(ctx context.Context, tx *sql.Tx, r ManualSubmission) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM GO_ORDINARY_RSS_RECEIPTS WHERE ID=? AND RESOURCE_KEY=? AND ACK=''`, r.ID, r.Definition.SubscriptionKey)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND TASK_ID=0 AND STATE='pending'`, r.Definition.SubscriptionKey)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	return nil
}

func (s *Store) AcknowledgeOrdinaryRSS(ctx context.Context, r ManualSubmission, task string) error {
	if task == "" || len(task) > 128 {
		return ErrManualPending
	}
	result, err := s.database.ExecContext(ctx, `UPDATE GO_ORDINARY_RSS_RECEIPTS SET ACK=? WHERE ID=? AND RESOURCE_KEY=? AND (ACK='' OR ACK=?) AND EXISTS(SELECT 1 FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS WHERE ID=? AND STATE='pending' AND DOCUMENT=?)`, task, r.ID, r.Definition.SubscriptionKey, task, r.ID, manualDocument(r))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	return nil
}

func (s *Store) RestoreOrdinaryRSS(ctx context.Context, id, actor string) (ManualSubmission, error) {
	r, err := s.ManualSubmission(ctx, id)
	if err != nil {
		return r, err
	}
	if r.Definition.SubscriptionKey == "" {
		return r, ErrManualPending
	}
	if r.State == "submitted" {
		return r, nil
	}
	var task string
	if err = s.database.QueryRowContext(ctx, `SELECT ACK FROM GO_ORDINARY_RSS_RECEIPTS WHERE ID=? AND RESOURCE_KEY=? AND ACK!=''`, id, r.Definition.SubscriptionKey).Scan(&task); err != nil {
		return r, err
	}
	h, err := s.ClassicPendingHistory(ctx, id)
	if err != nil {
		return r, err
	}
	if actor == "" {
		actor = r.Definition.Actor
	}
	if err = s.CompleteWithHistory(ctx, r, task, "acknowledged", actor, &h); err != nil {
		return r, err
	}
	return s.ManualSubmission(ctx, id)
}
