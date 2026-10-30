package bus

import (
	"context"
	"time"
)

func (s *Store) WaitEventWebhook(ctx context.Context, expected EventWebhookReceipt) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	p, e := eventHookRead(ctx, tx, expected.ID)
	if e != nil || p.State != "pending" || p.Fingerprint != expected.Fingerprint || p.Definition.Source == nil {
		return ErrEventWebhook
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_EVENT_WEBHOOK_WAITS(ID TEXT PRIMARY KEY,JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,NEXT_CHECK TEXT NOT NULL);CREATE INDEX IF NOT EXISTS GO_EVENT_WEBHOOK_SOURCE_WAIT ON GO_EVENT_WEBHOOK_WAITS(JOB_ID,ORDINAL)`); e != nil {
		return e
	}
	next := time.Now().UTC().Add(30 * time.Second).Format("2006-01-02T15:04:05.000000000Z")
	res, e := tx.ExecContext(ctx, `INSERT INTO GO_EVENT_WEBHOOK_WAITS(ID,JOB_ID,ORDINAL,NEXT_CHECK)VALUES(?,?,?,?) ON CONFLICT(ID)DO UPDATE SET JOB_ID=excluded.JOB_ID,ORDINAL=excluded.ORDINAL,NEXT_CHECK=excluded.NEXT_CHECK`, p.ID, p.Definition.Source.Reference, p.Definition.Source.Ordinal, next)
	if e != nil {
		return e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrEventWebhook
	}
	var job, at string
	var ordinal int
	if e = tx.QueryRowContext(ctx, `SELECT JOB_ID,ORDINAL,NEXT_CHECK FROM GO_EVENT_WEBHOOK_WAITS WHERE ID=?`, p.ID).Scan(&job, &ordinal, &at); e != nil || job != p.Definition.Source.Reference || ordinal != p.Definition.Source.Ordinal || at != next {
		return ErrEventWebhook
	}
	return tx.Commit()
}
