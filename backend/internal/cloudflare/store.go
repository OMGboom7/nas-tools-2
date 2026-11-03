package cloudflare

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Source struct{ Config, Hosts, Installed string }
type Control struct {
	Epoch, Actor, Authority, Root, HostsPath, Location string
	Enabled, FromSignin                                bool
	NextRun, NextOnce                                  string
	Source                                             Source
}
type ApplyPlan struct {
	Hosts                     HostPlan
	HostsRaw, HostsInput      string
	Config                    string
	FileBeforeHash, FileAfter string
	FileBefore                string
}
type Job struct {
	ID, Origin, Epoch, Actor, Created, Updated, State, Reason string
	HostsPath                                                 string
	Source                                                    Source
	Operations                                                []Operation
	Plan                                                      *ApplyPlan
	Result                                                    *Result
}
type Store struct{ db *sql.DB }
type query interface {
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
		`CREATE TABLE IF NOT EXISTS GO_CLOUDFLARE_CONTROL(ID INTEGER PRIMARY KEY CHECK(ID=1),DOCUMENT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_CLOUDFLARE_JOBS(ID TEXT PRIMARY KEY,ORIGIN TEXT NOT NULL UNIQUE,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,DOCUMENT TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS GO_CLOUDFLARE_ACTIVE ON GO_CLOUDFLARE_JOBS((1)) WHERE STATE IN('queued','running','applying','unknown')`,
	} {
		if _, err = db.Exec(stmt); err != nil {
			db.Close()
			return nil, err
		}
	}
	db.SetMaxIdleConns(0)
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }
func encode(v any) string     { data, _ := json.Marshal(v); return string(data) }
func setting(ctx context.Context, q query, key string) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || len(raw) > 128<<10 {
		return "", ErrState
	}
	var total, matches int
	if err = q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(VALUE=?),0) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, raw, key).Scan(&total, &matches); err != nil || total != matches {
		return "", ErrState
	}
	return raw, nil
}
func source(ctx context.Context, q query) (Source, error) {
	var out Source
	for key, target := range map[string]*string{ConfigKey: &out.Config, "plugin.CustomHosts": &out.Hosts, "UserInstalledPlugins": &out.Installed} {
		value, err := setting(ctx, q, key)
		if err != nil {
			return out, err
		}
		*target = value
	}
	return out, nil
}
func (s *Store) Source(ctx context.Context) (Source, error) { return source(ctx, s.db) }
func installed(raw, id string) bool {
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil {
		return false
	}
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func Installed(src Source) bool {
	return installed(src.Installed, Plugin) && installed(src.Installed, "CustomHosts")
}
func control(ctx context.Context, q query) (Control, error) {
	var raw string
	var c Control
	err := q.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_CLOUDFLARE_CONTROL WHERE ID=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil || len(raw) > 512<<10 || json.Unmarshal([]byte(raw), &c) != nil || c.Epoch == "" {
		return c, ErrState
	}
	return c, nil
}
func (s *Store) Control(ctx context.Context) (Control, error) { return control(ctx, s.db) }
func readJob(ctx context.Context, q query, id string) (Job, error) {
	var j Job
	var raw, state, origin string
	err := q.QueryRowContext(ctx, `SELECT DOCUMENT,STATE,ORIGIN FROM GO_CLOUDFLARE_JOBS WHERE ID=?`, id).Scan(&raw, &state, &origin)
	if err != nil {
		return j, err
	}
	if len(raw) > 8<<20 || json.Unmarshal([]byte(raw), &j) != nil || j.ID != id || j.State != state || j.Origin != origin || sumJSON(j.Origin) != id {
		return j, ErrState
	}
	return j, nil
}
func (s *Store) Get(ctx context.Context, id string) (Job, error) { return readJob(ctx, s.db, id) }
func (s *Store) Jobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_CLOUDFLARE_JOBS ORDER BY CREATED DESC,ID DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
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
		j, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}
func saveControl(ctx context.Context, tx *sql.Tx, c Control) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO GO_CLOUDFLARE_CONTROL(ID,DOCUMENT) VALUES(1,?) ON CONFLICT(ID) DO UPDATE SET DOCUMENT=excluded.DOCUMENT`, encode(c))
	return err
}
func saveJob(ctx context.Context, tx *sql.Tx, j Job) error {
	j.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `UPDATE GO_CLOUDFLARE_JOBS SET STATE=?,DOCUMENT=? WHERE ID=?`, j.State, encode(j), j.ID)
	return err
}
func writeSetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	if value == "" {
		return ErrState
	}
	result, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, value, key)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE) VALUES('SystemConfig',?,?,'')`, key, value); err != nil {
			return err
		}
	}
	stored, err := setting(ctx, tx, key)
	if err != nil || stored != value {
		return ErrState
	}
	return nil
}
func current(ctx context.Context, q query, c Control) error {
	src, err := source(ctx, q)
	if err != nil || !c.Enabled || !Installed(src) || c.Source != src {
		return ErrState
	}
	return nil
}
func (s *Store) Valid(ctx context.Context, c Control) error {
	actual, err := s.Control(ctx)
	if err != nil || actual.Epoch != c.Epoch {
		return ErrState
	}
	return current(ctx, s.db, actual)
}

func (s *Store) Activate(ctx context.Context, c Control, expected Source) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actual, err := source(ctx, tx)
	if err != nil || actual != expected || !Installed(actual) || c.Epoch == "" || c.Actor == "" || c.Authority == "" || !filepath.IsAbs(c.Root) || !filepath.IsAbs(c.HostsPath) {
		return ErrState
	}
	settings, err := DecodeSettings(actual.Config)
	if err != nil || settings.CFIP == "" {
		return ErrState
	}
	if _, err = PlanHosts(actual.Hosts, settings, settings.CFIP); err != nil {
		return err
	}
	actual.Config, err = Patch(actual.Config, map[string]json.RawMessage{"onlyonce": json.RawMessage(`false`)})
	if err != nil {
		return err
	}
	if err = writeSetting(ctx, tx, ConfigKey, actual.Config); err != nil {
		return err
	}
	c.Source = actual
	c.Enabled = true
	if err = saveControl(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Stop(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := control(ctx, tx)
	if err != nil || c.Epoch == "" {
		return err
	}
	c.Enabled = false
	c.NextOnce = ""
	c.NextRun = ""
	if err = saveControl(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Queue(ctx context.Context, origin string, fromSignin bool, expected ...Source) (Job, error) {
	if len(expected) > 1 {
		return Job{}, ErrState
	}
	if origin == "" || len(origin) > 256 {
		return Job{}, ErrState
	}
	id := sumJSON(origin)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if old, err := readJob(ctx, tx, id); err == nil {
		return old, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Job{}, err
	}
	c, err := control(ctx, tx)
	if err != nil || fromSignin && !c.FromSignin {
		return Job{}, ErrState
	}
	if err = current(ctx, tx, c); err != nil {
		return Job{}, err
	}
	if len(expected) == 1 && c.Source != expected[0] {
		return Job{}, ErrState
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	j := Job{ID: id, Origin: origin, Epoch: c.Epoch, Actor: c.Actor, HostsPath: c.HostsPath, Source: c.Source, Created: now, Updated: now, State: "queued", Operations: []Operation{}}
	if _, err = tx.ExecContext(ctx, `INSERT INTO GO_CLOUDFLARE_JOBS(ID,ORIGIN,STATE,CREATED,DOCUMENT) VALUES(?,?,?,?,?)`, j.ID, j.Origin, j.State, j.Created, encode(j)); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, err
	}
	return j, nil
}
func (s *Store) Advance(ctx context.Context, epoch, next, nextOnce string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := control(ctx, tx)
	if err != nil || c.Epoch != epoch {
		return ErrState
	}
	c.NextRun = next
	c.NextOnce = nextOnce
	if err = saveControl(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Before(ctx context.Context, id string, op Operation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readJob(ctx, tx, id)
	if err != nil || j.State != "queued" && j.State != "running" || len(j.Operations) >= 64 {
		return ErrState
	}
	c, err := control(ctx, tx)
	if err != nil || c.Epoch != j.Epoch || c.Source != j.Source {
		return ErrState
	}
	if err = current(ctx, tx, c); err != nil {
		return err
	}
	j.State = "running"
	j.Operations = append(j.Operations, op)
	if err = saveJob(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Terminal(ctx context.Context, id, state, reason string) error {
	switch state {
	case "failed", "blocked", "unknown", "closed":
	default:
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readJob(ctx, tx, id)
	if err != nil {
		return err
	}
	if state == "closed" {
		if j.State != "unknown" {
			return ErrState
		}
	} else if j.State != "queued" && j.State != "running" && j.State != "applying" {
		return ErrState
	}
	if j.Plan != nil && state != "unknown" && state != "closed" {
		return ErrState
	}
	j.State = state
	j.Reason = reason
	if err = saveJob(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

// Call only after acquiring the process-shared execution guard.
func (s *Store) Recover(ctx context.Context) error {
	jobs, err := s.Jobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.State == "running" || j.State == "applying" {
			if err = s.Terminal(ctx, j.ID, "unknown", "原进程中断；不自动重发测速或 Hosts 写入"); err != nil {
				return err
			}
		}
	}
	return nil
}
