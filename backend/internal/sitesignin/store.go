package sitesignin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	_ "modernc.org/sqlite"
)

var ErrState = errors.New("sign-in receipt changed or is unavailable")
var ErrBusy = errors.New("original site sign-in is active")

type Store struct{ db *sql.DB }
type queryRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_BATCHES(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_JOBS(ID TEXT PRIMARY KEY,BATCH_ID TEXT NOT NULL,SITE_ID INTEGER NOT NULL,DAY TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,DOCUMENT TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS GO_SITE_SIGNIN_ACTIVE ON GO_SITE_SIGNIN_JOBS(SITE_ID) WHERE STATE IN('queued','running','waiting_input')`,
		`CREATE INDEX IF NOT EXISTS GO_SITE_SIGNIN_DAILY ON GO_SITE_SIGNIN_JOBS(DAY,SITE_ID,CREATED DESC)`,
		`CREATE TABLE IF NOT EXISTS GO_SITE_SIGNIN_CONTROL(ID INTEGER PRIMARY KEY CHECK(ID=1),ENABLED INTEGER NOT NULL,DOCUMENT TEXT NOT NULL,NEXT_RUN TEXT NOT NULL,NEXT_MISSED TEXT NOT NULL)`,
	} {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	db.SetMaxIdleConns(0)
	return &Store{db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func encoded(v any) string    { raw, _ := json.Marshal(v); return string(raw) }
func readJob(ctx context.Context, q queryRow, id string) (Job, error) {
	var r Job
	var raw, state, batch, day, created string
	var site int64
	if !sitelogin.Hex(id, 64) {
		return r, ErrState
	}
	if err := q.QueryRowContext(ctx, `SELECT DOCUMENT,STATE,SITE_ID,BATCH_ID,DAY,CREATED FROM GO_SITE_SIGNIN_JOBS WHERE ID=?`, id).Scan(&raw, &state, &site, &batch, &day, &created); err != nil {
		return r, err
	}
	if len(raw) > 1<<20 || json.Unmarshal([]byte(raw), &r) != nil || !r.Valid() || r.ID != id || r.State != state || r.Source.SiteID != site || r.Batch != batch || r.Day != day || r.Created != created {
		return r, ErrState
	}
	return r, nil
}
func (s *Store) Get(ctx context.Context, id string) (Job, error) { return readJob(ctx, s.db, id) }
func readBatch(ctx context.Context, q queryRow, id string) (Batch, error) {
	var b Batch
	var raw string
	if !sitelogin.Hex(id, 64) {
		return b, ErrState
	}
	if err := q.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_SITE_SIGNIN_BATCHES WHERE ID=?`, id).Scan(&raw); err != nil {
		return b, err
	}
	if len(raw) > 512<<10 || json.Unmarshal([]byte(raw), &b) != nil || b.ID != id || !b.Scope.Valid() || len(b.Jobs) > MaxSites {
		return b, ErrState
	}
	return b, nil
}
func (s *Store) Batch(ctx context.Context, id string) (Batch, []Job, error) {
	b, err := readBatch(ctx, s.db, id)
	if err != nil {
		return b, nil, err
	}
	out := []Job{}
	for _, id := range b.Jobs {
		r, e := s.Get(ctx, id)
		if e != nil || r.Batch != b.ID || r.ScopeID != b.Scope.ID {
			return b, nil, ErrState
		}
		out = append(out, r)
	}
	return b, out, nil
}
func sourceCurrent(ctx context.Context, q queryRow, source Source) error {
	var v siteconfig.Site
	err := q.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(PRI,''),COALESCE(RSSURL,''),COALESCE(SIGNURL,''),COALESCE(COOKIE,''),COALESCE(APIKEY,''),COALESCE(INCLUDE,''),COALESCE(EXCLUDE,''),COALESCE(SIZE,''),COALESCE(NOTE,'') FROM CONFIG_SITE WHERE ID=?`, source.SiteID).Scan(&v.ID, &v.Name, &v.Priority, &v.RSSURL, &v.SignURL, &v.Cookie, &v.APIKey, &v.Include, &v.Exclude, &v.Size, &v.Note)
	if err != nil {
		return err
	}
	if sitelogin.Hash(v) != source.SiteDigest {
		return ErrScope
	}
	return nil
}

// Create is called under the application's cross-process sign-in guard. A
// repeated batch ID returns the original records, including interrupted work.
func (s *Store) Create(ctx context.Context, b Batch, records []Job, guard func(context.Context) error) ([]Job, bool, error) {
	if old, rows, err := s.Batch(ctx, b.ID); err == nil {
		if sitelogin.Hash(old.Scope) != sitelogin.Hash(b.Scope) {
			return nil, false, ErrScope
		}
		return rows, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if !b.Scope.Valid() || !sitelogin.Hex(b.ID, 64) || len(records) != len(b.Jobs) || len(records) > MaxSites || guard == nil || guard(ctx) != nil {
		return nil, false, ErrScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err = controlMatches(ctx, tx, b); err != nil {
		return nil, false, err
	}
	seen := map[int64]bool{}
	for i, r := range records {
		if !r.Valid() || r.State != "queued" || len(r.Operations) > 0 || r.ID != b.Jobs[i] || r.Batch != b.ID || r.ScopeID != b.Scope.ID || r.Actor != b.Scope.Actor || seen[r.Source.SiteID] {
			return nil, false, ErrState
		}
		seen[r.Source.SiteID] = true
		found := false
		for _, source := range b.Scope.Sources {
			if sitelogin.Hash(source) == sitelogin.Hash(r.Source) {
				found = true
			}
		}
		if !found {
			return nil, false, ErrScope
		}
		if err = sourceCurrent(ctx, tx, r.Source); err != nil {
			return nil, false, err
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_SITE_SIGNIN_JOBS WHERE SITE_ID=? AND STATE IN('queued','running','waiting_input')`, r.Source.SiteID).Scan(&n); err != nil {
			return nil, false, err
		}
		if n > 0 {
			return nil, false, ErrBusy
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_JOBS VALUES(?,?,?,?,?,?,?)`, r.ID, r.Batch, r.Source.SiteID, r.Day, r.State, r.Created, encoded(r)); err != nil {
			return nil, false, err
		}
		actual, e := readJob(ctx, tx, r.ID)
		if e != nil || sitelogin.Hash(actual) != sitelogin.Hash(r) {
			return nil, false, ErrState
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO GO_SITE_SIGNIN_BATCHES VALUES(?,?)`, b.ID, encoded(b)); err != nil {
		return nil, false, err
	}
	actual, err := readBatch(ctx, tx, b.ID)
	if err != nil || sitelogin.Hash(actual) != sitelogin.Hash(b) || guard(ctx) != nil {
		return nil, false, ErrScope
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return records, false, nil
}
func updateJob(ctx context.Context, tx *sql.Tx, before, after Job) error {
	after.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	if !after.Valid() {
		return ErrState
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_SITE_SIGNIN_JOBS SET STATE=?,DOCUMENT=? WHERE ID=? AND DOCUMENT=?`, after.State, encoded(after), before.ID, encoded(before))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrState
	}
	actual, err := readJob(ctx, tx, after.ID)
	if err != nil || sitelogin.Hash(actual) != sitelogin.Hash(after) {
		return ErrState
	}
	return nil
}
func (s *Store) change(ctx context.Context, id string, guard func(context.Context) error, source bool, change func(*Job, *sql.Tx) error) error {
	if guard == nil || guard(ctx) != nil {
		return ErrScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := readJob(ctx, tx, id)
	if err != nil {
		return err
	}
	if source {
		if err = sourceCurrent(ctx, tx, before.Source); err != nil {
			return err
		}
		batch, err := readBatch(ctx, tx, before.Batch)
		if err != nil {
			return err
		}
		if err = controlMatches(ctx, tx, batch); err != nil {
			return err
		}
	}
	after := before
	after.Operations = append([]Operation(nil), before.Operations...)
	if err = change(&after, tx); err != nil {
		return err
	}
	if err = updateJob(ctx, tx, before, after); err != nil {
		return err
	}
	if guard(ctx) != nil {
		return ErrScope
	}
	return tx.Commit()
}
func (s *Store) Start(ctx context.Context, id string, guard func(context.Context) error) error {
	return s.change(ctx, id, guard, true, func(r *Job, _ *sql.Tx) error {
		if r.State != "queued" {
			return ErrState
		}
		r.State = "running"
		return nil
	})
}
func (s *Store) Step(ctx context.Context, id string, operation Operation, guard func(context.Context) error) (int, error) {
	index := -1
	err := s.change(ctx, id, guard, true, func(r *Job, _ *sql.Tx) error {
		if r.State != "running" && r.State != "waiting_input" || operation.State != "intent" || len(r.Operations) >= 256 {
			return ErrState
		}
		for _, op := range r.Operations {
			if op.Signature == operation.Signature || op.State == "uncertain" || op.State == "denied" || op.State == "intent" && !op.Action {
				return ErrState
			}
		}
		index = len(r.Operations)
		r.Operations = append(r.Operations, operation)
		return nil
	})
	return index, err
}
func (s *Store) Acknowledge(ctx context.Context, id string, index int, state, digest string) error {
	return s.change(ctx, id, func(ctx context.Context) error { return ctx.Err() }, false, func(r *Job, _ *sql.Tx) error {
		if index < 0 || index >= len(r.Operations) || r.Operations[index].State != "intent" || state != "received" && state != "uncertain" && state != "denied" {
			return ErrState
		}
		r.Operations[index].State = state
		r.Operations[index].Digest = digest
		return nil
	})
}
func (s *Store) Finish(ctx context.Context, id string, outcome Outcome) error {
	if !validOutcome(outcome) {
		return ErrState
	}
	return s.change(ctx, id, func(ctx context.Context) error { return ctx.Err() }, false, func(r *Job, _ *sql.Tx) error {
		if !Active(r.State) {
			return ErrState
		}
		for _, op := range r.Operations {
			if op.State == "intent" || op.State == "uncertain" {
				outcome = unknown()
			}
		}
		if outcome.State == "signed" || outcome.State == "already" || outcome.State == "authenticated" {
			if len(r.Operations) == 0 {
				return ErrState
			}
			action := false
			for _, op := range r.Operations {
				if op.State != "received" {
					return ErrState
				}
				action = action || op.Action
			}
			if outcome.State == "signed" && !action {
				return ErrState
			}
		}
		r.State = "finished"
		r.Outcome = outcome
		r.Need = ""
		r.Nonce = ""
		r.Deadline = ""
		return nil
	})
}
func (s *Store) Prompt(ctx context.Context, id, kind, nonce string, deadline time.Time, guard func(context.Context) error) error {
	return s.change(ctx, id, guard, true, func(r *Job, _ *sql.Tx) error {
		if r.State != "running" || !deadline.After(time.Now()) || time.Until(deadline) > 2*time.Minute || !sitelogin.Hex(nonce, 64) {
			return ErrState
		}
		r.State = "waiting_input"
		r.Need = kind
		r.Nonce = nonce
		r.Deadline = deadline.UTC().Format(time.RFC3339Nano)
		return nil
	})
}
func (s *Store) Consume(ctx context.Context, id, nonce string, guard func(context.Context) error) error {
	return s.change(ctx, id, guard, true, func(r *Job, _ *sql.Tx) error {
		deadline, err := time.Parse(time.RFC3339Nano, r.Deadline)
		if r.State != "waiting_input" || r.Nonce != nonce || err != nil || !time.Now().Before(deadline) {
			return ErrInput
		}
		r.State = "running"
		r.Need = ""
		r.Nonce = ""
		r.Deadline = ""
		return nil
	})
}
func (s *Store) List(ctx context.Context, page int) ([]Job, error) {
	if page < 1 || page > 100000 {
		return nil, ErrState
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_SITE_SIGNIN_JOBS ORDER BY CREATED DESC,ID LIMIT 30 OFFSET ?`, (page-1)*30)
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
	out := []Job{}
	for _, id := range ids {
		r, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Recover never executes a provider request. It is used only while holding the
// same process guard as execution, so a second process cannot stop live work.
func (s *Store) Recover(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_SITE_SIGNIN_JOBS WHERE STATE IN('queued','running','waiting_input') LIMIT 501`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) > 500 {
		return ErrState
	}
	for _, id := range ids {
		r, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		out := Outcome{State: "stopped", Reason: "原进程已结束，没有恢复或重放请求"}
		for _, op := range r.Operations {
			if op.Action || op.State == "intent" || op.State == "uncertain" {
				out = unknown()
			}
		}
		if err = s.Finish(ctx, id, out); err != nil {
			return err
		}
	}
	return nil
}

// Daily returns native receipts only. Legacy PLUGINHISTORY is retained but is
// never upgraded into proof of a provider result.
func (s *Store) Daily(ctx context.Context, day string) (map[int64]Job, error) {
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return nil, ErrState
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_SITE_SIGNIN_JOBS WHERE DAY=? ORDER BY CREATED DESC,ID LIMIT 5001`, day)
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
	if err != nil || len(ids) > 5000 {
		return nil, ErrState
	}
	out := map[int64]Job{}
	for _, id := range ids {
		r, e := s.Get(ctx, id)
		if e != nil {
			return nil, e
		}
		if _, exists := out[r.Source.SiteID]; !exists {
			out[r.Source.SiteID] = r
		}
	}
	return out, nil
}
func (s *Store) Notify(ctx context.Context, id, summary string) error {
	if len(summary) > 10000 {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := readBatch(ctx, tx, id)
	if err != nil {
		return err
	}
	if b.Notified {
		return nil
	}
	for _, id := range b.Jobs {
		r, err := readJob(ctx, tx, id)
		if err != nil || Active(r.State) {
			return ErrState
		}
	}
	if b.Scope.Notify {
		if err = bus.Emit(ctx, tx, "site_signin", b.ID, b.ID, "站点签到任务完成", summary); err != nil {
			return err
		}
	}
	before := encoded(b)
	b.Notified = true
	b.Summary = summary
	if _, err = tx.ExecContext(ctx, `UPDATE GO_SITE_SIGNIN_BATCHES SET DOCUMENT=? WHERE ID=? AND DOCUMENT=?`, encoded(b), id, before); err != nil {
		return err
	}
	actual, err := readBatch(ctx, tx, id)
	if err != nil || encoded(actual) != encoded(b) {
		return ErrState
	}
	return tx.Commit()
}
