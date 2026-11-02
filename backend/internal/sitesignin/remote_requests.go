package sitesignin

import (
	"context"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

func (s *Store) RemoteIntent(ctx context.Context, scope Scope, source, kind string, guard func(context.Context) error) (string, error) {
	if !sitelogin.Hex(source, 64) || !sitelogin.Text(kind, 60) || guard == nil || guard(ctx) != nil {
		return "", ErrScope
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	c, e := readControl(ctx, tx)
	if e != nil || !c.Enabled || sitelogin.Hash(c.Scope) != sitelogin.Hash(scope) {
		return "", ErrScope
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_REMOTE_REQUESTS(ID TEXT PRIMARY KEY,SCOPE_ID TEXT NOT NULL,SOURCE TEXT NOT NULL,KIND TEXT NOT NULL,STATE TEXT NOT NULL,STATUS INTEGER NOT NULL,CREATED TEXT NOT NULL)`); e != nil {
		return "", e
	}
	id := sitelogin.Random()
	if id == "" {
		return "", ErrState
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_REMOTE_REQUESTS VALUES(?,?,?,?,'intent',0,?)`, id, scope.ID, source, kind, time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
		return "", e
	}
	if guard(ctx) != nil {
		return "", ErrScope
	}
	return id, tx.Commit()
}

// Called under the receiver's process guard; creating an unknown socket ticket
// is not automatically repeated under the same old grant after a crash.
func (s *Store) UnconfirmedRemoteConnect(ctx context.Context, scope, source string) (bool, error) {
	exists, e := tableExists(ctx, s.db, "GO_SITE_SIGNIN_REMOTE_REQUESTS")
	if e != nil || !exists {
		return false, e
	}
	var count int
	e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_SITE_SIGNIN_REMOTE_REQUESTS WHERE SCOPE_ID=? AND SOURCE=? AND KIND='slack-connect' AND STATE IN('intent','unknown')`, scope, source).Scan(&count)
	return count > 0, e
}
func (s *Store) RemoteReceived(ctx context.Context, id, state string, status int) error {
	if !sitelogin.Hex(id, 64) || state != "received" && state != "unknown" {
		return ErrState
	}
	r, e := s.db.ExecContext(ctx, `UPDATE GO_SITE_SIGNIN_REMOTE_REQUESTS SET STATE=?,STATUS=? WHERE ID=? AND STATE='intent'`, state, status, id)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil || n != 1 {
		return ErrState
	}
	return nil
}
