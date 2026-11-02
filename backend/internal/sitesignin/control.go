package sitesignin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type Control struct {
	Scope               Scope
	Enabled             bool
	NextRun, NextMissed string
	Reason              string
}

func readControl(ctx context.Context, q queryRow) (Control, error) {
	var c Control
	var raw, run, missed string
	var enabled bool
	err := q.QueryRowContext(ctx, `SELECT ENABLED,DOCUMENT,NEXT_RUN,NEXT_MISSED FROM GO_SITE_SIGNIN_CONTROL WHERE ID=1`).Scan(&enabled, &raw, &run, &missed)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if len(raw) > 512<<10 || json.Unmarshal([]byte(raw), &c) != nil || !c.Scope.Valid() || c.Enabled != enabled || c.NextRun != run || c.NextMissed != missed || !sitelogin.Text(c.Reason, 800) {
		return c, ErrState
	}
	for _, value := range []string{run, missed} {
		if value != "" {
			if _, err = time.Parse(time.RFC3339Nano, value); err != nil {
				return c, ErrState
			}
		}
	}
	return c, nil
}
func (s *Store) Control(ctx context.Context) (Control, error) { return readControl(ctx, s.db) }
func controlMatches(ctx context.Context, q queryRow, b Batch) error {
	if b.Trigger == "manual" {
		return nil
	}
	c, err := readControl(ctx, q)
	if err != nil || !c.Enabled || sitelogin.Hash(c.Scope) != sitelogin.Hash(b.Scope) {
		return ErrScope
	}
	return nil
}

// SetControl requires the exact previous document. Old enabled/onlyonce flags
// are not consulted at startup; only this explicitly saved Go control runs.
func (s *Store) SetControl(ctx context.Context, previous Control, next Control, guard func(context.Context) error) error {
	return s.setControl(ctx, previous, next, "", "", false, guard)
}

// Activate consumes the original one-shot flags in the same transaction as Go
// takeover. Unknown original plugin fields are retained by MergeSettings.
func (s *Store) Activate(ctx context.Context, previous, next Control, before, after string, guard func(context.Context) error) error {
	return s.setControl(ctx, previous, next, before, after, true, guard)
}
func (s *Store) setControl(ctx context.Context, previous Control, next Control, before, after string, consume bool, guard func(context.Context) error) error {
	if !next.Scope.Valid() || guard == nil || guard(ctx) != nil {
		return ErrScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actual, err := readControl(ctx, tx)
	if err != nil || sitelogin.Hash(actual) != sitelogin.Hash(previous) {
		return ErrState
	}
	if consume {
		if err = reviewCurrent(ctx, tx, next.Scope); err != nil {
			return err
		}
		if sitelogin.Hash(after) != next.Scope.PluginDigest {
			return ErrScope
		}
		if err = replaceSetting(ctx, tx, "plugin."+Plugin, before, after); err != nil {
			return err
		}
	}
	for _, source := range next.Scope.Sources {
		if err = sourceCurrent(ctx, tx, source); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_CONTROL VALUES(1,?,?,?,?) ON CONFLICT(ID) DO UPDATE SET ENABLED=excluded.ENABLED,DOCUMENT=excluded.DOCUMENT,NEXT_RUN=excluded.NEXT_RUN,NEXT_MISSED=excluded.NEXT_MISSED`, next.Enabled, encoded(next), next.NextRun, next.NextMissed); err != nil {
		return err
	}
	actual, err = readControl(ctx, tx)
	if err != nil || sitelogin.Hash(actual) != sitelogin.Hash(next) || guard(ctx) != nil {
		return ErrScope
	}
	return tx.Commit()
}

// StopControl doesn't require the old external source to remain valid. It is a
// revocation only, and cannot select new sites or initiate a request.
func (s *Store) StopControl(ctx context.Context, id, reason string) error {
	if !sitelogin.Text(reason, 800) {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if c.Scope.ID == "" {
		return nil
	}
	if c.Scope.ID != id {
		return ErrState
	}
	c.Enabled = false
	c.Reason = reason
	if _, err = tx.ExecContext(ctx, `UPDATE GO_SITE_SIGNIN_CONTROL SET ENABLED=0,DOCUMENT=? WHERE ID=1`, encoded(c)); err != nil {
		return err
	}
	actual, err := readControl(ctx, tx)
	if err != nil || encoded(actual) != encoded(c) {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) Advance(ctx context.Context, previous, next Control) error {
	if sitelogin.Hash(previous.Scope) != sitelogin.Hash(next.Scope) || previous.Enabled != next.Enabled {
		return ErrScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := readControl(ctx, tx)
	if err != nil || sitelogin.Hash(c) != sitelogin.Hash(previous) {
		return ErrState
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_SITE_SIGNIN_CONTROL SET DOCUMENT=?,NEXT_RUN=?,NEXT_MISSED=? WHERE ID=1 AND DOCUMENT=?`, encoded(next), next.NextRun, next.NextMissed, encoded(previous)); err != nil {
		return err
	}
	c, err = readControl(ctx, tx)
	if err != nil || encoded(c) != encoded(next) {
		return ErrState
	}
	return tx.Commit()
}
