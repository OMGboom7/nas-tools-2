package organization

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"strconv"
	"time"
)

type DownloadMonitorLease struct {
	Remote          *RemoteAutomation `json:"remote,omitempty"`
	Generation      string            `json:"generation"`
	Actor           string            `json:"actor"`
	IDs             []string          `json:"ids"`
	Downloaders     map[string]string `json:"downloaders"`
	ConfigDigest    string            `json:"configDigest"`
	Roots           []string          `json:"roots"`
	IncludeExisting bool              `json:"includeExisting"`
	SourceRemoval   bool              `json:"sourceRemoval"`
	PauseTasks      bool              `json:"pauseTasks"`
	ResultWrites    bool              `json:"resultWrites"`
	TaskRemoval     bool              `json:"taskRemoval"`
	ExclusiveLabels bool              `json:"exclusiveLabels"`
	AllowOmitted    bool              `json:"allowOmitted"`
}
type DownloadMonitorControl struct {
	State  string               `json:"state"`
	Lease  DownloadMonitorLease `json:"lease"`
	Reason string               `json:"reason,omitempty"`
}
type DownloadObservation struct {
	Task     downloadcompletion.Task `json:"task"`
	Files    []File                  `json:"files"`
	Root     string                  `json:"root"`
	Identity string                  `json:"identity"`
	Config   string                  `json:"config"`
}
type DownloadMonitorEvent struct {
	Key          string `json:"key"`
	Generation   string `json:"generation"`
	DownloaderID string `json:"downloaderId"`
	TaskID       string `json:"taskId"`
	State        string `json:"state"`
	Snapshot     string `json:"snapshot,omitempty"`
	FirstSeen    string `json:"firstSeen,omitempty"`
	Updated      string `json:"updated"`
	BatchID      string `json:"batchId,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

func downloadMonitorTables(ctx context.Context, w syncWriter) error {
	for _, q := range []string{`CREATE TABLE IF NOT EXISTS GO_DOWNLOAD_MONITOR_CONTROL(ID INTEGER PRIMARY KEY,DOCUMENT TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '')`, `CREATE TABLE IF NOT EXISTS GO_DOWNLOAD_MONITOR_EVENTS(EVENT_KEY TEXT PRIMARY KEY,GENERATION TEXT NOT NULL,DOWNLOADER_ID TEXT NOT NULL,TASK_ID TEXT NOT NULL,STATE TEXT NOT NULL,SNAPSHOT TEXT NOT NULL DEFAULT '',FIRST_SEEN TEXT NOT NULL DEFAULT '',UPDATED TEXT NOT NULL,BATCH_ID TEXT NOT NULL DEFAULT '',REASON TEXT NOT NULL DEFAULT '')`} {
		if _, err := w.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func readDownloadMonitor(ctx context.Context, r unknownReader) (DownloadMonitorControl, error) {
	c := DownloadMonitorControl{State: "disabled"}
	var exists bool
	if err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_DOWNLOAD_MONITOR_CONTROL')`).Scan(&exists); err != nil || !exists {
		return c, err
	}
	var raw string
	err := r.QueryRowContext(ctx, `SELECT DOCUMENT,STATE,REASON FROM GO_DOWNLOAD_MONITOR_CONTROL WHERE ID=1`).Scan(&raw, &c.State, &c.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return DownloadMonitorControl{State: "disabled"}, nil
	}
	if err != nil {
		return c, err
	}
	if json.Unmarshal([]byte(raw), &c.Lease) != nil {
		return c, ErrState
	}
	return c, nil
}
func (s *Store) DownloadMonitorControl(ctx context.Context) (DownloadMonitorControl, error) {
	return readDownloadMonitor(ctx, s.db)
}
func readMonitoredDownloader(ctx context.Context, r unknownReader, id string) (downloaderconfig.Downloader, error) {
	var item downloaderconfig.Downloader
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
		return item, ErrState
	}
	err = r.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(ENABLED,0),COALESCE(TYPE,''),COALESCE(TRANSFER,0),COALESCE(ONLY_NASTOOL,0),COALESCE(MATCH_PATH,0),COALESCE(RMT_MODE,''),COALESCE(CONFIG,'{}'),COALESCE(DOWNLOAD_DIR,'[]') FROM DOWNLOADER WHERE ID=?`, n).Scan(&item.ID, &item.Name, &item.Enabled, &item.Type, &item.Transfer, &item.OnlyNastool, &item.MatchPath, &item.RmtMode, &item.Config, &item.DownloadDir)
	return item, err
}
func verifyDownloadMonitor(ctx context.Context, r unknownReader, generation string) error {
	c, err := readDownloadMonitor(ctx, r)
	if err != nil {
		return err
	}
	if generation == "" || c.State != "enabled" || c.Lease.Generation != generation || len(c.Lease.IDs) == 0 || len(c.Lease.IDs) > 16 {
		return ErrState
	}
	for _, id := range c.Lease.IDs {
		row, err := readMonitoredDownloader(ctx, r, id)
		if err != nil {
			return err
		}
		if row.Enabled != 1 || row.Transfer != 1 || Digest(row) != c.Lease.Downloaders[id] {
			return ErrState
		}
	}
	return nil
}
func (s *Store) VerifyDownloadMonitor(ctx context.Context, generation string) error {
	return verifyDownloadMonitor(ctx, s.db, generation)
}
func (s *Store) PauseDownloadMonitor(ctx context.Context, reason string) error {
	c, err := s.DownloadMonitorControl(ctx)
	if err != nil || c.State != "enabled" {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE GO_DOWNLOAD_MONITOR_CONTROL SET STATE='paused',REASON=? WHERE ID=1 AND STATE='enabled'`, reason)
	return err
}
func (s *Store) PauseDownloadMonitorGeneration(ctx context.Context, generation, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE GO_DOWNLOAD_MONITOR_CONTROL SET STATE='paused',REASON=? WHERE ID=1 AND STATE='enabled' AND json_extract(DOCUMENT,'$.generation')=?`, reason, generation)
	return err
}
func DownloadMonitorEventKey(generation, downloaderID, taskID string) string {
	return Digest([]string{generation, downloaderID, taskID})
}
func (s *Store) EnableDownloadMonitor(ctx context.Context, lease DownloadMonitorLease, candidates map[string][]string, now time.Time) (DownloadMonitorControl, error) {
	if lease.Remote != nil && !lease.Remote.Valid() {
		return DownloadMonitorControl{}, ErrState
	}
	if lease.Actor == "" || len(lease.IDs) == 0 || len(lease.IDs) > 16 || !lease.ResultWrites {
		return DownloadMonitorControl{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DownloadMonitorControl{}, err
	}
	defer tx.Rollback()
	for _, id := range lease.IDs {
		row, err := readMonitoredDownloader(ctx, tx, id)
		if err != nil {
			return DownloadMonitorControl{}, err
		}
		if row.Enabled != 1 || row.Transfer != 1 || Digest(row) != lease.Downloaders[id] {
			return DownloadMonitorControl{}, ErrState
		}
	}
	if err := downloadMonitorTables(ctx, tx); err != nil {
		return DownloadMonitorControl{}, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return DownloadMonitorControl{}, err
	}
	lease.Generation = hex.EncodeToString(random[:])
	raw, err := json.Marshal(lease)
	if err != nil {
		return DownloadMonitorControl{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_DOWNLOAD_MONITOR_CONTROL VALUES(1,?,'enabled','') ON CONFLICT(ID) DO UPDATE SET DOCUMENT=excluded.DOCUMENT,STATE='enabled',REASON=''`, string(raw)); err != nil {
		return DownloadMonitorControl{}, err
	}
	state := "baseline"
	if lease.IncludeExisting {
		state = "observed"
	}
	if err := insertDownloadCandidates(ctx, tx, lease, candidates, state, now); err != nil {
		return DownloadMonitorControl{}, err
	}
	if err := tx.Commit(); err != nil {
		return DownloadMonitorControl{}, err
	}
	return DownloadMonitorControl{State: "enabled", Lease: lease}, nil
}
func insertDownloadCandidates(ctx context.Context, w syncWriter, lease DownloadMonitorLease, candidates map[string][]string, state string, now time.Time) error {
	total := 0
	for _, id := range lease.IDs {
		for _, task := range candidates[id] {
			total++
			if total > 2048 {
				return ErrLimit
			}
			if _, err := w.ExecContext(ctx, `INSERT INTO GO_DOWNLOAD_MONITOR_EVENTS(EVENT_KEY,GENERATION,DOWNLOADER_ID,TASK_ID,STATE,UPDATED) VALUES(?,?,?,?,?,?) ON CONFLICT(EVENT_KEY) DO NOTHING`, DownloadMonitorEventKey(lease.Generation, id, task), lease.Generation, id, task, state, now.UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Store) ObserveDownloadCandidates(ctx context.Context, lease DownloadMonitorLease, candidates map[string][]string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyDownloadMonitor(ctx, tx, lease.Generation); err != nil {
		return err
	}
	if err := insertDownloadCandidates(ctx, tx, lease, candidates, "observed", now); err != nil {
		return err
	}
	return tx.Commit()
}

const downloadEventColumns = `EVENT_KEY,GENERATION,DOWNLOADER_ID,TASK_ID,STATE,SNAPSHOT,FIRST_SEEN,UPDATED,BATCH_ID,REASON`

func scanDownloadEvent(row interface{ Scan(...any) error }) (DownloadMonitorEvent, error) {
	var e DownloadMonitorEvent
	err := row.Scan(&e.Key, &e.Generation, &e.DownloaderID, &e.TaskID, &e.State, &e.Snapshot, &e.FirstSeen, &e.Updated, &e.BatchID, &e.Reason)
	if err != nil {
		return e, err
	}
	if DownloadMonitorEventKey(e.Generation, e.DownloaderID, e.TaskID) != e.Key {
		return e, ErrState
	}
	return e, nil
}
func (s *Store) DownloadMonitorEvents(ctx context.Context, generation string, pending bool) ([]DownloadMonitorEvent, error) {
	items := []DownloadMonitorEvent{}
	c, err := s.DownloadMonitorControl(ctx)
	if err != nil || c.Lease.Generation == "" {
		return items, err
	}
	q := `SELECT ` + downloadEventColumns + ` FROM GO_DOWNLOAD_MONITOR_EVENTS`
	args := []any{}
	if pending {
		q += ` WHERE GENERATION=? AND STATE IN ('observed','filtered','pausing','reserving','running') ORDER BY CASE WHEN STATE IN ('pausing','reserving','running') THEN 0 ELSE 1 END,UPDATED,EVENT_KEY LIMIT 2049`
		args = append(args, generation)
	} else {
		q += ` ORDER BY CASE WHEN STATE IN ('needs_review','pausing','reserving','running') THEN 0 ELSE 1 END,UPDATED DESC,EVENT_KEY LIMIT 100`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanDownloadEvent(rows)
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
func (s *Store) GetDownloadMonitorEvent(ctx context.Context, key string) (DownloadMonitorEvent, error) {
	return scanDownloadEvent(s.db.QueryRowContext(ctx, `SELECT `+downloadEventColumns+` FROM GO_DOWNLOAD_MONITOR_EVENTS WHERE EVENT_KEY=?`, key))
}
func (s *Store) SetDownloadMonitorEvent(ctx context.Context, e DownloadMonitorEvent, state, snapshot, firstSeen, batchID, reason string, now time.Time) error {
	switch state {
	case "observed", "filtered", "pausing", "reserving", "running", "needs_review", "completed", "linked", "dismissed":
	default:
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if state == "pausing" || state == "reserving" || state == "running" {
		if err := verifyDownloadMonitor(ctx, tx, e.Generation); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_DOWNLOAD_MONITOR_EVENTS SET STATE=?,SNAPSHOT=?,FIRST_SEEN=?,UPDATED=?,BATCH_ID=?,REASON=? WHERE EVENT_KEY=? AND STATE=?`, state, snapshot, firstSeen, now.UTC().Format(time.RFC3339Nano), batchID, reason, e.Key, e.State)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) DownloadMonitorScopeBlocked(ctx context.Context, e DownloadMonitorEvent) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_DOWNLOAD_MONITOR_EVENTS WHERE DOWNLOADER_ID=? AND TASK_ID=? AND EVENT_KEY!=? AND STATE IN ('pausing','reserving','running','needs_review'))`, e.DownloaderID, e.TaskID, e.Key).Scan(&exists)
	return exists, err
}
