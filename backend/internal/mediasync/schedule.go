package mediasync

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

var ErrSchedule = errors.New("media sync schedule changed or invalid")

type ScheduleDefinition struct {
	Server          string   `json:"server"`
	ConfigDigest    string   `json:"configDigest"`
	AuthorityDigest string   `json:"authorityDigest"`
	Actor           string   `json:"actor"`
	Libraries       []string `json:"libraries"`
	IntervalSeconds int64    `json:"intervalSeconds"`
}
type Schedule struct {
	Revision   string             `json:"revision"`
	Enabled    bool               `json:"enabled"`
	Definition ScheduleDefinition `json:"definition"`
	NextRun    time.Time          `json:"nextRun"`
	RunID      string             `json:"runId"`
	LastState  string             `json:"lastState"`
	Reason     string             `json:"reason"`
	ChangedBy  string             `json:"changedBy"`
	Updated    time.Time          `json:"updated"`
}
type ScheduledRun struct {
	ID                 string             `json:"id"`
	Revision           string             `json:"revision"`
	Definition         ScheduleDefinition `json:"definition"`
	State              string             `json:"state"`
	Reason             string             `json:"reason"`
	Started            time.Time          `json:"started"`
	Finished           time.Time          `json:"finished"`
	SnapshotGeneration string             `json:"snapshotGeneration"`
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
func validScheduleDefinition(d ScheduleDefinition) bool {
	if d.Server != "emby" && d.Server != "jellyfin" && d.Server != "plex" || d.Actor == "" || len(d.Libraries) == 0 || len(d.Libraries) > 100 || d.IntervalSeconds < 60 || d.IntervalSeconds > 31536000 {
		return false
	}
	for _, digest := range []string{d.ConfigDigest, d.AuthorityDigest} {
		if len(digest) != 64 {
			return false
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return false
		}
	}
	seen := map[string]bool{}
	for _, id := range d.Libraries {
		if id == "" || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func readSchedule(ctx context.Context, tx *sql.Tx) (Schedule, string, error) {
	s := Schedule{LastState: "disabled", Definition: ScheduleDefinition{Libraries: []string{}}}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_MEDIASYNC_SCHEDULE WHERE ID=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, "", nil
	}
	if err != nil {
		return s, "", err
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, "", err
	}
	return s, raw, nil
}
func writeSchedule(ctx context.Context, tx *sql.Tx, previous string, s Schedule) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	var result sql.Result
	if previous == "" {
		result, err = tx.ExecContext(ctx, `INSERT INTO GO_MEDIASYNC_SCHEDULE(ID,DOCUMENT) VALUES(1,?)`, string(raw))
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE GO_MEDIASYNC_SCHEDULE SET DOCUMENT=? WHERE ID=1 AND DOCUMENT=?`, string(raw), previous)
	}
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrSchedule
	}
	return nil
}
func writeScheduledRun(ctx context.Context, tx *sql.Tx, run ScheduledRun, insert bool) error {
	raw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	var result sql.Result
	if insert {
		result, err = tx.ExecContext(ctx, `INSERT INTO GO_MEDIASYNC_RUNS(ID,DOCUMENT) VALUES(?,?)`, run.ID, string(raw))
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE GO_MEDIASYNC_RUNS SET DOCUMENT=? WHERE ID=?`, string(raw), run.ID)
	}
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrSchedule
	}
	return nil
}
func readScheduledRun(ctx context.Context, tx *sql.Tx, id string) (ScheduledRun, error) {
	var run ScheduledRun
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_MEDIASYNC_RUNS WHERE ID=?`, id).Scan(&raw)
	if err != nil {
		return run, err
	}
	err = json.Unmarshal([]byte(raw), &run)
	return run, err
}
func (s *Store) Schedule(ctx context.Context) (Schedule, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Schedule{}, err
	}
	defer tx.Rollback()
	schedule, _, err := readSchedule(ctx, tx)
	return schedule, err
}
func (s *Store) Runs(ctx context.Context) ([]ScheduledRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DOCUMENT FROM GO_MEDIASYNC_RUNS ORDER BY rowid DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []ScheduledRun{}
	for rows.Next() {
		var raw string
		var run ScheduledRun
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &run); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
func (s *Store) ConfigureSchedule(ctx context.Context, expected string, d ScheduleDefinition, now time.Time) (Schedule, error) {
	if !validScheduleDefinition(d) {
		return Schedule{}, ErrSchedule
	}
	d.Libraries = append([]string{}, d.Libraries...)
	sort.Strings(d.Libraries)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, err
	}
	defer tx.Rollback()
	old, raw, err := readSchedule(ctx, tx)
	if err != nil {
		return old, err
	}
	if old.Revision != expected {
		return old, ErrSchedule
	}
	if old.RunID != "" {
		run, err := readScheduledRun(ctx, tx, old.RunID)
		if err != nil {
			return old, err
		}
		run.State = "superseded"
		run.Reason = "schedule replaced during collection; no snapshot authorized"
		run.Finished = now.UTC()
		if err := writeScheduledRun(ctx, tx, run, false); err != nil {
			return old, err
		}
		if err := finishActivity(ctx, tx, run.ID, run.State, run.Reason, "", now, true); err != nil {
			return old, err
		}
	}
	revision, err := randomID()
	if err != nil {
		return old, err
	}
	next := Schedule{Revision: revision, Enabled: true, Definition: d, NextRun: now.UTC().Add(time.Duration(d.IntervalSeconds) * time.Second), LastState: "scheduled", ChangedBy: d.Actor, Updated: now.UTC()}
	if err := writeSchedule(ctx, tx, raw, next); err != nil {
		return old, err
	}
	return next, tx.Commit()
}

// Disable never waits for a remote read. Revocation and in-flight disposition
// commit together; PublishScheduled verifies this same row inside its write tx.
func (s *Store) DisableSchedule(ctx context.Context, expected, actor, state, reason string, now time.Time) (Schedule, error) {
	if actor == "" || (state != "stopped" && state != "paused") {
		return Schedule{}, ErrSchedule
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Schedule{}, err
	}
	defer tx.Rollback()
	current, raw, err := readSchedule(ctx, tx)
	if err != nil {
		return current, err
	}
	if current.Revision != expected {
		return current, ErrSchedule
	}
	if current.RunID != "" {
		run, err := readScheduledRun(ctx, tx, current.RunID)
		if err != nil {
			return current, err
		}
		run.State = state
		run.Reason = reason
		run.Finished = now.UTC()
		if err := writeScheduledRun(ctx, tx, run, false); err != nil {
			return current, err
		}
		if err := finishActivity(ctx, tx, run.ID, run.State, run.Reason, "", now, true); err != nil {
			return current, err
		}
	}
	current.Revision, err = randomID()
	if err != nil {
		return current, err
	}
	current.Enabled = false
	current.NextRun = time.Time{}
	current.RunID = ""
	current.LastState = state
	current.Reason = reason
	current.ChangedBy = actor
	current.Updated = now.UTC()
	if err := writeSchedule(ctx, tx, raw, current); err != nil {
		return current, err
	}
	return current, tx.Commit()
}

// RecoverInterrupted requires the global execution guard, not a TTL. A prior
// claimant can no longer publish while that guard is held by this caller.
func (s *Store) RecoverInterrupted(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, raw, err := readSchedule(ctx, tx)
	if err != nil {
		return err
	}
	if current.RunID == "" {
		if err := recoverActivities(ctx, tx, now); err != nil {
			return err
		}
		return tx.Commit()
	}
	run, err := readScheduledRun(ctx, tx, current.RunID)
	if err != nil {
		return err
	}
	run.State = "interrupted"
	run.Reason = "previous collector no longer holds execution guard; prior snapshot retained"
	run.Finished = now.UTC()
	if err := writeScheduledRun(ctx, tx, run, false); err != nil {
		return err
	}
	if err := finishActivity(ctx, tx, run.ID, run.State, run.Reason, "", now, true); err != nil {
		return err
	}
	current.RunID = ""
	current.LastState = run.State
	current.Reason = run.Reason
	current.Updated = now.UTC()
	if err := writeSchedule(ctx, tx, raw, current); err != nil {
		return err
	}
	if err := recoverActivities(ctx, tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Claim advances the durable cadence before any remote read. Overdue periods
// collapse into one complete scan, never a replay of every missed interval.
func (s *Store) ClaimSchedule(ctx context.Context, expected string, now time.Time) (ScheduledRun, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScheduledRun{}, false, err
	}
	defer tx.Rollback()
	current, raw, err := readSchedule(ctx, tx)
	if err != nil {
		return ScheduledRun{}, false, err
	}
	if current.Revision != expected {
		return ScheduledRun{}, false, ErrSchedule
	}
	if !current.Enabled || current.RunID != "" || current.NextRun.After(now) {
		return ScheduledRun{}, false, nil
	}
	if !validScheduleDefinition(current.Definition) || current.NextRun.IsZero() {
		return ScheduledRun{}, false, ErrSchedule
	}
	id, err := randomID()
	if err != nil {
		return ScheduledRun{}, false, err
	}
	run := ScheduledRun{ID: id, Revision: current.Revision, Definition: current.Definition, State: "collecting", Started: now.UTC()}
	interval := time.Duration(current.Definition.IntervalSeconds) * time.Second
	current.NextRun = now.UTC().Add(interval - now.Sub(current.NextRun)%interval)
	current.RunID = id
	current.LastState = "collecting"
	current.Reason = ""
	current.Updated = now.UTC()
	if err := writeScheduledRun(ctx, tx, run, true); err != nil {
		return run, false, err
	}
	if err := insertActivity(ctx, tx, newActivity(run.ID, "scheduled", run.Definition.Server, run.Definition.ConfigDigest, run.Definition.Actor, run.Definition.Libraries, now)); err != nil {
		return run, false, err
	}
	if err := writeSchedule(ctx, tx, raw, current); err != nil {
		return run, false, err
	}
	if err := tx.Commit(); err != nil {
		return run, false, err
	}
	return run, true, nil
}
func checkScheduledRun(ctx context.Context, tx *sql.Tx, run ScheduledRun) (Schedule, string, error) {
	current, raw, err := readSchedule(ctx, tx)
	if err != nil {
		return current, raw, err
	}
	if !current.Enabled || current.Revision != run.Revision || current.RunID != run.ID {
		return current, raw, ErrSchedule
	}
	persisted, err := readScheduledRun(ctx, tx, run.ID)
	if err != nil {
		return current, raw, err
	}
	a, _ := json.Marshal(current.Definition)
	b, _ := json.Marshal(run.Definition)
	c, _ := json.Marshal(persisted.Definition)
	if persisted.State != "collecting" || persisted.Revision != run.Revision || string(a) != string(b) || string(b) != string(c) {
		return current, raw, ErrSchedule
	}
	return current, raw, nil
}
func finishScheduledRun(ctx context.Context, tx *sql.Tx, run ScheduledRun, state, reason, generation string, now time.Time) error {
	current, raw, err := checkScheduledRun(ctx, tx, run)
	if err != nil {
		return err
	}
	run.State = state
	run.Reason = reason
	run.SnapshotGeneration = generation
	run.Finished = now.UTC()
	if err := writeScheduledRun(ctx, tx, run, false); err != nil {
		return err
	}
	if err := finishActivity(ctx, tx, run.ID, state, reason, generation, now, state != "completed"); err != nil {
		return err
	}
	current.RunID = ""
	current.LastState = state
	current.Reason = reason
	current.Updated = now.UTC()
	if err := writeSchedule(ctx, tx, raw, current); err != nil {
		return err
	}
	return nil
}
func (s *Store) FailScheduledRun(ctx context.Context, run ScheduledRun, state, reason string, now time.Time) error {
	if state != "failed" && state != "interrupted" {
		return ErrSchedule
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := finishScheduledRun(ctx, tx, run, state, reason, "", now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) PublishScheduled(ctx context.Context, run ScheduledRun, items []mediaserver.SyncItem, now time.Time) (Snapshot, error) {
	return s.publish(ctx, run.Definition.Server, run.Definition.ConfigDigest, run.Definition.Actor, run.Definition.Libraries, items, &run, run.ID, now)
}
