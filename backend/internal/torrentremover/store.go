package torrentremover

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(file string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: file, RawQuery: "mode=rwc&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	defer tx.Rollback()
	if err := schema(context.Background(), tx); err != nil {
		tx.Rollback()
		db.Close()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func schema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS GO_TORRENT_REMOVAL_CONTROL(ID INTEGER PRIMARY KEY CHECK(ID=1),DOCUMENT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_TORRENT_REMOVAL_EVENTS(ID TEXT PRIMARY KEY,EVENT_ID TEXT NOT NULL UNIQUE,STATE TEXT NOT NULL,REASON TEXT NOT NULL,ACTOR TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,DOCUMENT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_TORRENT_REMOVAL_TASKS(RECEIPT_ID TEXT NOT NULL,ID TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,PROOF TEXT NOT NULL DEFAULT '',PRIMARY KEY(RECEIPT_ID,ID))`,
		`CREATE INDEX IF NOT EXISTS GO_TORRENT_REMOVAL_STATE ON GO_TORRENT_REMOVAL_EVENTS(STATE,CREATED,ID)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
func setting(ctx context.Context, q reader, key string) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(VALUE,'') FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
func readSettings(ctx context.Context, q reader) (Settings, error) {
	s := Settings{}
	exists, err := tableExists(ctx, q, "SYSTEM_DICT")
	if err != nil || !exists {
		return s, err
	}
	s.Raw, err = setting(ctx, q, ConfigKey)
	if err != nil {
		return s, err
	}
	s.InstalledRaw, err = setting(ctx, q, "UserInstalledPlugins")
	if err != nil {
		return s, err
	}
	list := []string{}
	if s.InstalledRaw != "" && (json.Unmarshal([]byte(s.InstalledRaw), &list) != nil || list == nil) {
		return s, ErrState
	}
	s.Installed = slices.Contains(list, Plugin)
	exists, err = tableExists(ctx, q, "GO_TORRENT_REMOVAL_CONTROL")
	if err != nil || !exists {
		return s, err
	}
	var raw string
	err = q.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_TORRENT_REMOVAL_CONTROL WHERE ID=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil || json.Unmarshal([]byte(raw), &s.Control) != nil {
		return s, ErrState
	}
	return s, nil
}
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Settings{}, err
	}
	defer tx.Rollback()
	result, err := readSettings(ctx, tx)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
func verifySetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	var total, matches int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(VALUE=?),0) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, value, key).Scan(&total, &matches); err != nil || total == 0 || total != matches {
		return ErrState
	}
	return nil
}
func writeSetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	if value == "" {
		return ErrState
	}
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, key).Scan(&total); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, value, key)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != int64(total) {
		return ErrState
	}
	if total == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE)VALUES('SystemConfig',?,?,'')`, key, value)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrState
		}
	}
	return verifySetting(ctx, tx, key, value)
}

// Change atomically edits the original plugin configuration/installation and
// revokes old pending work. Passing nil keeps that original setting untouched.
func (s *Store) Change(ctx context.Context, before Settings, raw, installed *string, next Control) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readSettings(ctx, tx)
	if err != nil || Hash(current) != Hash(before) {
		return ErrState
	}
	if raw != nil {
		if _, err := Enabled(*raw); err != nil {
			return err
		}
		if err := writeSetting(ctx, tx, ConfigKey, *raw); err != nil {
			return err
		}
	}
	if installed != nil {
		var list []string
		if json.Unmarshal([]byte(*installed), &list) != nil || list == nil {
			return ErrState
		}
		if err := writeSetting(ctx, tx, "UserInstalledPlugins", *installed); err != nil {
			return err
		}
	}
	if next.Epoch != "" && !next.Valid() {
		return ErrState
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_REMOVAL_CONTROL(ID,DOCUMENT)VALUES(1,?) ON CONFLICT(ID)DO UPDATE SET DOCUMENT=excluded.DOCUMENT`, string(b))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrState
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_TORRENT_REMOVAL_CONTROL WHERE ID=1`).Scan(&stored); err != nil || stored != string(b) {
		return ErrState
	}
	if raw != nil {
		if err := verifySetting(ctx, tx, ConfigKey, *raw); err != nil {
			return err
		}
	}
	if installed != nil {
		if err := verifySetting(ctx, tx, "UserInstalledPlugins", *installed); err != nil {
			return err
		}
	}
	if Hash(before.Control) != Hash(next) {
		if _, err := tx.ExecContext(ctx, `UPDATE GO_TORRENT_REMOVAL_EVENTS SET STATE='stopped',REASON='原授权已停止或改变',UPDATED=? WHERE STATE IN('pending','running')`, timestamp()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Capture is intended only for the original committed Go source deletion
// transaction. An enabled legacy switch without a reviewed control does nothing.
func Capture(ctx context.Context, tx *sql.Tx, event Event) error {
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return err
	}
	enabled, err := Enabled(settings.Raw)
	if err != nil {
		return err
	}
	if !settings.Installed || !enabled || !settings.Control.Valid() || settings.Control.Config != Hash(settings.Raw) {
		return nil
	}
	if !event.Valid() {
		return ErrState
	}
	// Initialization belongs to Store.Open; disabled/uninitialized subscribers do
	// not create tables or turn ordinary historical events into new work.
	exists, err := tableExists(ctx, tx, "GO_TORRENT_REMOVAL_EVENTS")
	if err != nil || !exists {
		return err
	}
	var oldID string
	err = tx.QueryRowContext(ctx, `SELECT ID FROM GO_TORRENT_REMOVAL_EVENTS WHERE EVENT_ID=?`, event.ID).Scan(&oldID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	definition, err := Snapshot(ctx, tx, event, settings.Control)
	if err != nil {
		return err
	}
	state, reason := "pending", "等待原下载器证据"
	if len(definition.Problems) > 0 {
		state, reason = "blocked", "存在未获授权的原下载器或关联，未移除任何任务"
	} else if len(definition.Candidates) == 0 {
		state, reason = "ignored", "没有原下载历史候选"
	}
	b, err := json.Marshal(definition)
	if err != nil || len(b) > 4<<20 {
		return ErrState
	}
	id := Hash([]string{"torrent-remover", settings.Control.Epoch, event.ID})
	now := timestamp()
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_REMOVAL_EVENTS VALUES(?,?,?,?,?,?,?,?)`, id, event.ID, state, reason, settings.Control.Actor, now, now, string(b))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrState
	}
	for _, candidate := range definition.Candidates {
		result, err = tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_REMOVAL_TASKS(RECEIPT_ID,ID,STATE,REASON)VALUES(?,?,'pending','尚未发送移除请求')`, id, candidate.ID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrState
		}
	}
	receipt, err := readReceipt(ctx, tx, id)
	if err != nil || Hash(receipt.Definition) != Hash(definition) || len(receipt.Tasks) != len(definition.Candidates) || receipt.State != state || receipt.Reason != reason || receipt.Created != now || receipt.Updated != now {
		return ErrState
	}
	for _, task := range receipt.Tasks {
		if task.State != "pending" || task.Reason != "尚未发送移除请求" || task.Proof != "" {
			return ErrState
		}
	}
	return nil
}
func readReceipt(ctx context.Context, q reader, id string) (Receipt, error) {
	var r Receipt
	var raw string
	err := q.QueryRowContext(ctx, `SELECT ID,EVENT_ID,STATE,REASON,ACTOR,CREATED,UPDATED,DOCUMENT FROM GO_TORRENT_REMOVAL_EVENTS WHERE ID=?`, id).Scan(&r.ID, &r.EventID, &r.State, &r.Reason, &r.Actor, &r.Created, &r.Updated, &raw)
	if err != nil {
		return r, err
	}
	if !slices.Contains([]string{"pending", "running", "blocked", "ignored", "stopped", "unknown", "partial", "completed"}, r.State) {
		return r, ErrState
	}
	if len(raw) > 4<<20 || json.Unmarshal([]byte(raw), &r.Definition) != nil || !r.Definition.Valid() || r.EventID != r.Definition.Event.ID || r.Actor != r.Definition.Control.Actor || r.ID != Hash([]string{"torrent-remover", r.Definition.Control.Epoch, r.EventID}) {
		return r, ErrState
	}
	rows, err := q.QueryContext(ctx, `SELECT ID,STATE,REASON,PROOF FROM GO_TORRENT_REMOVAL_TASKS WHERE RECEIPT_ID=? ORDER BY ID LIMIT 2001`, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	r.Tasks = []Task{}
	candidates := map[string]Candidate{}
	for _, candidate := range r.Definition.Candidates {
		candidates[candidate.ID] = candidate
	}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.State, &t.Reason, &t.Proof); err != nil {
			return r, err
		}
		candidate, found := candidates[t.ID]
		t.Candidate = candidate
		if !found || !slices.Contains([]string{"pending", "intent", "completed", "unknown", "ignored", "blocked"}, t.State) {
			return r, ErrState
		}
		if slices.Contains([]string{"intent", "completed", "unknown"}, t.State) && !Hex(t.Proof, 64) {
			return r, ErrState
		}
		r.Tasks = append(r.Tasks, t)
	}
	if err := rows.Err(); err != nil {
		return r, err
	}
	if len(r.Tasks) != len(r.Definition.Candidates) {
		return r, ErrState
	}
	return r, nil
}
func (s *Store) Get(ctx context.Context, id string) (Receipt, error) {
	if !Hex(id, 64) {
		return Receipt{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()
	r, err := readReceipt(ctx, tx, id)
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}
func (s *Store) List(ctx context.Context, state string) ([]Receipt, error) {
	if state != "" && state != "pending" {
		return nil, ErrState
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := `SELECT ID FROM GO_TORRENT_REMOVAL_EVENTS`
	args := []any{}
	if state != "" {
		query += ` WHERE STATE=?`
		args = append(args, state)
	}
	query += ` ORDER BY CREATED DESC,ID DESC LIMIT 30`
	rows, err := tx.QueryContext(ctx, query, args...)
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
	receipts := []Receipt{}
	for _, id := range ids {
		r, err := readReceipt(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, r)
	}
	return receipts, tx.Commit()
}
