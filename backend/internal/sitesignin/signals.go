package sitesignin

import (
	"context"
	"encoding/json"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type Signal struct {
	ID, Reference, State, Reason, Created, Batch string
	Scope                                        Scope
}
type Dependency struct {
	Batch  string `json:"batch"`
	Plugin string `json:"plugin"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func (s *Store) Publish(ctx context.Context, scope Scope, key, reference string, data any, guard func(context.Context) error) error {
	if guard == nil || guard(ctx) != nil {
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
	if e = bus.PublishSignal(ctx, tx, "site.signin", key, reference, data); e != nil {
		return e
	}
	if guard(ctx) != nil {
		return ErrScope
	}
	return tx.Commit()
}
func tableExists(ctx context.Context, q queryRow, name string) (bool, error) {
	var yes bool
	e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&yes)
	return yes, e
}
func (s *Store) Signals(ctx context.Context, pending bool) ([]Signal, error) {
	out := []Signal{}
	exists, e := tableExists(ctx, s.db, "GO_NATIVE_SIGNAL_JOBS")
	if e != nil || !exists {
		return out, e
	}
	query := `SELECT ID,REFERENCE,SCOPE_ID,SOURCE,STATE,REASON,CREATED,BATCH_ID FROM GO_NATIVE_SIGNAL_JOBS WHERE TYPE='site.signin'`
	if pending {
		query += ` AND STATE='queued'`
	}
	query += ` ORDER BY CREATED DESC,ID LIMIT 30`
	rows, e := s.db.QueryContext(ctx, query)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var r Signal
		var source, scopeID string
		if e = rows.Scan(&r.ID, &r.Reference, &scopeID, &source, &r.State, &r.Reason, &r.Created, &r.Batch); e != nil {
			return nil, e
		}
		if json.Unmarshal([]byte(source), &r.Scope) != nil || !r.Scope.Valid() || r.Scope.ID != scopeID || !sitelogin.Hex(r.ID, 64) {
			return nil, ErrState
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) ResolveSignal(ctx context.Context, original Signal, state, reason, batch string) error {
	if state != "handled" && state != "blocked" || !sitelogin.Text(reason, 800) || batch != "" && !sitelogin.Hex(batch, 64) {
		return ErrState
	}
	res, e := s.db.ExecContext(ctx, `UPDATE GO_NATIVE_SIGNAL_JOBS SET STATE=?,REASON=?,BATCH_ID=? WHERE ID=? AND STATE='queued' AND SCOPE_ID=? AND SOURCE=?`, state, reason, batch, original.ID, original.Scope.ID, encoded(original.Scope))
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return ErrState
	}
	return nil
}
func (s *Store) Dependency(ctx context.Context, batch, plugin, state, reason string) error {
	validState := false
	switch state {
	case "queued", "running", "applying", "complete", "failed", "blocked", "unknown", "closed":
		validState = true
	}
	if !sitelogin.Hex(batch, 64) || plugin != "CloudflareSpeedTest" || !validState || !sitelogin.Text(reason, 800) {
		return ErrState
	}
	_, e := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_DEPENDENCIES(BATCH_ID TEXT PRIMARY KEY,PLUGIN TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,CREATED TEXT NOT NULL)`)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_DEPENDENCIES VALUES(?,?,?,?,?) ON CONFLICT(BATCH_ID) DO NOTHING`, batch, plugin, state, reason, time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
func (s *Store) Dependencies(ctx context.Context) ([]Dependency, error) {
	out := []Dependency{}
	exists, e := tableExists(ctx, s.db, "GO_SITE_SIGNIN_DEPENDENCIES")
	if e != nil || !exists {
		return out, e
	}
	rows, e := s.db.QueryContext(ctx, `SELECT BATCH_ID,PLUGIN,STATE,REASON FROM GO_SITE_SIGNIN_DEPENDENCIES ORDER BY CREATED DESC LIMIT 30`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var d Dependency
		if e = rows.Scan(&d.Batch, &d.Plugin, &d.State, &d.Reason); e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
