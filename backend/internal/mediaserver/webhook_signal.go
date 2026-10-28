package mediaserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
)

// A raw callback is a different event from its optional notification projection.
// The original JSON is snapshotted only by an explicitly authorized subscriber;
// the receiver's duplicate ledger retains only identity, digest and receipt time.
func WebhookSignalIdentity(raw []byte) (string, error) {
	data, err := webhookJSON(raw)
	if err != nil {
		return "", err
	}
	for _, key := range []string{"EventId", "NotificationId"} {
		value, err := webhookText(data[key], 512)
		if err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
	}
	return "", nil
}

func recordWebhookSignal(ctx context.Context, tx *sql.Tx, c WebhookControl, id, digest, received string, raw json.RawMessage) (bool, error) {
	actual, err := WebhookPayloadDigest(c.Server, raw)
	if err != nil || actual != digest || !refreshHex(id, 32) || !refreshHex(digest, 64) {
		return false, ErrWebhookState
	}
	if _, err := time.Parse(time.RFC3339Nano, received); err != nil {
		return false, ErrWebhookState
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_MEDIA_WEBHOOK_SIGNALS(ID TEXT PRIMARY KEY,REVISION TEXT NOT NULL,DIGEST TEXT NOT NULL,CREATED TEXT NOT NULL)`); err != nil {
		return false, err
	}
	var revision, previous string
	err = tx.QueryRowContext(ctx, `SELECT REVISION,DIGEST FROM GO_MEDIA_WEBHOOK_SIGNALS WHERE ID=?`, id).Scan(&revision, &previous)
	if err == nil {
		if revision != c.Revision || previous != digest {
			return false, ErrWebhookState
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_WEBHOOK_SIGNALS(ID,REVISION,DIGEST,CREATED) VALUES(?,?,?,?)`, id, c.Revision, digest, received)
	if err != nil {
		return false, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return false, ErrWebhookState
	}
	var savedTime string
	err = tx.QueryRowContext(ctx, `SELECT REVISION,DIGEST,CREATED FROM GO_MEDIA_WEBHOOK_SIGNALS WHERE ID=?`, id).Scan(&revision, &previous, &savedTime)
	if err != nil || revision != c.Revision || previous != digest || savedTime != received {
		return false, ErrWebhookState
	}
	if err := bus.PublishSignal(ctx, tx, c.Server+".webhook", id, "media-webhook:"+id, raw); err != nil {
		return false, err
	}
	if err := speedlimit.Capture(ctx, tx, c.Server, id, raw); err != nil {
		return false, err
	}
	if err := mediasyncdel.Capture(ctx, tx, c.Server, id, raw); err != nil {
		return false, err
	}
	return false, nil
}

// ReceiveSignal accepts a valid callback that has no notification projection.
// It shares receiver authority, payload deduplication and the event transaction;
// it never fabricates a user notification or contacts the media server.
func (s *WebhookStore) ReceiveSignal(ctx context.Context, c WebhookControl, id, digest string, raw json.RawMessage, validate func() error) (bool, error) {
	if !c.Enabled || validate == nil {
		return false, ErrWebhookState
	}
	actual, err := WebhookPayloadDigest(c.Server, raw)
	if err != nil || actual != digest || !refreshHex(id, 32) {
		return false, ErrWebhookState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	current, err := webhookControl(ctx, tx)
	if err != nil || current != c {
		return false, ErrWebhookState
	}
	if err := validate(); err != nil {
		return false, err
	}
	// Earlier receiver versions saved notification receipts without a raw
	// signal ledger. That original identity still owns the callback: neither a
	// newly enabled subscriber nor a changed action may adopt it as a new fact.
	previous, revision, original, err := readWebhookReceipt(ctx, tx, id)
	if err == nil {
		if revision != c.Revision || original != digest {
			return false, ErrWebhookState
		}
		if err := verifyWebhookBusinessEvent(ctx, tx, previous); err != nil {
			return false, err
		}
		if err := validate(); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if !errors.Is(err, ErrWebhookMissing) {
		return false, err
	}
	duplicate, err := recordWebhookSignal(ctx, tx, c, id, digest, time.Now().UTC().Format(time.RFC3339Nano), raw)
	if err != nil {
		return false, err
	}
	current, err = webhookControl(ctx, tx)
	if err != nil || current != c {
		return false, ErrWebhookState
	}
	if err := validate(); err != nil {
		return false, err
	}
	return duplicate, tx.Commit()
}
