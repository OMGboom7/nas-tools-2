package sitesignin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type Review struct {
	ID, Mode, Expires, BeforePluginDigest, PreviousControlDigest string
	Scope                                                        Scope
}

func (r Review) Valid() bool {
	if !sitelogin.Hex(r.ID, 64) || r.Scope.ID != r.ID || !r.Scope.Valid() || !sitelogin.Hex(r.BeforePluginDigest, 64) || !sitelogin.Hex(r.PreviousControlDigest, 64) || r.Mode != "manual" && r.Mode != "activate" {
		return false
	}
	created, e := time.Parse(time.RFC3339Nano, r.Scope.Created)
	if e != nil {
		return false
	}
	end, e := time.Parse(time.RFC3339Nano, r.Expires)
	return e == nil && end.After(created) && end.Sub(created) <= 10*time.Minute
}
func (s *Store) SaveReview(ctx context.Context, r Review, guard func(context.Context) error) error {
	if !r.Valid() || guard == nil || guard(ctx) != nil {
		return ErrScope
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_REVIEWS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL)`); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_REVIEW_SITES(SITE_ID INTEGER PRIMARY KEY,REVIEW_ID TEXT NOT NULL)`); e != nil {
		return e
	}
	for _, source := range r.Scope.Sources {
		if e = sourceCurrent(ctx, tx, source); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_REVIEWS VALUES(?,?)`, r.ID, encoded(r)); e != nil {
		return e
	}
	for _, source := range r.Scope.Sources {
		if _, e = tx.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_REVIEW_SITES VALUES(?,?) ON CONFLICT(SITE_ID) DO UPDATE SET REVIEW_ID=excluded.REVIEW_ID`, source.SiteID, r.ID); e != nil {
			return e
		}
	}
	var raw string
	if e = tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_SITE_SIGNIN_REVIEWS WHERE ID=?`, r.ID).Scan(&raw); e != nil || raw != encoded(r) || guard(ctx) != nil {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) Review(ctx context.Context, id string) (Review, error) {
	var r Review
	if !sitelogin.Hex(id, 64) {
		return r, ErrState
	}
	var raw string
	if e := s.db.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_SITE_SIGNIN_REVIEWS WHERE ID=?`, id).Scan(&raw); e != nil {
		return r, e
	}
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &r) != nil || !r.Valid() || r.ID != id {
		return r, ErrState
	}
	return r, nil
}
func reviewCurrent(ctx context.Context, q queryRow, scope Scope) error {
	for _, source := range scope.Sources {
		var id string
		if e := q.QueryRowContext(ctx, `SELECT REVIEW_ID FROM GO_SITE_SIGNIN_REVIEW_SITES WHERE SITE_ID=?`, source.SiteID).Scan(&id); e != nil || id != scope.ID {
			return ErrScope
		}
	}
	return nil
}
func (s *Store) ReviewCurrent(ctx context.Context, scope Scope) error {
	return reviewCurrent(ctx, s.db, scope)
}
func setting(ctx context.Context, q queryRow, key string) (string, error) {
	var raw string
	e := q.QueryRowContext(ctx, `SELECT COALESCE(VALUE,'') FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return raw, e
}
func replaceSetting(ctx context.Context, tx *sql.Tx, key, before, after string) error {
	actual, e := setting(ctx, tx, key)
	if e != nil || actual != before {
		return ErrScope
	}
	var id int64
	e = tx.QueryRowContext(ctx, `SELECT ID FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		_, e = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig',?,?)`, key, after)
	} else if e == nil {
		_, e = tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE ID=?`, after, id)
	}
	if e != nil {
		return e
	}
	actual, e = setting(ctx, tx, key)
	if e != nil || actual != after {
		return ErrState
	}
	return nil
}
