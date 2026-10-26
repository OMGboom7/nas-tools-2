package mediaserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const RefreshPluginID = "LibraryRefresh"
const RefreshPluginKey = "plugin.LibraryRefresh"
const RefreshControlKey = "GoLibraryRefresh"

var ErrRefreshState = errors.New("media refresh scope or receipt changed")
var ErrRefreshMissing = errors.New("media refresh receipt not found")

func RefreshDigest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func refreshHex(v string, n int) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(v) == n && len(b)*2 == n && strings.ToLower(v) == v
}

type RefreshControl struct {
	Epoch, Actor, Server, ServerID, ServerDigest, AuthorityDigest, PluginDigest, Created string
	Authorized                                                                           bool
	PlexAccount, PlexHTTP                                                                bool
	Delay                                                                                int64
}

func DecodeRefreshControl(raw string) (RefreshControl, error) {
	c := RefreshControl{}
	if raw == "" {
		return c, nil
	}
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &c) != nil || !refreshHex(c.Epoch, 32) || c.Delay < 0 || c.Delay > 2147483647 {
		return c, ErrRefreshState
	}
	if c.Authorized {
		if (c.PlexAccount || c.PlexHTTP) && c.Server != "plex" || c.PlexHTTP && !c.PlexAccount {
			return c, ErrRefreshState
		}
		_, err := time.Parse(time.RFC3339Nano, c.Created)
		if c.Actor == "" || !validInventoryID(c.ServerID) || c.Server != "emby" && c.Server != "jellyfin" && c.Server != "plex" || !refreshHex(c.ServerDigest, 64) || !refreshHex(c.AuthorityDigest, 64) || !refreshHex(c.PluginDigest, 64) || err != nil {
			return c, ErrRefreshState
		}
	}
	return c, nil
}

type refreshReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func refreshTable(ctx context.Context, r refreshReader, name string) (bool, error) {
	var yes bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&yes)
	return yes, err
}
func refreshSetting(ctx context.Context, r refreshReader, key string) (string, error) {
	yes, err := refreshTable(ctx, r, "SYSTEM_DICT")
	if err != nil || !yes {
		return "", err
	}
	var raw string
	err = r.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
func refreshControlCurrent(ctx context.Context, r refreshReader, c RefreshControl) error {
	raw, err := refreshSetting(ctx, r, RefreshControlKey)
	if err != nil {
		return err
	}
	current, err := DecodeRefreshControl(raw)
	if err != nil || !current.Authorized || current != c {
		return ErrRefreshState
	}
	raw, err = refreshSetting(ctx, r, RefreshPluginKey)
	if err != nil {
		return err
	}
	var v map[string]any
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &v) != nil || v["enable"] != true || RefreshDigest(v) != c.PluginDigest {
		return ErrRefreshState
	}
	raw, err = refreshSetting(ctx, r, "UserInstalledPlugins")
	if err != nil {
		return err
	}
	var ids []string
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &ids) != nil || len(ids) > 1024 {
		return ErrRefreshState
	}
	for _, id := range ids {
		if id == RefreshPluginID {
			return nil
		}
	}
	return ErrRefreshState
}

type RefreshDefinition struct {
	Actor, RequestID, Source, Reference string
	OriginDigest                        string
	Enrichment                          []RefreshDependency
	Control                             RefreshControl
	Items                               []RefreshItem
	Whole                               bool
	Reviewed                            *RefreshPlan
}

func RefreshJobID(d RefreshDefinition) string {
	return RefreshDigest([]string{"media-server-refresh", d.Source, d.Actor, d.RequestID})
}
func ValidRefreshPlan(p RefreshPlan) bool {
	if !validInventoryID(p.ServerID) || p.Server != "emby" && p.Server != "jellyfin" && p.Server != "plex" || p.Targets == nil || len(p.Targets) > RefreshTargetLimit {
		return false
	}
	seen := map[string]bool{}
	for _, t := range p.Targets {
		if !validInventoryID(t.ID) || len(t.Label) > 4096 {
			return false
		}
		if p.Server == "plex" {
			if t.Kind != "section" {
				return false
			}
			n, err := strconv.ParseInt(t.ID, 10, 64)
			if err != nil || n < 1 {
				return false
			}
			for _, c := range t.ID {
				if c < '0' || c > '9' {
					return false
				}
			}
		} else if t.Kind != "root" && !(p.Server == "emby" && t.Kind == "item") {
			return false
		}
		if t.Kind == "root" && (t.ID != "root" || t.Path != "") {
			return false
		}
		if t.Path != "" {
			if _, ok := RefreshPath(t.Path); !ok {
				return false
			}
		}
		k := t.Kind + "\x00" + t.ID + "\x00" + t.Path
		if seen[k] {
			return false
		}
		seen[k] = true
	}
	return true
}
func validRefreshDefinition(d RefreshDefinition) bool {
	if d.Actor == "" || !refreshHex(d.RequestID, 32) || d.Source != "transfer_finished" && d.Source != "refresh_media_server" || !d.Control.Authorized || d.Actor != d.Control.Actor {
		return false
	}
	if d.Whole && len(d.Items) != 0 || !d.Whole && !ValidRefreshItems(d.Items) {
		return false
	}
	if d.Source == "transfer_finished" && (!refreshHex(d.Reference, 32) || d.Whole || d.Reviewed != nil) {
		return false
	}
	if d.Reference != "" && !refreshHex(d.OriginDigest, 64) || len(d.Enrichment) > RefreshItemLimit {
		return false
	}
	for _, dep := range d.Enrichment {
		if !refreshHex(dep.ID, 64) || !refreshHex(dep.Origin, 64) {
			return false
		}
	}
	if d.Source == "refresh_media_server" && (d.Reviewed == nil || !ValidRefreshPlan(*d.Reviewed) || d.Reviewed.Server != d.Control.Server || d.Reviewed.ServerID != d.Control.ServerID) {
		return false
	}
	b, err := json.Marshal(d.Control)
	if err != nil {
		return false
	}
	_, err = DecodeRefreshControl(string(b))
	return err == nil
}

type RefreshAction struct {
	Index  int           `json:"index"`
	Target RefreshTarget `json:"target"`
	State  string        `json:"state"`
	Reason string        `json:"reason"`
}
type RefreshJob struct {
	ID          string            `json:"id"`
	State       string            `json:"state"`
	Reason      string            `json:"reason"`
	Created     string            `json:"created"`
	Due         string            `json:"due"`
	Actor       string            `json:"actor"`
	Source      string            `json:"source"`
	Reference   string            `json:"reference"`
	Server      string            `json:"server"`
	Fingerprint string            `json:"fingerprint"`
	Plan        *RefreshPlan      `json:"plan"`
	Actions     []RefreshAction   `json:"actions"`
	Definition  RefreshDefinition `json:"-"`
}
type RefreshStore struct{ db *sql.DB }

func NewRefreshStore(db *sql.DB) *RefreshStore { return &RefreshStore{db: db} }
func refreshSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_MEDIA_REFRESH_JOBS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,PLAN TEXT NOT NULL DEFAULT '',STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',CREATED TEXT NOT NULL,DUE TEXT NOT NULL,NEXT_CHECK TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS GO_MEDIA_REFRESH_ACTIONS(JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,TARGET TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',PRIMARY KEY(JOB_ID,ORDINAL))`,
		`CREATE INDEX IF NOT EXISTS GO_MEDIA_REFRESH_PENDING ON GO_MEDIA_REFRESH_JOBS(STATE,DUE,ID)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func readRefreshJob(ctx context.Context, r refreshReader, id string) (RefreshJob, error) {
	j := RefreshJob{Actions: []RefreshAction{}}
	yes, err := refreshTable(ctx, r, "GO_MEDIA_REFRESH_JOBS")
	if err != nil {
		return j, err
	}
	if !yes {
		return j, ErrRefreshMissing
	}
	var raw, plan string
	err = r.QueryRowContext(ctx, `SELECT ID,DOCUMENT,FINGERPRINT,PLAN,STATE,REASON,CREATED,DUE FROM GO_MEDIA_REFRESH_JOBS WHERE ID=?`, id).Scan(&j.ID, &raw, &j.Fingerprint, &plan, &j.State, &j.Reason, &j.Created, &j.Due)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrRefreshMissing
	}
	if err != nil {
		return j, err
	}
	if len(raw) > 8<<20 || json.Unmarshal([]byte(raw), &j.Definition) != nil || !validRefreshDefinition(j.Definition) || RefreshJobID(j.Definition) != id || RefreshDigest(j.Definition) != j.Fingerprint {
		return j, ErrRefreshState
	}
	switch j.State {
	case "pending", "planning", "running", "accepted", "not_needed", "blocked", "uncertain", "stopped":
	default:
		return j, ErrRefreshState
	}
	j.Actor, j.Source, j.Reference, j.Server = j.Definition.Actor, j.Definition.Source, j.Definition.Reference, j.Definition.Control.Server
	if plan != "" {
		var p RefreshPlan
		if len(plan) > 16<<20 || json.Unmarshal([]byte(plan), &p) != nil || !ValidRefreshPlan(p) || p.Server != j.Server || p.ServerID != j.Definition.Control.ServerID || j.Definition.Reviewed != nil && RefreshDigest(j.Definition.Reviewed) != RefreshDigest(p) {
			return j, ErrRefreshState
		}
		j.Plan = &p
	}
	rows, err := r.QueryContext(ctx, `SELECT ORDINAL,TARGET,STATE,REASON FROM GO_MEDIA_REFRESH_ACTIONS WHERE JOB_ID=? ORDER BY ORDINAL`, id)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	for rows.Next() {
		a := RefreshAction{}
		if err := rows.Scan(&a.Index, &raw, &a.State, &a.Reason); err != nil {
			return j, err
		}
		if j.Plan == nil || a.Index != len(j.Actions) || a.Index >= len(j.Plan.Targets) || json.Unmarshal([]byte(raw), &a.Target) != nil || a.Target != j.Plan.Targets[a.Index] {
			return j, ErrRefreshState
		}
		switch a.State {
		case "pending", "intent", "accepted", "uncertain":
		default:
			return j, ErrRefreshState
		}
		j.Actions = append(j.Actions, a)
	}
	if err := rows.Err(); err != nil {
		return j, err
	}
	if j.Plan != nil && len(j.Actions) != len(j.Plan.Targets) {
		return j, ErrRefreshState
	}
	if j.State == "accepted" || j.State == "not_needed" {
		if j.Plan == nil || j.State == "not_needed" && len(j.Actions) != 0 || j.State == "accepted" && len(j.Actions) == 0 {
			return j, ErrRefreshState
		}
		for _, a := range j.Actions {
			if a.State != "accepted" {
				return j, ErrRefreshState
			}
		}
	}
	return j, nil
}
func createRefreshJob(ctx context.Context, tx *sql.Tx, d RefreshDefinition, now time.Time) (RefreshJob, error) {
	if !validRefreshDefinition(d) {
		return RefreshJob{}, ErrRefreshState
	}
	id := RefreshJobID(d)
	if old, err := readRefreshJob(ctx, tx, id); err == nil {
		if old.Fingerprint != RefreshDigest(d) {
			return RefreshJob{}, ErrRefreshState
		}
		return old, nil
	} else if !errors.Is(err, ErrRefreshMissing) {
		return RefreshJob{}, err
	}
	if err := refreshControlCurrent(ctx, tx, d.Control); err != nil {
		return RefreshJob{}, err
	}
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 8<<20 {
		return RefreshJob{}, ErrRefreshState
	}
	if err := refreshSchema(ctx, tx); err != nil {
		return RefreshJob{}, err
	}
	due := now
	if d.Source == "transfer_finished" {
		due = now.Add(time.Duration(d.Control.Delay) * time.Second)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_REFRESH_JOBS(ID,DOCUMENT,FINGERPRINT,STATE,CREATED,DUE)VALUES(?,?,?,'pending',?,?)`, id, string(raw), RefreshDigest(d), now.UTC().Format(refreshTimestamp), due.UTC().Format(refreshTimestamp))
	if err != nil {
		return RefreshJob{}, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return RefreshJob{}, ErrRefreshState
	}
	return readRefreshJob(ctx, tx, id)
}

// Only newly committed, fully successful native intake events can enqueue
// automatic work. Enabling the plugin never adopts old text/history events.
func EnqueueTransferRefresh(ctx context.Context, tx *sql.Tx, job string, items []RefreshItem) error {
	raw, err := refreshSetting(ctx, tx, RefreshControlKey)
	if err != nil {
		return err
	}
	c, err := DecodeRefreshControl(raw)
	if err != nil {
		return err
	}
	if !c.Authorized {
		return nil
	}
	if err := refreshControlCurrent(ctx, tx, c); errors.Is(err, ErrRefreshState) {
		return nil
	} else if err != nil {
		return err
	}
	var created string
	err = tx.QueryRowContext(ctx, `SELECT CREATED FROM GO_BUSINESS_EVENTS WHERE ID=? AND KIND='transfer_finished'`, RefreshDigest([]string{"transfer_finished", job})).Scan(&created)
	if err != nil {
		return err
	}
	emitted, e1 := time.Parse(time.RFC3339Nano, created)
	activated, e2 := time.Parse(time.RFC3339Nano, c.Created)
	if e1 != nil || e2 != nil {
		return ErrRefreshState
	}
	if emitted.Before(activated) {
		return nil
	}
	d := RefreshDefinition{Actor: c.Actor, RequestID: RefreshDigest([]string{job, c.Epoch})[:32], Source: "transfer_finished", Reference: job, Control: c, Items: items}
	d, err = bindRefreshSource(ctx, tx, d)
	if err != nil {
		return err
	}
	_, err = createRefreshJob(ctx, tx, d, emitted)
	return err
}
func (s *RefreshStore) Get(ctx context.Context, id string) (RefreshJob, error) {
	if !refreshHex(id, 64) {
		return RefreshJob{}, ErrRefreshState
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RefreshJob{}, err
	}
	defer tx.Rollback()
	return readRefreshJob(ctx, tx, id)
}
func (s *RefreshStore) Create(ctx context.Context, d RefreshDefinition) (RefreshJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RefreshJob{}, err
	}
	defer tx.Rollback()
	j, err := createRefreshJob(ctx, tx, d, time.Now())
	if err != nil {
		return j, err
	}
	return j, tx.Commit()
}
func (s *RefreshStore) List(ctx context.Context, pending bool, now time.Time) ([]RefreshJob, error) {
	out := []RefreshJob{}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	yes, err := refreshTable(ctx, tx, "GO_MEDIA_REFRESH_JOBS")
	if err != nil || !yes {
		return out, err
	}
	q := `SELECT ID FROM GO_MEDIA_REFRESH_JOBS ORDER BY CREATED DESC,ID LIMIT 30`
	args := []any{}
	if pending {
		q = `SELECT ID FROM GO_MEDIA_REFRESH_JOBS WHERE STATE='pending' AND DUE<=? AND (NEXT_CHECK='' OR NEXT_CHECK<=?) ORDER BY CASE WHEN NEXT_CHECK='' THEN DUE ELSE NEXT_CHECK END,ID LIMIT 5`
		args = append(args, now.UTC().Format(refreshTimestamp), now.UTC().Format(refreshTimestamp))
	}
	rows, err := tx.QueryContext(ctx, q, args...)
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
	for _, id := range ids {
		j, err := readRefreshJob(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, nil
}
func (s *RefreshStore) Start(ctx context.Context, id string) (RefreshJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RefreshJob{}, err
	}
	defer tx.Rollback()
	j, err := readRefreshJob(ctx, tx, id)
	if err != nil {
		return j, err
	}
	if err := refreshControlCurrent(ctx, tx, j.Definition.Control); err != nil {
		return j, err
	}
	if j.State != "pending" {
		return j, ErrRefreshState
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REFRESH_JOBS SET STATE='planning' WHERE ID=? AND STATE='pending'`, id)
	if err != nil {
		return j, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return j, ErrRefreshState
	}
	j.State = "planning"
	return j, tx.Commit()
}
func (s *RefreshStore) Plan(ctx context.Context, id string, p RefreshPlan) error {
	if !ValidRefreshPlan(p) {
		return ErrRefreshState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readRefreshJob(ctx, tx, id)
	if err != nil {
		return err
	}
	if j.State != "planning" || j.Server != p.Server || j.Definition.Control.ServerID != p.ServerID || j.Definition.Reviewed != nil && RefreshDigest(j.Definition.Reviewed) != RefreshDigest(p) {
		return ErrRefreshState
	}
	if err := refreshControlCurrent(ctx, tx, j.Definition.Control); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REFRESH_JOBS SET PLAN=?,STATE='running' WHERE ID=? AND STATE='planning' AND PLAN=''`, string(b), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrRefreshState
	}
	for i, t := range p.Targets {
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_REFRESH_ACTIONS(JOB_ID,ORDINAL,TARGET,STATE)VALUES(?,?,?,'pending')`, id, i, string(b))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return ErrRefreshState
		}
	}
	return tx.Commit()
}

const refreshTimestamp = "2006-01-02T15:04:05.000000000Z"

func (s *RefreshStore) Active(ctx context.Context, id string) (bool, error) {
	if !refreshHex(id, 64) {
		return false, ErrRefreshState
	}
	var active bool
	err := s.db.QueryRowContext(ctx, `SELECT STATE IN('planning','running') FROM GO_MEDIA_REFRESH_JOBS WHERE ID=?`, id).Scan(&active)
	return active, err
}
func (s *RefreshStore) Intent(ctx context.Context, id string, index int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readRefreshJob(ctx, tx, id)
	if err != nil {
		return err
	}
	if j.State != "running" || index < 0 || index >= len(j.Actions) || j.Actions[index].State != "pending" {
		return ErrRefreshState
	}
	if err := refreshControlCurrent(ctx, tx, j.Definition.Control); err != nil {
		return err
	}
	ready, err := refreshSourceReady(ctx, tx, j.Definition)
	if err != nil || !ready {
		return ErrRefreshState
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REFRESH_ACTIONS SET STATE='intent' WHERE JOB_ID=? AND ORDINAL=? AND STATE='pending'`, id, index)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrRefreshState
	}
	return tx.Commit()
}
func (s *RefreshStore) ActionResult(ctx context.Context, id string, index int, accepted bool) error {
	state, reason := "uncertain", "server response not positively acknowledged; no automatic retry"
	if accepted {
		state, reason = "accepted", "server accepted the request; scan completion is not proven"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE GO_MEDIA_REFRESH_ACTIONS SET STATE=?,REASON=? WHERE JOB_ID=? AND ORDINAL=? AND STATE='intent'`, state, reason, id, index)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrRefreshState
	}
	return nil
}
func (s *RefreshStore) Finish(ctx context.Context, id, state, reason string) error {
	switch state {
	case "accepted", "not_needed", "blocked", "uncertain", "stopped":
	default:
		return ErrRefreshState
	}
	if len(reason) > 1000 {
		return ErrRefreshState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readRefreshJob(ctx, tx, id)
	if err != nil {
		return err
	}
	if j.State == "stopped" {
		return nil
	}
	if state == "accepted" || state == "not_needed" {
		if j.Plan == nil || j.State != "running" {
			return ErrRefreshState
		}
		for _, a := range j.Actions {
			if a.State != "accepted" {
				return ErrRefreshState
			}
		}
		if state == "not_needed" && len(j.Actions) != 0 {
			return ErrRefreshState
		}
		if state == "accepted" && len(j.Actions) == 0 {
			return ErrRefreshState
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_REFRESH_JOBS SET STATE=?,REASON=? WHERE ID=? AND STATE IN('pending','planning','running')`, state, reason, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrRefreshState
	}
	return tx.Commit()
}
