package organization

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type MonitorLease struct {
	Remote          *RemoteAutomation `json:"remote,omitempty"`
	Generation      string            `json:"generation"`
	Actor           string            `json:"actor"`
	IDs             []string          `json:"ids"`
	ConfigDigest    string            `json:"configDigest"`
	GraphDigest     string            `json:"graphDigest"`
	Roots           []string          `json:"roots"`
	IncludeExisting bool              `json:"includeExisting"`
	SourceRemoval   bool              `json:"sourceRemoval"`
}
type MonitorControl struct {
	State  string       `json:"state"`
	Lease  MonitorLease `json:"lease"`
	Reason string       `json:"reason,omitempty"`
}
type MonitorSnapshot struct {
	ConfigID string `json:"configId"`
	Scope    string `json:"scope"`
	Files    []File `json:"files"`
}
type MonitorEvent struct {
	Key        string          `json:"key"`
	Generation string          `json:"generation"`
	Snapshot   MonitorSnapshot `json:"snapshot"`
	FirstSeen  string          `json:"firstSeen"`
	State      string          `json:"state"`
	Jobs       []string        `json:"jobs"`
	Reason     string          `json:"reason,omitempty"`
	Updated    string          `json:"updated"`
}
type syncWriter interface {
	syncReader
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func monitorTables(ctx context.Context, w syncWriter) error {
	for _, statement := range []string{`CREATE TABLE IF NOT EXISTS GO_SYNC_MONITOR_CONTROL(ID INTEGER PRIMARY KEY,DOCUMENT TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '')`, `CREATE TABLE IF NOT EXISTS GO_SYNC_MONITOR_EVENTS(EVENT_KEY TEXT PRIMARY KEY,GENERATION TEXT NOT NULL,CONFIG_ID TEXT NOT NULL,SCOPE TEXT NOT NULL,SNAPSHOT TEXT NOT NULL,FIRST_SEEN TEXT NOT NULL,STATE TEXT NOT NULL,JOBS TEXT NOT NULL DEFAULT '[]',REASON TEXT NOT NULL DEFAULT '',UPDATED TEXT NOT NULL)`} {
		if _, err := w.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
func readMonitorControl(ctx context.Context, r unknownReader) (MonitorControl, error) {
	control := MonitorControl{State: "disabled"}
	var exists bool
	if err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SYNC_MONITOR_CONTROL')`).Scan(&exists); err != nil || !exists {
		return control, err
	}
	var raw string
	err := r.QueryRowContext(ctx, `SELECT DOCUMENT,STATE,REASON FROM GO_SYNC_MONITOR_CONTROL WHERE ID=1`).Scan(&raw, &control.State, &control.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return MonitorControl{State: "disabled"}, nil
	}
	if err != nil {
		return control, err
	}
	if json.Unmarshal([]byte(raw), &control.Lease) != nil {
		return control, ErrState
	}
	return control, nil
}
func (s *Store) MonitorControl(ctx context.Context) (MonitorControl, error) {
	return readMonitorControl(ctx, s.db)
}
func verifyMonitorControl(ctx context.Context, r unknownReader, generation string) error {
	control, err := readMonitorControl(ctx, r)
	if err != nil {
		return err
	}
	if generation == "" || control.State != "enabled" || control.Lease.Generation != generation {
		return ErrState
	}
	return nil
}
func (s *Store) VerifyMonitorControl(ctx context.Context, generation string) error {
	return verifyMonitorControl(ctx, s.db, generation)
}
func pauseMonitorControl(ctx context.Context, w syncWriter, reason string) error {
	control, err := readMonitorControl(ctx, w)
	if err != nil {
		return err
	}
	if control.State != "enabled" {
		return nil
	}
	_, err = w.ExecContext(ctx, `UPDATE GO_SYNC_MONITOR_CONTROL SET STATE='paused',REASON=? WHERE ID=1 AND STATE='enabled'`, reason)
	return err
}
func (s *Store) StopSyncMonitor(ctx context.Context, actor string) error {
	return pauseMonitorControl(ctx, s.db, "stop requested by "+actor+"; active steps require ledger review")
}
func (s *Store) PauseSyncMonitor(ctx context.Context, reason string) error {
	return pauseMonitorControl(ctx, s.db, reason)
}
func MonitorEventKey(generation string, snapshot MonitorSnapshot) string {
	return Digest(struct {
		Generation string
		Snapshot   MonitorSnapshot
	}{generation, snapshot})
}
func (s *Store) EnableSyncMonitor(ctx context.Context, lease MonitorLease, snapshots []MonitorSnapshot, records []SyncRecord, now time.Time) (MonitorControl, error) {
	if lease.Remote != nil && !lease.Remote.Valid() {
		return MonitorControl{}, ErrState
	}
	if lease.Actor == "" || len(lease.IDs) == 0 || len(lease.IDs) > 16 || len(snapshots) > 2048 {
		return MonitorControl{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MonitorControl{}, err
	}
	defer tx.Rollback()
	current, err := listSync(ctx, tx)
	if err != nil {
		return MonitorControl{}, err
	}
	if Digest(current) != Digest(records) || SyncGraphDigest(current) != lease.GraphDigest {
		return MonitorControl{}, ErrState
	}
	if err := monitorTables(ctx, tx); err != nil {
		return MonitorControl{}, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return MonitorControl{}, err
	}
	lease.Generation = hex.EncodeToString(random[:])
	raw, err := json.Marshal(lease)
	if err != nil {
		return MonitorControl{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_SYNC_MONITOR_CONTROL VALUES(1,?,'enabled','') ON CONFLICT(ID) DO UPDATE SET DOCUMENT=excluded.DOCUMENT,STATE='enabled',REASON=''`, string(raw)); err != nil {
		return MonitorControl{}, err
	}
	state := "baseline"
	if lease.IncludeExisting {
		state = "observed"
	}
	for _, snapshot := range snapshots {
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return MonitorControl{}, err
		}
		stamp := now.UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_SYNC_MONITOR_EVENTS(EVENT_KEY,GENERATION,CONFIG_ID,SCOPE,SNAPSHOT,FIRST_SEEN,STATE,UPDATED) VALUES(?,?,?,?,?,?,?,?)`, MonitorEventKey(lease.Generation, snapshot), lease.Generation, snapshot.ConfigID, snapshot.Scope, string(raw), stamp, state, stamp); err != nil {
			return MonitorControl{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return MonitorControl{}, err
	}
	return MonitorControl{State: "enabled", Lease: lease}, nil
}
func (s *Store) ObserveSyncMonitor(ctx context.Context, lease MonitorLease, snapshots []MonitorSnapshot, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyMonitorControl(ctx, tx, lease.Generation); err != nil {
		return err
	}
	keys := map[string]bool{}
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, snapshot := range snapshots {
		key := MonitorEventKey(lease.Generation, snapshot)
		keys[key] = true
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_SYNC_MONITOR_EVENTS(EVENT_KEY,GENERATION,CONFIG_ID,SCOPE,SNAPSHOT,FIRST_SEEN,STATE,UPDATED) VALUES(?,?,?,?,?,?,'observed',?) ON CONFLICT(EVENT_KEY) DO NOTHING`, key, lease.Generation, snapshot.ConfigID, snapshot.Scope, string(raw), stamp, stamp); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT EVENT_KEY FROM GO_SYNC_MONITOR_EVENTS WHERE GENERATION=? AND STATE='observed'`, lease.Generation)
	if err != nil {
		return err
	}
	old := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		if !keys[key] {
			old = append(old, key)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, key := range old {
		if _, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_MONITOR_EVENTS SET STATE='superseded',REASON='source changed or disappeared before reservation',UPDATED=? WHERE EVENT_KEY=? AND STATE='observed'`, stamp, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func scanMonitorEvent(row interface{ Scan(...any) error }) (MonitorEvent, error) {
	var e MonitorEvent
	var raw, jobs string
	err := row.Scan(&e.Key, &e.Generation, &raw, &e.FirstSeen, &e.State, &jobs, &e.Reason, &e.Updated)
	if err != nil {
		return e, err
	}
	if json.Unmarshal([]byte(raw), &e.Snapshot) != nil || json.Unmarshal([]byte(jobs), &e.Jobs) != nil || MonitorEventKey(e.Generation, e.Snapshot) != e.Key {
		return e, ErrState
	}
	return e, nil
}

const monitorEventColumns = `EVENT_KEY,GENERATION,SNAPSHOT,FIRST_SEEN,STATE,JOBS,REASON,UPDATED`

func (s *Store) MonitorEvents(ctx context.Context, generation string, pending bool) ([]MonitorEvent, error) {
	items := []MonitorEvent{}
	control, err := s.MonitorControl(ctx)
	if err != nil || control.Lease.Generation == "" {
		return items, err
	}
	query := `SELECT ` + monitorEventColumns + ` FROM GO_SYNC_MONITOR_EVENTS`
	args := []any{}
	if pending {
		query += ` WHERE GENERATION=? AND STATE IN ('observed','reserving','reserved') ORDER BY FIRST_SEEN,EVENT_KEY LIMIT 2049`
		args = append(args, generation)
	} else {
		query += ` ORDER BY CASE WHEN STATE IN ('needs_review','reserving','reserved') THEN 0 ELSE 1 END,UPDATED DESC,EVENT_KEY LIMIT 100`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanMonitorEvent(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, e)
		if len(items) > 2048 {
			return nil, ErrLimit
		}
	}
	return items, rows.Err()
}
func (s *Store) ReserveMonitorEvent(ctx context.Context, e MonitorEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyMonitorControl(ctx, tx, e.Generation); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_MONITOR_EVENTS SET STATE='reserving',UPDATED=? WHERE EVENT_KEY=? AND STATE='observed'`, time.Now().UTC().Format(time.RFC3339Nano), e.Key)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) SetMonitorEvent(ctx context.Context, key, state string, jobs []string, reason string) error {
	switch state {
	case "reserved", "completed", "needs_review", "filtered", "dismissed":
	default:
		return ErrState
	}
	if jobs == nil {
		jobs = []string{}
	}
	raw, err := json.Marshal(jobs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE GO_SYNC_MONITOR_EVENTS SET STATE=?,JOBS=?,REASON=?,UPDATED=? WHERE EVENT_KEY=?`, state, string(raw), reason, time.Now().UTC().Format(time.RFC3339Nano), key)
	return err
}
func (s *Store) MonitorEventJobs(ctx context.Context, key string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_ORGANIZATION_JOBS WHERE json_extract(DEFINITION,'$.sync.monitorEvent')=? ORDER BY CREATED,ID`, key)
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
	jobs := []Job{}
	for _, id := range ids {
		job, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}
func (s *Store) MonitorScopeBlocked(ctx context.Context, e MonitorEvent) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SYNC_MONITOR_EVENTS WHERE CONFIG_ID=? AND SCOPE=? AND EVENT_KEY!=? AND STATE IN ('reserving','reserved','needs_review'))`, e.Snapshot.ConfigID, e.Snapshot.Scope, e.Key).Scan(&exists)
	return exists, err
}
func (s *Store) GetMonitorEvent(ctx context.Context, key string) (MonitorEvent, error) {
	return scanMonitorEvent(s.db.QueryRowContext(ctx, `SELECT `+monitorEventColumns+` FROM GO_SYNC_MONITOR_EVENTS WHERE EVENT_KEY=?`, key))
}
