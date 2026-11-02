package sitesignin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type RemoteState struct {
	Source  string `json:"source"`
	Scope   string `json:"scope"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Updated string `json:"updated"`
	Offset  int64  `json:"offset"`
}

func (s *Store) Incoming(ctx context.Context, scope Scope, source, key, user string, channelID int64, raw json.RawMessage, data any, command bool, guard func(context.Context) error) error {
	if !scope.Options.Remote || !sitelogin.Hex(source, 64) || len(key) > 4096 || key == "" || len(raw) > 256<<10 || !json.Valid(raw) || guard == nil || guard(ctx) != nil {
		return ErrScope
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	current, e := readControl(ctx, tx)
	if e != nil || !current.Enabled || sitelogin.Hash(current.Scope) != sitelogin.Hash(scope) {
		return ErrScope
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_INBOX(ID TEXT PRIMARY KEY,SOURCE TEXT NOT NULL,SCOPE_ID TEXT NOT NULL,STATE TEXT NOT NULL,DOCUMENT TEXT NOT NULL,CREATED TEXT NOT NULL)`); e != nil {
		return e
	}
	id := sitelogin.Hash([]string{source, key})
	state := "unhandled"
	if command {
		state = "queued"
	}
	result, e := tx.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_INBOX VALUES(?,?,?,?,?,?) ON CONFLICT(ID) DO NOTHING`, id, source, scope.ID, state, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n > 0 {
		if e = bus.PublishSignal(ctx, tx, "message.incoming", id, source, data); e != nil {
			return e
		}
		if command {
			if e = bus.PublishSignal(ctx, tx, "site.signin", id, source, map[string]any{"command": "/pts", "origin": source}); e != nil {
				return e
			}
			if e = bus.EmitInteractiveReply(ctx, tx, id, id, source, user, channelID); e != nil {
				return e
			}
		}
	}
	if guard(ctx) != nil {
		return ErrScope
	}
	return tx.Commit()
}
func (s *Store) Remote(ctx context.Context, source string) (RemoteState, error) {
	r := RemoteState{Source: source}
	exists, e := tableExists(ctx, s.db, "GO_SITE_SIGNIN_REMOTE")
	if e != nil || !exists {
		return r, e
	}
	var raw string
	e = s.db.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_SITE_SIGNIN_REMOTE WHERE SOURCE=?`, source).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return r, nil
	}
	if e != nil {
		return r, e
	}
	if json.Unmarshal([]byte(raw), &r) != nil || r.Source != source {
		return r, ErrState
	}
	return r, nil
}
func (s *Store) SetRemote(ctx context.Context, r RemoteState) error {
	if !sitelogin.Hex(r.Source, 64) || !sitelogin.Hex(r.Scope, 64) || !sitelogin.Text(r.State, 60) || !sitelogin.Text(r.Reason, 800) || r.Offset < 0 {
		return ErrState
	}
	if _, e := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_REMOTE(SOURCE TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL)`); e != nil {
		return e
	}
	r.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	_, e := s.db.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_REMOTE VALUES(?,?) ON CONFLICT(SOURCE) DO UPDATE SET DOCUMENT=excluded.DOCUMENT`, r.Source, encoded(r))
	return e
}
