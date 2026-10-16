package mediasync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

type Activity struct {
	ID                 string                   `json:"id"`
	Kind               string                   `json:"kind"`
	Server             string                   `json:"server"`
	ConfigDigest       string                   `json:"configDigest"`
	Actor              string                   `json:"actor"`
	Libraries          []string                 `json:"libraries"`
	State              string                   `json:"state"`
	Reason             string                   `json:"reason"`
	Started            time.Time                `json:"started"`
	Updated            time.Time                `json:"updated"`
	Finished           time.Time                `json:"finished"`
	SnapshotGeneration string                   `json:"snapshotGeneration"`
	Progress           mediaserver.SyncProgress `json:"progress"`
}

func insertActivity(ctx context.Context, tx *sql.Tx, a Activity) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIASYNC_ACTIVITY(ID,DOCUMENT) VALUES(?,?)`, a.ID, string(raw))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrSchedule
	}
	return nil
}
func readActivity(ctx context.Context, tx *sql.Tx, id string) (Activity, string, error) {
	var a Activity
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_MEDIASYNC_ACTIVITY WHERE ID=?`, id).Scan(&raw)
	if err != nil {
		return a, raw, err
	}
	err = json.Unmarshal([]byte(raw), &a)
	return a, raw, err
}
func writeActivity(ctx context.Context, tx *sql.Tx, a Activity, previous string) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_MEDIASYNC_ACTIVITY SET DOCUMENT=? WHERE ID=? AND DOCUMENT=?`, string(raw), a.ID, previous)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrSchedule
	}
	return nil
}
func newActivity(id, kind, server, digest, actor string, libraries []string, now time.Time) Activity {
	return Activity{ID: id, Kind: kind, Server: server, ConfigDigest: digest, Actor: actor, Libraries: append([]string{}, libraries...), State: "collecting", Started: now.UTC(), Updated: now.UTC(), Progress: mediaserver.SyncProgress{Stage: "libraries", LibrariesTotal: len(libraries)}}
}
func (s *Store) BeginManual(ctx context.Context, server, digest, actor string, libraries []string, now time.Time) (Activity, error) {
	if !validScheduleDefinition(ScheduleDefinition{Server: server, ConfigDigest: digest, AuthorityDigest: digest, Actor: actor, Libraries: libraries, IntervalSeconds: 60}) {
		return Activity{}, ErrSnapshot
	}
	id, err := randomID()
	if err != nil {
		return Activity{}, err
	}
	a := newActivity(id, "manual", server, digest, actor, libraries, now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	if err := insertActivity(ctx, tx, a); err != nil {
		return a, err
	}
	return a, tx.Commit()
}
func (s *Store) Activities(ctx context.Context) ([]Activity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DOCUMENT FROM GO_MEDIASYNC_ACTIVITY ORDER BY rowid DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Activity{}
	for rows.Next() {
		var raw string
		var a Activity
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, rows.Err()
}
func (s *Store) ReportProgress(ctx context.Context, id string, p mediaserver.SyncProgress, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, raw, err := readActivity(ctx, tx, id)
	if err != nil {
		return err
	}
	if a.State != "collecting" || p.LibrariesTotal != len(a.Libraries) || p.LibrariesDone < a.Progress.LibrariesDone || p.LibrariesDone > p.LibrariesTotal || p.ItemsDone < a.Progress.ItemsDone || p.ItemsDone > 100000 || p.EpisodesDone < a.Progress.EpisodesDone || p.EpisodesDone > 1000000 || p.PagesRead < a.Progress.PagesRead {
		return ErrSchedule
	}
	if p.Stage != "libraries" && p.Stage != "items" && p.Stage != "episodes" && p.Stage != "library-complete" && p.Stage != "publishing" {
		return ErrSchedule
	}
	if p.Library != "" {
		found := false
		for _, library := range a.Libraries {
			found = found || library == p.Library
		}
		if !found {
			return ErrSchedule
		}
	}
	a.Progress = p
	a.Updated = now.UTC()
	if err := writeActivity(ctx, tx, a, raw); err != nil {
		return err
	}
	return tx.Commit()
}
func finishActivity(ctx context.Context, tx *sql.Tx, id, state, reason, generation string, now time.Time, optional bool) error {
	a, raw, err := readActivity(ctx, tx, id)
	if errors.Is(err, sql.ErrNoRows) && optional {
		return nil
	}
	if err != nil {
		return err
	}
	if a.State != "collecting" {
		return ErrSchedule
	}
	if state == "completed" && generation == "" {
		return ErrSchedule
	}
	a.State = state
	a.Reason = reason
	a.SnapshotGeneration = generation
	a.Updated = now.UTC()
	a.Finished = now.UTC()
	if state == "completed" {
		a.Progress.Stage = "completed"
		a.Progress.LibrariesDone = a.Progress.LibrariesTotal
	}
	return writeActivity(ctx, tx, a, raw)
}
func (s *Store) FailManual(ctx context.Context, id, state, reason string, now time.Time) error {
	if state != "failed" && state != "interrupted" {
		return ErrSchedule
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, _, err := readActivity(ctx, tx, id)
	if err != nil {
		return err
	}
	if a.Kind != "manual" {
		return ErrSchedule
	}
	if err := finishActivity(ctx, tx, id, state, reason, "", now, false); err != nil {
		return err
	}
	return tx.Commit()
}
func recoverActivities(ctx context.Context, tx *sql.Tx, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT ID FROM GO_MEDIASYNC_ACTIVITY WHERE json_extract(DOCUMENT,'$.state')='collecting'`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := finishActivity(ctx, tx, id, "interrupted", "previous collector no longer holds execution guard; no success inferred", "", now, false); err != nil {
			return err
		}
	}
	return nil
}
func checkActivity(ctx context.Context, tx *sql.Tx, id, kind, server, digest, actor string, libraries []string, items []mediaserver.SyncItem) error {
	a, _, err := readActivity(ctx, tx, id)
	if err != nil {
		return err
	}
	left, _ := json.Marshal(a.Libraries)
	right, _ := json.Marshal(libraries)
	episodes := 0
	for _, item := range items {
		episodes += len(item.Episodes)
	}
	if a.State != "collecting" || a.Kind != kind || a.Server != server || a.ConfigDigest != digest || a.Actor != actor || string(left) != string(right) || a.Progress.Stage != "publishing" || a.Progress.LibrariesDone != len(libraries) || a.Progress.ItemsDone != len(items) || a.Progress.EpisodesDone != episodes {
		return ErrSchedule
	}
	return nil
}
func (s *Store) PublishManual(ctx context.Context, a Activity, items []mediaserver.SyncItem, now time.Time) (Snapshot, error) {
	if a.Kind != "manual" {
		return Snapshot{}, ErrSchedule
	}
	return s.publish(ctx, a.Server, a.ConfigDigest, a.Actor, a.Libraries, items, nil, a.ID, now)
}
