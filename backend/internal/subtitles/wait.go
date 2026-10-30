package subtitles

import (
	"context"
	"time"
)

func (s *Store) Wait(ctx context.Context, expected Receipt) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	p, e := scan(tx.QueryRowContext(ctx, selectSQL+` WHERE ID=?`, expected.ID))
	if e != nil || p.State != "pending" || receiptDigest(p) != receiptDigest(expected) {
		return ErrState
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SUBTITLE_WAITS(ID TEXT PRIMARY KEY,JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,NEXT_CHECK TEXT NOT NULL);CREATE INDEX IF NOT EXISTS GO_SUBTITLE_SOURCE_WAIT ON GO_SUBTITLE_WAITS(JOB_ID,ORDINAL)`); e != nil {
		return e
	}
	next := time.Now().UTC().Add(30 * time.Second).Format("2006-01-02T15:04:05.000000000Z")
	res, e := tx.ExecContext(ctx, `INSERT INTO GO_SUBTITLE_WAITS(ID,JOB_ID,ORDINAL,NEXT_CHECK)VALUES(?,?,?,?) ON CONFLICT(ID)DO UPDATE SET JOB_ID=excluded.JOB_ID,ORDINAL=excluded.ORDINAL,NEXT_CHECK=excluded.NEXT_CHECK`, p.ID, p.JobID, p.Ordinal, next)
	if e != nil {
		return e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var job, at string
	var ordinal int
	if e = tx.QueryRowContext(ctx, `SELECT JOB_ID,ORDINAL,NEXT_CHECK FROM GO_SUBTITLE_WAITS WHERE ID=?`, p.ID).Scan(&job, &ordinal, &at); e != nil || job != p.JobID || ordinal != p.Ordinal || at != next {
		return ErrState
	}
	return tx.Commit()
}
