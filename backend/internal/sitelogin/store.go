package sitelogin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS GO_SITE_LOGIN_BATCHES(ID TEXT PRIMARY KEY,ACTOR TEXT NOT NULL,DOCUMENT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SITE_LOGIN_JOBS(ID TEXT PRIMARY KEY,BATCH_ID TEXT NOT NULL,ACTOR TEXT NOT NULL,SITE_ID INTEGER NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,DOCUMENT TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS GO_SITE_LOGIN_HISTORY ON GO_SITE_LOGIN_JOBS(CREATED DESC,ID)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS GO_SITE_LOGIN_ACTIVE_SITE ON GO_SITE_LOGIN_JOBS(SITE_ID) WHERE STATE IN('queued','running','waiting_input')`,
	} {
		if _, err = db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	// Most service instances never request site login. Do not retain an idle
	// SQLite descriptor for each instance; the DSN reinstalls busy_timeout on
	// every subsequent connection, including after an idle close.
	db.SetMaxIdleConns(0)
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSite(ctx context.Context, q rowReader, id int64) (siteconfig.Site, error) {
	var v siteconfig.Site
	err := q.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(PRI,''),COALESCE(RSSURL,''),COALESCE(SIGNURL,''),COALESCE(COOKIE,''),COALESCE(APIKEY,''),COALESCE(INCLUDE,''),COALESCE(EXCLUDE,''),COALESCE(SIZE,''),COALESCE(NOTE,'') FROM CONFIG_SITE WHERE ID=?`, id).Scan(&v.ID, &v.Name, &v.Priority, &v.RSSURL, &v.SignURL, &v.Cookie, &v.APIKey, &v.Include, &v.Exclude, &v.Size, &v.Note)
	return v, err
}
func read(ctx context.Context, q rowReader, id string) (Record, error) {
	var raw, state, updated, batch, actor, created string
	var site int64
	if err := q.QueryRowContext(ctx, "SELECT DOCUMENT,STATE,UPDATED,SITE_ID,BATCH_ID,ACTOR,CREATED FROM GO_SITE_LOGIN_JOBS WHERE ID=?", id).Scan(&raw, &state, &updated, &site, &batch, &actor, &created); err != nil {
		return Record{}, err
	}
	var r Record
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &r) != nil || !r.Valid() || r.State != state || r.Updated != updated || r.Source.SiteID != site || r.ID != id || r.Batch != batch || r.Actor != actor || r.Created != created {
		return r, ErrState
	}
	return r, nil
}
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	if !Hex(id, 64) {
		return Record{}, ErrState
	}
	return read(ctx, s.db, id)
}
func (s *Store) List(ctx context.Context, page int) ([]Record, error) {
	if page < 1 || page > 100000 {
		return nil, ErrState
	}
	rows, err := s.db.QueryContext(ctx, "SELECT ID FROM GO_SITE_LOGIN_JOBS ORDER BY CREATED DESC,ID LIMIT 30 OFFSET ?", (page-1)*30)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, id := range ids {
		r, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
func (s *Store) Create(ctx context.Context, batch Batch, records []Record, guard Guard) ([]Record, bool, error) {
	if guard == nil || guard(ctx) != nil || !batch.Valid() || len(records) == 0 || len(records) > MaxSites || len(batch.Jobs) != len(records) {
		return nil, false, ErrScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var oldRaw string
	if err = tx.QueryRowContext(ctx, "SELECT DOCUMENT FROM GO_SITE_LOGIN_BATCHES WHERE ID=?", batch.ID).Scan(&oldRaw); err == nil {
		var old Batch
		if json.Unmarshal([]byte(oldRaw), &old) != nil || Hash(old) != Hash(batch) {
			return nil, false, ErrState
		}
		out := []Record{}
		for _, id := range old.Jobs {
			r, e := read(ctx, tx, id)
			if e != nil {
				return nil, false, e
			}
			out = append(out, r)
		}
		return out, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	for i, r := range records {
		if !r.Valid() || r.State != "queued" || len(r.Operations) > 0 || r.Batch != batch.ID || r.Actor != batch.Actor || r.ID != batch.Jobs[i] {
			return nil, false, ErrState
		}
		site, e := readSite(ctx, tx, r.Source.SiteID)
		if e != nil || !SourceMatches(r.Source, site) {
			return nil, false, ErrScope
		}
		if r.Source.UseSaved {
			raw, e := Saved(ctx, tx)
			if e != nil || Hash(raw) != r.Source.Saved {
				return nil, false, ErrScope
			}
		}
		var active int
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_SITE_LOGIN_JOBS WHERE SITE_ID=? AND STATE IN('queued','running','waiting_input')", r.Source.SiteID).Scan(&active); e != nil {
			return nil, false, e
		}
		if active > 0 {
			return nil, false, ErrBusy
		}
		raw, _ := json.Marshal(r)
		if _, e = tx.ExecContext(ctx, "INSERT INTO GO_SITE_LOGIN_JOBS(ID,BATCH_ID,ACTOR,SITE_ID,STATE,CREATED,UPDATED,DOCUMENT)VALUES(?,?,?,?,?,?,?,?)", r.ID, r.Batch, r.Actor, r.Source.SiteID, r.State, r.Created, r.Updated, string(raw)); e != nil {
			return nil, false, e
		}
		saved, e := read(ctx, tx, r.ID)
		if e != nil || Hash(saved) != Hash(r) {
			return nil, false, ErrState
		}
	}
	raw, _ := json.Marshal(batch)
	if _, err = tx.ExecContext(ctx, "INSERT INTO GO_SITE_LOGIN_BATCHES(ID,ACTOR,DOCUMENT)VALUES(?,?,?)", batch.ID, batch.Actor, string(raw)); err != nil {
		return nil, false, err
	}
	confirmed, err := readBatch(ctx, tx, batch.ID)
	if err != nil || Hash(confirmed) != Hash(batch) {
		return nil, false, ErrState
	}
	if guard(ctx) != nil {
		return nil, false, ErrScope
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return records, false, nil
}
func update(ctx context.Context, tx *sql.Tx, before, r Record) error {
	r.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	if !r.Valid() {
		return ErrState
	}
	raw, _ := json.Marshal(r)
	result, err := tx.ExecContext(ctx, "UPDATE GO_SITE_LOGIN_JOBS SET STATE=?,UPDATED=?,DOCUMENT=? WHERE ID=? AND STATE=? AND UPDATED=? AND DOCUMENT=?", r.State, r.Updated, string(raw), r.ID, before.State, before.Updated, mustJSON(before))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrState
	}
	saved, err := read(ctx, tx, r.ID)
	if err != nil || Hash(saved) != Hash(r) {
		return ErrState
	}
	return nil
}
func mustJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }
func (s *Store) change(ctx context.Context, id string, guard Guard, siteCheck bool, fn func(*Record, *sql.Tx) error) error {
	if guard == nil || guard(ctx) != nil {
		return ErrScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := read(ctx, tx, id)
	if err != nil {
		return err
	}
	before := r
	// Copy the operation slice before mutation so the compare-and-swap still
	// binds the exact original private document.
	r.Operations = append([]Operation(nil), r.Operations...)
	if siteCheck {
		site, e := readSite(ctx, tx, r.Source.SiteID)
		if e != nil || !SourceMatches(r.Source, site) {
			return ErrScope
		}
		if r.Source.UseSaved {
			raw, e := Saved(ctx, tx)
			if e != nil || Hash(raw) != r.Source.Saved {
				return ErrScope
			}
		}
	}
	if err = fn(&r, tx); err != nil {
		return err
	}
	if guard(ctx) != nil {
		return ErrScope
	}
	if err = update(ctx, tx, before, r); err != nil {
		return err
	}
	if guard(ctx) != nil {
		return ErrScope
	}
	return tx.Commit()
}
func (s *Store) Batch(ctx context.Context, id string) (Batch, []Record, error) {
	b, err := readBatch(ctx, s.db, id)
	if err != nil {
		return b, nil, err
	}
	records := []Record{}
	for _, id := range b.Jobs {
		r, err := s.Get(ctx, id)
		if err != nil || r.Batch != b.ID || r.Actor != b.Actor {
			return b, nil, ErrState
		}
		records = append(records, r)
	}
	return b, records, nil
}

func readBatch(ctx context.Context, q rowReader, id string) (Batch, error) {
	var b Batch
	if !Hex(id, 64) {
		return b, ErrState
	}
	var raw, actor string
	if err := q.QueryRowContext(ctx, "SELECT DOCUMENT,ACTOR FROM GO_SITE_LOGIN_BATCHES WHERE ID=?", id).Scan(&raw, &actor); err != nil {
		return b, err
	}
	if len(raw) > 128<<10 || json.Unmarshal([]byte(raw), &b) != nil || !b.Valid() || b.ID != id || b.Actor != actor {
		return b, ErrState
	}
	return b, nil
}
