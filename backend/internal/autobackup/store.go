package autobackup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Control struct {
	Epoch, Scope, Actor string
	Next, Once          string
}

type State struct {
	Raw       string
	Installed bool
	Control   Control
}

type Run struct {
	ID, Kind, State, Actor, Scope, Epoch string
	Created, Message                     string
	Target                               Target
	Archive                              Archive
	Pruned                               int
	Retired                              bool
	Retiring                             string
	Stop                                 bool
}

func OpenStore(path string) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, "rw"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS GO_AUTO_BACKUP_CONTROL(ID INTEGER PRIMARY KEY CHECK(ID=1),VALUE TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS GO_AUTO_BACKUP_RUNS(ID TEXT PRIMARY KEY,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,VALUE TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS GO_AUTO_BACKUP_RUNS_CREATED ON GO_AUTO_BACKUP_RUNS(CREATED DESC,ID);
CREATE INDEX IF NOT EXISTS GO_AUTO_BACKUP_PENDING_PRUNE ON GO_AUTO_BACKUP_RUNS(ID) WHERE STATE='complete' AND COALESCE(json_extract(VALUE,'$.Retiring'),'')<>'';`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func setting(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(VALUE,'') FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}

func installed(ctx context.Context, tx *sql.Tx) ([]string, error) {
	list := []string{}
	raw, err := setting(ctx, tx, "UserInstalledPlugins")
	if err == nil && raw != "" && json.Unmarshal([]byte(raw), &list) != nil {
		err = ErrSettings
	}
	return list, err
}

func state(ctx context.Context, tx *sql.Tx) (State, error) {
	var out State
	raw, err := setting(ctx, tx, ConfigKey)
	if err != nil {
		return out, err
	}
	list, err := installed(ctx, tx)
	if err != nil {
		return out, err
	}
	out.Raw, out.Installed = raw, slices.Contains(list, Plugin)
	err = tx.QueryRowContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_CONTROL WHERE ID=1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &out.Control)
	}
	return out, err
}

func (s *Store) State(ctx context.Context) (State, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return State{}, err
	}
	defer tx.Rollback()
	return state(ctx, tx)
}

func putSetting(ctx context.Context, tx *sql.Tx, key, raw string) error {
	res, err := tx.ExecContext(ctx, "UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?", raw, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig',?,?)", key, raw)
	}
	return err
}

func setControl(ctx context.Context, tx *sql.Tx, control Control) error {
	raw, err := json.Marshal(control)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO GO_AUTO_BACKUP_CONTROL VALUES(1,?) ON CONFLICT(ID) DO UPDATE SET VALUE=excluded.VALUE", raw)
	return err
}

type storedRun struct {
	Run
	DestinationIdentity string
	ArchiveDigest       string
}

func readRun(row interface{ Scan(...any) error }) (Run, error) {
	var raw string
	var out storedRun
	if err := row.Scan(&raw); err != nil {
		return out.Run, err
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return Run{}, err
	}
	out.Run.Target.Identity, out.Run.Archive.Digest = out.DestinationIdentity, out.ArchiveDigest
	return out.Run, nil
}

func writeRun(ctx context.Context, tx *sql.Tx, r Run, create bool) error {
	value, err := json.Marshal(storedRun{r, r.Target.Identity, r.Archive.Digest})
	if err != nil {
		return err
	}
	stmt := "INSERT INTO GO_AUTO_BACKUP_RUNS(VALUE,STATE,CREATED,ID) VALUES(?,?,?,?)"
	if !create {
		stmt = "UPDATE GO_AUTO_BACKUP_RUNS SET VALUE=?,STATE=?,CREATED=? WHERE ID=?"
	}
	res, err := tx.ExecContext(ctx, stmt, value, r.State, r.Created, r.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrScope
	}
	actual, err := readRun(tx.QueryRowContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_RUNS WHERE ID=? AND STATE=?", r.ID, r.State))
	if err != nil || actual != r {
		return ErrScope
	}
	return nil
}

func active(ctx context.Context, tx *sql.Tx) (Run, error) {
	r, err := readRun(tx.QueryRowContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_RUNS WHERE STATE IN ('running','staged') LIMIT 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, nil
	}
	return r, err
}

func (s *Store) Change(ctx context.Context, before, next State, activeID *string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := state(ctx, tx)
	if err != nil || current != before {
		return ErrScope
	}
	r, err := active(ctx, tx)
	if err != nil {
		return err
	}
	if activeID != nil && r.ID != *activeID {
		return ErrScope
	}
	if (next != before || activeID != nil) && r.ID != "" {
		r.Stop = true
		if err := writeRun(ctx, tx, r, false); err != nil {
			return err
		}
	}
	if next.Installed != before.Installed {
		list, err := installed(ctx, tx)
		if err != nil {
			return err
		}
		list = slices.DeleteFunc(list, func(id string) bool { return id == Plugin })
		if next.Installed {
			list = append(list, Plugin)
		}
		raw, err := json.Marshal(list)
		if err != nil {
			return err
		}
		if err := putSetting(ctx, tx, "UserInstalledPlugins", string(raw)); err != nil {
			return err
		}
	}
	if next.Raw != before.Raw {
		if err := putSetting(ctx, tx, ConfigKey, next.Raw); err != nil {
			return err
		}
	}
	if next.Control != before.Control {
		if err := setControl(ctx, tx, next.Control); err != nil {
			return err
		}
	}
	if actual, err := state(ctx, tx); err != nil || actual != next {
		return ErrScope
	}
	return tx.Commit()
}

func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	return readRun(s.db.QueryRowContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_RUNS WHERE ID=?", id))
}

func (s *Store) Runs(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_RUNS ORDER BY STATE IN ('running','staged') DESC,CREATED DESC,ID LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := readRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Retention(ctx context.Context, target Target) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT VALUE FROM GO_AUTO_BACKUP_RUNS WHERE STATE='complete'
	AND json_extract(VALUE,'$.Target.path')=? AND json_extract(VALUE,'$.DestinationIdentity')=?
	AND json_extract(VALUE,'$.Retired')=0 AND json_extract(VALUE,'$.Kind')<>'legacy'
	ORDER BY CREATED DESC,ID DESC`, target.Path, target.Identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := readRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Start(ctx context.Context, before State, next Control, r Run) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	current, err := state(ctx, tx)
	if err != nil || current != before || !current.Installed && r.Kind != "legacy" {
		return Run{}, ErrScope
	}
	a, err := active(ctx, tx)
	if err != nil {
		return Run{}, err
	}
	if a.ID != "" {
		return Run{}, ErrBusy
	}
	if next != before.Control {
		if err := setControl(ctx, tx, next); err != nil {
			return Run{}, err
		}
	}
	r.State, r.Created = "running", time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
	if err := writeRun(ctx, tx, r, true); err != nil {
		return Run{}, err
	}
	expected := before
	expected.Control = next
	if actual, err := state(ctx, tx); err != nil || actual != expected {
		return Run{}, ErrScope
	}
	return r, tx.Commit()
}

func (s *Store) Replace(ctx context.Context, before, next Run, required *State, notify bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if required != nil {
		if actual, err := state(ctx, tx); err != nil || actual != *required {
			return ErrScope
		}
	}
	actual, err := readRun(tx.QueryRowContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_RUNS WHERE ID=?", before.ID))
	if err != nil || actual != before {
		return ErrScope
	}
	if err := writeRun(ctx, tx, next, false); err != nil {
		return err
	}
	if notify {
		if err := bus.EmitData(ctx, tx, "plugin_message", next.ID, next.ID, "【自动备份任务完成】", next.Message, map[string]string{"plugin": Plugin}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Finalize serializes publication with control changes in other processes.
// The staged receipt survives a crash between the filesystem and SQLite commits.
func (s *Store) Finalize(ctx context.Context, staged Run, required State, notify bool, publish func() error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE GO_AUTO_BACKUP_RUNS SET STATE=STATE WHERE ID=?", staged.ID); err != nil {
		return err
	}
	actual, err := state(ctx, tx)
	if err != nil || actual != required {
		return ErrScope
	}
	run, err := readRun(tx.QueryRowContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_RUNS WHERE ID=?", staged.ID))
	if err != nil || run != staged || run.Stop || run.State != "staged" {
		return ErrScope
	}
	if err := publish(); err != nil {
		return err
	}
	run.State, run.Message = "complete", "备份已完成"
	if err := writeRun(ctx, tx, run, false); err != nil {
		return err
	}
	if notify {
		if err := bus.EmitData(ctx, tx, "plugin_message", run.ID, run.ID, "【自动备份任务完成】", run.Message, map[string]string{"plugin": Plugin}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Prune(ctx context.Context, required State, owner, old Run, remove func() error) error {
	if owner.State != "complete" || owner.ID == old.ID || old.State != "complete" || old.Retired || old.Retiring != "" || old.Target != owner.Target {
		return ErrScope
	}
	pending := old
	pending.Retiring = owner.ID
	if err := s.Replace(ctx, old, pending, &required, false); err != nil {
		return err
	}
	return s.finishPrune(ctx, pending, &required, func() (bool, error) {
		err := remove()
		return err == nil, err
	})
}

func (s *Store) PendingPrunes(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT VALUE FROM GO_AUTO_BACKUP_RUNS
		WHERE STATE='complete' AND COALESCE(json_extract(VALUE,'$.Retiring'),'')<>'' LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		run, err := readRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) ReconcilePrune(ctx context.Context, pending Run) error {
	return s.finishPrune(ctx, pending, nil, func() (bool, error) {
		return pending.Target.Absent(pending.Archive.Name)
	})
}

func (s *Store) finishPrune(ctx context.Context, pending Run, required *State, effect func() (bool, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE GO_AUTO_BACKUP_RUNS SET STATE=STATE WHERE ID=?", pending.ID); err != nil {
		return err
	}
	if required != nil {
		actual, err := state(ctx, tx)
		if err != nil || actual != *required {
			return ErrScope
		}
	}
	read := func(id string) (Run, error) {
		return readRun(tx.QueryRowContext(ctx, "SELECT VALUE FROM GO_AUTO_BACKUP_RUNS WHERE ID=?", id))
	}
	owner, err := read(pending.Retiring)
	if err != nil || owner.State != "complete" || owner.ID == pending.ID {
		return ErrScope
	}
	item, err := read(pending.ID)
	if err != nil || item != pending || item.State != "complete" || item.Retired || item.Target != owner.Target {
		return ErrScope
	}
	removed, err := effect()
	if err != nil {
		return err
	}
	item.Retired, item.Retiring = removed, ""
	if err := writeRun(ctx, tx, item, false); err != nil {
		return err
	}
	if removed {
		owner.Pruned++
		if err := writeRun(ctx, tx, owner, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}
