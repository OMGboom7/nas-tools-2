package sitestats

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	_ "modernc.org/sqlite"
)

var ErrState = errors.New("原统计配置或执行状态已变化，请重新读取")

type Store struct{ db *sql.DB }
type Control struct {
	Created      string `json:"created"`
	Enabled      bool   `json:"enabled"`
	Epoch        string `json:"epoch"`
	Actor        string `json:"actor"`
	Authority    string `json:"-"`
	Fingerprint  string `json:"fingerprint"`
	Cron         string `json:"cron"`
	Location     string `json:"location"`
	ReadMessages bool   `json:"readMessages"`
	Remote       bool   `json:"remote"`
	NextRun      string `json:"nextRun"`
}
type Result struct {
	SiteID string `json:"siteId"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}
type Job struct {
	ID              string   `json:"id"`
	Origin          string   `json:"origin"`
	Actor           string   `json:"actor"`
	Epoch           string   `json:"epoch"`
	Fingerprint     string   `json:"fingerprint"`
	Authority       string   `json:"-"`
	State           string   `json:"state"`
	StopRequested   bool     `json:"stopRequested"`
	Created         string   `json:"created"`
	Updated         string   `json:"updated"`
	SiteIDs         []string `json:"siteIds"`
	ReadMessages    bool     `json:"readMessages"`
	Summary         bool     `json:"summary"`
	Requests        int      `json:"requests"`
	MessageRequests int      `json:"messageRequests"`
	Results         []Result `json:"results"`
}

// Persist authority privately: exported views deliberately exclude it.
type controlDocument struct {
	Control
	PrivateAuthority string `json:"privateAuthority"`
}
type jobDocument struct {
	Job
	PrivateAuthority string `json:"privateAuthority"`
}

func Hash(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func Open(path string) (*Store, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	for _, q := range []string{
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS SITE_USER_INFO_STATS (ID INTEGER PRIMARY KEY AUTOINCREMENT,SITE TEXT,USERNAME TEXT,USER_LEVEL TEXT,JOIN_AT TEXT,LAST_SEEN TEXT,UPDATE_AT TEXT,UPLOAD INTEGER,DOWNLOAD INTEGER,RATIO REAL,SEEDING INTEGER,LEECHING INTEGER,SEEDING_SIZE INTEGER,BONUS REAL,URL TEXT UNIQUE,MSG_UNREAD INTEGER,EXT_INFO TEXT)`,
		`CREATE TABLE IF NOT EXISTS SITE_USER_SEEDING_INFO (ID INTEGER PRIMARY KEY AUTOINCREMENT,SITE TEXT,SEEDING_INFO TEXT DEFAULT '[]',UPDATE_AT TEXT,URL TEXT UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS SITE_STATISTICS_HISTORY (ID INTEGER PRIMARY KEY AUTOINCREMENT,SITE TEXT,DATE TEXT,USER_LEVEL TEXT,UPLOAD TEXT,DOWNLOAD TEXT,RATIO TEXT,SEEDING INTEGER DEFAULT 0,LEECHING INTEGER DEFAULT 0,SEEDING_SIZE INTEGER DEFAULT 0,BONUS REAL DEFAULT 0,URL TEXT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS UN_INDX_SITE_STATISTICS_HISTORY_DS ON SITE_STATISTICS_HISTORY(DATE,URL)`,
		`CREATE TABLE IF NOT EXISTS SITE_FAVICON (SITE TEXT PRIMARY KEY,URL TEXT,FAVICON TEXT)`,
		`CREATE TABLE IF NOT EXISTS GO_SITE_STATS_CONTROL (ID INTEGER PRIMARY KEY CHECK(ID=1),DOCUMENT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SITE_STATS_JOBS (ID TEXT PRIMARY KEY,ACTOR TEXT NOT NULL,ORIGIN TEXT NOT NULL,STATE TEXT NOT NULL,DOCUMENT TEXT NOT NULL,CREATED TEXT NOT NULL,UNIQUE(ACTOR,ORIGIN))`,
		`CREATE TABLE IF NOT EXISTS GO_SITE_STATS_META (URL TEXT PRIMARY KEY,SCHEMA TEXT NOT NULL)`,
	} {
		if _, e = db.Exec(q); e != nil {
			db.Close()
			return nil, e
		}
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }

type reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readControl(ctx context.Context, r reader) (Control, error) {
	var raw string
	e := r.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_SITE_STATS_CONTROL WHERE ID=1`).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return Control{}, nil
	}
	if e != nil {
		return Control{}, e
	}
	var d controlDocument
	if json.Unmarshal([]byte(raw), &d) != nil {
		return Control{}, ErrState
	}
	d.Authority = d.PrivateAuthority
	return d.Control, nil
}
func (s *Store) Control(ctx context.Context) (Control, error) { return readControl(ctx, s.db) }
func putControl(ctx context.Context, tx *sql.Tx, c Control) error {
	b, e := json.Marshal(controlDocument{c, c.Authority})
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO GO_SITE_STATS_CONTROL(ID,DOCUMENT)VALUES(1,?) ON CONFLICT(ID)DO UPDATE SET DOCUMENT=excluded.DOCUMENT`, string(b))
	return e
}
func (s *Store) SetControl(ctx context.Context, c Control, expected string) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	old, e := readControl(ctx, tx)
	if e != nil || old.Epoch != expected {
		return ErrState
	}
	if e = putControl(ctx, tx, c); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, `SELECT ID FROM GO_SITE_STATS_JOBS WHERE STATE IN('running','queued')`)
	if e != nil {
		return e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		j, e := readJob(ctx, tx, id)
		if e != nil {
			return e
		}
		j.StopRequested = true
		if e = putJob(ctx, tx, j); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) Valid(ctx context.Context, c Control) error {
	old, e := s.Control(ctx)
	if e != nil || !old.Enabled || old.Epoch != c.Epoch || old.Fingerprint != c.Fingerprint || old.Authority != c.Authority {
		return ErrState
	}
	return nil
}
func (s *Store) Advance(ctx context.Context, c Control, next string) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	old, e := readControl(ctx, tx)
	if e != nil || !old.Enabled || old.Epoch != c.Epoch || old.NextRun != c.NextRun {
		return ErrState
	}
	old.NextRun = next
	if e = putControl(ctx, tx, old); e != nil {
		return e
	}
	return tx.Commit()
}
func readJob(ctx context.Context, r reader, id string) (Job, error) {
	var raw string
	if e := r.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_SITE_STATS_JOBS WHERE ID=?`, id).Scan(&raw); e != nil {
		return Job{}, e
	}
	var d jobDocument
	if json.Unmarshal([]byte(raw), &d) != nil {
		return Job{}, ErrState
	}
	d.Authority = d.PrivateAuthority
	return d.Job, nil
}
func (s *Store) Get(ctx context.Context, id string) (Job, error) { return readJob(ctx, s.db, id) }
func putJob(ctx context.Context, tx *sql.Tx, j Job) error {
	j.Updated = stamp()
	b, e := json.Marshal(jobDocument{j, j.Authority})
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO GO_SITE_STATS_JOBS(ID,ACTOR,ORIGIN,STATE,DOCUMENT,CREATED)VALUES(?,?,?,?,?,?) ON CONFLICT(ID)DO UPDATE SET STATE=excluded.STATE,DOCUMENT=excluded.DOCUMENT`, j.ID, j.Actor, j.Origin, j.State, string(b), j.Created)
	return e
}
func (s *Store) Create(ctx context.Context, j Job) (Job, bool, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return j, false, e
	}
	defer tx.Rollback()
	if old, e := readJob(ctx, tx, j.ID); e == nil {
		return old, true, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return j, false, e
	}
	j.State = "queued"
	j.Created = stamp()
	j.Updated = j.Created
	j.Results = []Result{}
	if e = putJob(ctx, tx, j); e != nil {
		return j, false, e
	}
	e = tx.Commit()
	return j, false, e
}
func (s *Store) Jobs(ctx context.Context) ([]Job, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT DOCUMENT FROM GO_SITE_STATS_JOBS ORDER BY CREATED DESC LIMIT 100`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var d jobDocument
		if json.Unmarshal([]byte(raw), &d) != nil {
			return nil, ErrState
		}
		d.Authority = d.PrivateAuthority
		out = append(out, d.Job)
	}
	return out, rows.Err()
}
func (s *Store) Change(ctx context.Context, id string, f func(*Job) error) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	j, e := readJob(ctx, tx, id)
	if e != nil {
		return e
	}
	if e = f(&j); e != nil {
		return e
	}
	if e = putJob(ctx, tx, j); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Recover(ctx context.Context) error {
	jobs, e := s.Jobs(ctx)
	if e != nil {
		return e
	}
	for _, j := range jobs {
		if j.State == "running" || j.State == "queued" {
			if e = s.Change(ctx, j.ID, func(v *Job) error { v.State = "unknown"; return nil }); e != nil {
				return e
			}
		}
	}
	return nil
}

// Save publishes all three legacy tables and outgoing events atomically.
func (s *Store) Save(ctx context.Context, j Job, v Snapshot, day, updated string, notify bool, expected ...siteconfig.Site) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	current, e := readJob(ctx, tx, j.ID)
	if e != nil || current.State != "running" || current.StopRequested {
		return ErrState
	}
	if len(expected) > 0 {
		want := expected[0]
		var actual siteconfig.Site
		e = tx.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(PRI,''),COALESCE(RSSURL,''),COALESCE(SIGNURL,''),COALESCE(COOKIE,''),COALESCE(APIKEY,''),COALESCE(INCLUDE,''),COALESCE(EXCLUDE,''),COALESCE(SIZE,''),COALESCE(NOTE,'')FROM CONFIG_SITE WHERE ID=?`, want.ID).Scan(&actual.ID, &actual.Name, &actual.Priority, &actual.RSSURL, &actual.SignURL, &actual.Cookie, &actual.APIKey, &actual.Include, &actual.Exclude, &actual.Size, &actual.Note)
		if e != nil || Hash(actual) != Hash(want) {
			return ErrState
		}
	}
	if j.Epoch != "" {
		c, e := readControl(ctx, tx)
		if e != nil || !c.Enabled || c.Epoch != j.Epoch || c.Fingerprint != j.Fingerprint {
			return ErrState
		}
	}
	var unread int64 = -1
	e = tx.QueryRowContext(ctx, `SELECT COALESCE(MSG_UNREAD,0) FROM SITE_USER_INFO_STATS WHERE URL=?`, v.URL).Scan(&unread)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO SITE_USER_INFO_STATS(SITE,USERNAME,USER_LEVEL,JOIN_AT,LAST_SEEN,UPDATE_AT,UPLOAD,DOWNLOAD,RATIO,SEEDING,LEECHING,SEEDING_SIZE,BONUS,URL,MSG_UNREAD) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(URL) DO UPDATE SET SITE=excluded.SITE,USERNAME=excluded.USERNAME,USER_LEVEL=excluded.USER_LEVEL,JOIN_AT=excluded.JOIN_AT,LAST_SEEN=excluded.LAST_SEEN,UPDATE_AT=excluded.UPDATE_AT,UPLOAD=excluded.UPLOAD,DOWNLOAD=excluded.DOWNLOAD,RATIO=excluded.RATIO,SEEDING=excluded.SEEDING,LEECHING=excluded.LEECHING,SEEDING_SIZE=excluded.SEEDING_SIZE,BONUS=excluded.BONUS,MSG_UNREAD=excluded.MSG_UNREAD`, v.Site, v.Username, v.Level, v.Joined, v.LastSeen, updated, v.Upload, v.Download, v.Ratio, v.Seeding, v.Leeching, v.SeedingSize, v.Bonus, v.URL, v.Unread)
	if e != nil {
		return e
	}
	seeds, e := json.Marshal(v.Seeds)
	if e != nil {
		return e
	}
	if v.SeedDetailsKnown {
		if _, e = tx.ExecContext(ctx, `INSERT INTO SITE_USER_SEEDING_INFO(SITE,SEEDING_INFO,UPDATE_AT,URL)VALUES(?,?,?,?)ON CONFLICT(URL)DO UPDATE SET SITE=excluded.SITE,SEEDING_INFO=excluded.SEEDING_INFO,UPDATE_AT=excluded.UPDATE_AT`, v.Site, string(seeds), updated, v.URL); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO SITE_STATISTICS_HISTORY(SITE,DATE,USER_LEVEL,UPLOAD,DOWNLOAD,RATIO,SEEDING,LEECHING,SEEDING_SIZE,BONUS,URL)VALUES(?,?,?,?,?,?,?,?,?,?,?)ON CONFLICT(DATE,URL)DO UPDATE SET SITE=excluded.SITE,USER_LEVEL=excluded.USER_LEVEL,UPLOAD=excluded.UPLOAD,DOWNLOAD=excluded.DOWNLOAD,RATIO=excluded.RATIO,SEEDING=excluded.SEEDING,LEECHING=excluded.LEECHING,SEEDING_SIZE=excluded.SEEDING_SIZE,BONUS=excluded.BONUS`, v.Site, day, v.Level, v.Upload, v.Download, v.Ratio, v.Seeding, v.Leeching, v.SeedingSize, v.Bonus, v.URL); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO GO_SITE_STATS_META(URL,SCHEMA)VALUES(?,?)ON CONFLICT(URL)DO UPDATE SET SCHEMA=excluded.SCHEMA`, v.URL, v.Schema); e != nil {
		return e
	}
	if v.Favicon != "" {
		if _, e = tx.ExecContext(ctx, `INSERT INTO SITE_FAVICON(SITE,URL,FAVICON)VALUES(?,?,?)ON CONFLICT(SITE)DO UPDATE SET URL=excluded.URL,FAVICON=excluded.FAVICON`, v.Site, v.URL, v.Favicon); e != nil {
			return e
		}
	}
	if notify && v.Unread > 0 && v.Unread != unread {
		messages := v.Messages
		if len(messages) == 0 {
			messages = []Message{{Title: "收到 " + stringInt(v.Unread) + " 条新消息，请登录查看"}}
		}
		for i, m := range messages {
			title := "【站点 " + v.Site + " 消息】"
			body := m.Title
			if m.Content != "" {
				body = "时间：" + m.Date + "\n标题：" + m.Title + "\n内容：\n" + m.Content
			}
			if e = bus.Emit(ctx, tx, "site_message", Hash([]any{j.ID, v.URL, i}), j.ID, title, body); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
