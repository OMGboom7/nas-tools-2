package cookiecloud

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Control struct {
	Epoch    string `json:"epoch"`
	Actor    string `json:"-"`
	Scope    string `json:"-"`
	NextRun  string `json:"nextRun"`
	NextOnce string `json:"nextOnce"`
}

type Run struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Created      string `json:"created"`
	Updated      string `json:"updated"`
	Message      string `json:"message"`
	Added        int    `json:"added"`
	UpdatedSites int    `json:"updatedSites"`
	Kept         int    `json:"kept"`
	Skipped      int    `json:"skipped"`
	Actor        string `json:"-"`
	Scope        string `json:"-"`
	Epoch        string `json:"-"`
	Stop         bool   `json:"stopRequested"`
}

func Open(path string) (*Store, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, sql := range []string{
		`CREATE TABLE IF NOT EXISTS GO_COOKIE_CLOUD_CONTROL(ID INTEGER PRIMARY KEY CHECK(ID=1),EPOCH TEXT NOT NULL,ACTOR TEXT NOT NULL,SCOPE TEXT NOT NULL,NEXT_RUN TEXT NOT NULL,NEXT_ONCE TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_COOKIE_CLOUD_RUNS(ID TEXT PRIMARY KEY,KIND TEXT NOT NULL,STATE TEXT NOT NULL,ACTOR TEXT NOT NULL,SCOPE TEXT NOT NULL,EPOCH TEXT NOT NULL,STOP INTEGER NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,MESSAGE TEXT NOT NULL,ADDED INTEGER NOT NULL,UPDATED_SITES INTEGER NOT NULL,KEPT INTEGER NOT NULL,SKIPPED INTEGER NOT NULL)`,
	} {
		if _, err = db.Exec(sql); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func systemValue(ctx context.Context, q rowReader, key string) (string, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(VALUE,'') FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func installedList(raw string) ([]string, error) {
	items := []string{}
	if raw != "" && json.Unmarshal([]byte(raw), &items) != nil {
		return nil, ErrConfig
	}
	return items, nil
}

func readSettings(ctx context.Context, q rowReader) (string, bool, error) {
	raw, err := systemValue(ctx, q, ConfigKey)
	if err != nil {
		return "", false, err
	}
	list, err := systemValue(ctx, q, "UserInstalledPlugins")
	if err != nil {
		return "", false, err
	}
	installed, err := installedList(list)
	return raw, slices.Contains(installed, Plugin), err
}

func (s *Store) Settings(ctx context.Context) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	return readSettings(ctx, tx)
}

func putValue(ctx context.Context, tx *sql.Tx, key, raw string) error {
	result, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, raw, key)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, key, raw)
		if err != nil {
			return err
		}
	}
	current, err := systemValue(ctx, tx, key)
	if err != nil {
		return err
	}
	if current != raw {
		return ErrScope
	}
	return nil
}

func requireSettings(ctx context.Context, q rowReader, expected string) error {
	raw, installed, err := readSettings(ctx, q)
	if err != nil {
		return err
	}
	if !installed || raw != expected {
		return ErrScope
	}
	return nil
}

func (s *Store) Save(ctx context.Context, previous, next string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = requireSettings(ctx, tx, previous); err != nil {
		return err
	}
	if previous == next {
		return nil
	}
	if err = stop(ctx, tx); err != nil {
		return err
	}
	if err = putValue(ctx, tx, ConfigKey, next); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetInstalled(ctx context.Context, on bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	raw, err := systemValue(ctx, tx, "UserInstalledPlugins")
	if err != nil {
		return err
	}
	list, err := installedList(raw)
	if err != nil {
		return err
	}
	if on == slices.Contains(list, Plugin) {
		return nil
	}
	if on {
		list = append(list, Plugin)
	} else {
		list = slices.DeleteFunc(list, func(name string) bool { return name == Plugin })
		if err = stop(ctx, tx); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err = putValue(ctx, tx, "UserInstalledPlugins", string(encoded)); err != nil {
		return err
	}
	return tx.Commit()
}

func readControl(ctx context.Context, q rowReader) (Control, error) {
	var c Control
	err := q.QueryRowContext(ctx, `SELECT EPOCH,ACTOR,SCOPE,NEXT_RUN,NEXT_ONCE FROM GO_COOKIE_CLOUD_CONTROL WHERE ID=1`).
		Scan(&c.Epoch, &c.Actor, &c.Scope, &c.NextRun, &c.NextOnce)
	if errors.Is(err, sql.ErrNoRows) {
		return Control{}, nil
	}
	return c, err
}

func (s *Store) Control(ctx context.Context) (Control, error) { return readControl(ctx, s.db) }

func writeControl(ctx context.Context, tx *sql.Tx, c Control) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO GO_COOKIE_CLOUD_CONTROL VALUES(1,?,?,?,?,?)
		ON CONFLICT(ID) DO UPDATE SET EPOCH=excluded.EPOCH,ACTOR=excluded.ACTOR,SCOPE=excluded.SCOPE,NEXT_RUN=excluded.NEXT_RUN,NEXT_ONCE=excluded.NEXT_ONCE`,
		c.Epoch, c.Actor, c.Scope, c.NextRun, c.NextOnce)
	if err != nil {
		return err
	}
	actual, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if actual != c {
		return ErrScope
	}
	return nil
}

func (s *Store) Activate(ctx context.Context, previous, next, expectedEpoch string, c Control) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = requireSettings(ctx, tx, previous); err != nil {
		return err
	}
	before, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if before.Epoch != expectedEpoch {
		return ErrScope
	}
	if err = stop(ctx, tx); err != nil {
		return err
	}
	if previous != next {
		if err = putValue(ctx, tx, ConfigKey, next); err != nil {
			return err
		}
	}
	if err = writeControl(ctx, tx, c); err != nil {
		return err
	}
	return tx.Commit()
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

const runSelect = `SELECT ID,KIND,STATE,ACTOR,SCOPE,EPOCH,STOP,CREATED,UPDATED,MESSAGE,ADDED,UPDATED_SITES,KEPT,SKIPPED FROM GO_COOKIE_CLOUD_RUNS`

func readRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Kind, &r.State, &r.Actor, &r.Scope, &r.Epoch, &r.Stop, &r.Created, &r.Updated, &r.Message, &r.Added, &r.UpdatedSites, &r.Kept, &r.Skipped)
	return r, err
}

func (s *Store) Run(ctx context.Context, id string) (Run, error) {
	return readRun(s.db.QueryRowContext(ctx, runSelect+` WHERE ID=?`, id))
}

func (s *Store) Runs(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` ORDER BY CREATED DESC,ID LIMIT 50`)
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

// Recover is called only while the cross-process execution lock is held.
func (s *Store) Recover(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE GO_COOKIE_CLOUD_RUNS SET STATE=CASE STOP WHEN 1 THEN 'stopped' ELSE 'unknown' END,
		MESSAGE='进程中断，请核对原同步结果',UPDATED=? WHERE STATE='running'`, timestamp())
	return err
}

func (s *Store) Start(ctx context.Context, run Run, raw string, old, next Control) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	if err = requireSettings(ctx, tx, raw); err != nil {
		return Run{}, err
	}
	if run.Epoch != "" {
		current, err := readControl(ctx, tx)
		if err != nil {
			return Run{}, err
		}
		if current != old || old.Epoch != run.Epoch {
			return Run{}, ErrScope
		}
		if err = writeControl(ctx, tx, next); err != nil {
			return Run{}, err
		}
	}
	now := timestamp()
	run.Created, run.Updated, run.State = now, now, "running"
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_COOKIE_CLOUD_RUNS VALUES(?,?,?,?,?,?,0,?,?,?,0,0,0,0) ON CONFLICT(ID) DO NOTHING`,
		run.ID, run.Kind, run.State, run.Actor, run.Scope, run.Epoch, now, now, "正在读取原服务")
	if err != nil {
		return Run{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Run{}, err
	}
	if n != 1 {
		return Run{}, ErrBusy
	}
	run.Message = "正在读取原服务"
	if actual, err := readRun(tx.QueryRowContext(ctx, runSelect+` WHERE ID=?`, run.ID)); err != nil || actual != run {
		return Run{}, ErrScope
	}
	return run, tx.Commit()
}

func stop(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM GO_COOKIE_CLOUD_CONTROL WHERE ID=1`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_COOKIE_CLOUD_RUNS SET STOP=1,UPDATED=? WHERE STATE='running'`, timestamp()); err != nil {
		return err
	}
	control, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	var stillRunning bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_COOKIE_CLOUD_RUNS WHERE STATE='running' AND STOP=0)`).Scan(&stillRunning); err != nil {
		return err
	}
	if control != (Control{}) || stillRunning {
		return ErrScope
	}
	return nil
}

func (s *Store) Stop(ctx context.Context, epoch, runID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if c.Epoch != epoch {
		return ErrScope
	}
	var current string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT ID FROM GO_COOKIE_CLOUD_RUNS WHERE STATE='running' ORDER BY CREATED DESC,ID LIMIT 1),'')`).Scan(&current); err != nil {
		return err
	}
	if current != runID {
		return ErrScope
	}
	if err = stop(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Complete(ctx context.Context, result Run, raw string, original []siteconfig.Site, changes []Candidate, notify bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := readRun(tx.QueryRowContext(ctx, runSelect+` WHERE ID=?`, result.ID))
	if err != nil {
		return err
	}
	if job.State != "running" || job.Actor != result.Actor || job.Scope != result.Scope || job.Epoch != result.Epoch || job.Kind != result.Kind {
		return ErrScope
	}
	if job.Stop {
		result.State, result.Message = "stopped", "已停止同步"
		changes = nil
		notify = false
	}
	if result.State == "complete" {
		if err = requireSettings(ctx, tx, raw); err != nil {
			return err
		}
		if job.Epoch != "" {
			control, err := readControl(ctx, tx)
			if err != nil {
				return err
			}
			if control.Epoch != job.Epoch || control.Scope != job.Scope {
				return ErrScope
			}
		}
	}
	if len(changes) > 0 {
		current, err := siteconfig.ListIn(ctx, tx)
		if err != nil {
			return err
		}
		if !slices.Equal(current, original) {
			return ErrScope
		}
		expected := make(map[int64]siteconfig.Site, len(original)+len(changes))
		var priority int64
		for _, item := range current {
			value, _ := strconv.ParseInt(item.Priority, 10, 64)
			priority = max(priority, value)
			expected[item.ID] = item
		}
		for _, change := range changes {
			site := change.Original
			if site.ID == 0 {
				if priority == math.MaxInt64 {
					return ErrScope
				}
				priority++
				res, err := tx.ExecContext(ctx, `INSERT INTO CONFIG_SITE(NAME,PRI,SIGNURL,COOKIE,INCLUDE,NOTE) VALUES(?,?,?,?,?,?)`,
					site.Name, strconv.FormatInt(priority, 10), site.SignURL, change.Cookie, "T", "{}")
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil || n != 1 {
					return ErrScope
				}
				site.ID, err = res.LastInsertId()
				if err != nil || site.ID <= 0 {
					return ErrScope
				}
				site.Priority, site.Note = strconv.FormatInt(priority, 10), "{}"
				result.Added++
			} else {
				res, err := tx.ExecContext(ctx, `UPDATE CONFIG_SITE SET COOKIE=? WHERE ID=?`, change.Cookie, site.ID)
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil || n != 1 {
					return ErrScope
				}
				result.UpdatedSites++
			}
			site.Cookie = change.Cookie
			expected[site.ID] = site
		}
		after, err := siteconfig.ListIn(ctx, tx)
		if err != nil || len(after) != len(expected) {
			return ErrScope
		}
		for _, item := range after {
			if expected[item.ID] != item {
				return ErrScope
			}
		}
	}
	if result.Kind != "test" && result.State == "complete" {
		result.Message = fmt.Sprintf("已更新 %d 个站点，新增 %d 个站点，保留 %d 个站点，跳过 %d 个站点", result.UpdatedSites, result.Added, result.Kept, result.Skipped)
	}
	result.Updated, result.Stop = timestamp(), job.Stop
	if _, err = tx.ExecContext(ctx, `UPDATE GO_COOKIE_CLOUD_RUNS SET STATE=?,UPDATED=?,MESSAGE=?,ADDED=?,UPDATED_SITES=?,KEPT=?,SKIPPED=? WHERE ID=?`,
		result.State, result.Updated, result.Message, result.Added, result.UpdatedSites, result.Kept, result.Skipped, result.ID); err != nil {
		return err
	}
	if notify {
		if err = bus.EmitData(ctx, tx, "plugin_message", result.ID, result.ID, "【CookieCloud同步任务执行完成】", result.Message, map[string]string{"plugin": Plugin}); err != nil {
			return err
		}
	}
	if actual, err := readRun(tx.QueryRowContext(ctx, runSelect+` WHERE ID=?`, result.ID)); err != nil || actual != result {
		return ErrScope
	}
	return tx.Commit()
}
