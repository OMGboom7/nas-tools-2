package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

const SyncTimerPlugin = "SyncTimer"
const SyncTimerConfig = "plugin." + SyncTimerPlugin

type MonitorTimer struct {
	Cron           string    `json:"cron"`
	Timezone       string    `json:"timezone"`
	SettingsDigest string    `json:"settingsDigest"`
	FirstDue       time.Time `json:"-"`
}

type SyncTimerSettings struct {
	Raw       string
	Installed bool
}

type SyncTimerRun struct {
	ID         string   `json:"id"`
	Slot       string   `json:"slot"`
	Started    string   `json:"started"`
	State      string   `json:"state"`
	Candidates int      `json:"candidates"`
	Keys       []string `json:"-"`
}

type SyncTimerClock struct {
	Next   string `json:"next"`
	Active string `json:"active"`
}

func syncTimerSetting(ctx context.Context, r unknownReader, key string) (string, error) {
	var value string
	err := r.QueryRowContext(ctx, `SELECT COALESCE(VALUE,'') FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return value, err
}

func readSyncTimerSettings(ctx context.Context, r unknownReader) (SyncTimerSettings, []string, error) {
	var out SyncTimerSettings
	raw, err := syncTimerSetting(ctx, r, "UserInstalledPlugins")
	if err != nil {
		return out, nil, err
	}
	list := []string{}
	if raw != "" && json.Unmarshal([]byte(raw), &list) != nil {
		return out, nil, ErrState
	}
	out.Installed = slices.Contains(list, SyncTimerPlugin)
	out.Raw, err = syncTimerSetting(ctx, r, SyncTimerConfig)
	return out, list, err
}

func (s *Store) SyncTimerSettings(ctx context.Context) (SyncTimerSettings, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SyncTimerSettings{}, err
	}
	defer tx.Rollback()
	settings, _, err := readSyncTimerSettings(ctx, tx)
	return settings, err
}

func putSyncTimerSetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	res, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, value, key)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig',?,?)`, key, value); err != nil {
			return err
		}
	}
	actual, err := syncTimerSetting(ctx, tx, key)
	if err != nil || actual != value {
		return ErrState
	}
	return nil
}

func (s *Store) ChangeSyncTimer(ctx context.Context, before, next SyncTimerSettings, revoke bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, installed, err := readSyncTimerSettings(ctx, tx)
	if err != nil || current != before {
		return ErrState
	}
	if before == next && !revoke {
		return nil
	}
	control, err := readMonitorControl(ctx, tx)
	if err != nil {
		return err
	}
	if control.Lease.Timer != nil {
		if err := pauseMonitorControl(ctx, tx, "SyncTimer 配置已变化，需重新授权"); err != nil {
			return err
		}
	}
	if before.Installed != next.Installed {
		installed = slices.DeleteFunc(installed, func(id string) bool { return id == SyncTimerPlugin })
		if next.Installed {
			installed = append(installed, SyncTimerPlugin)
		}
		data, err := json.Marshal(installed)
		if err != nil {
			return err
		}
		if err := putSyncTimerSetting(ctx, tx, "UserInstalledPlugins", string(data)); err != nil {
			return err
		}
	}
	if before.Raw != next.Raw {
		if err := putSyncTimerSetting(ctx, tx, SyncTimerConfig, next.Raw); err != nil {
			return err
		}
	}
	actual, _, err := readSyncTimerSettings(ctx, tx)
	if err != nil || actual != next {
		return ErrState
	}
	if control.Lease.Timer != nil {
		actual, err := readMonitorControl(ctx, tx)
		if err != nil || actual.Enabled() {
			return ErrState
		}
	}
	return tx.Commit()
}

func verifySyncTimer(ctx context.Context, r unknownReader, timer *MonitorTimer) error {
	if timer == nil {
		return nil
	}
	settings, _, err := readSyncTimerSettings(ctx, r)
	if err != nil {
		return err
	}
	if !settings.Installed || Digest(settings.Raw) != timer.SettingsDigest {
		return ErrState
	}
	return nil
}

func timerTables(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SYNC_TIMER_CLOCK(GENERATION TEXT PRIMARY KEY,NEXT_DUE TEXT NOT NULL,ACTIVE TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS GO_SYNC_TIMER_RUNS(ID TEXT PRIMARY KEY,GENERATION TEXT NOT NULL,SLOT TEXT NOT NULL,STARTED TEXT NOT NULL,STATE TEXT NOT NULL,CANDIDATES INTEGER NOT NULL,KEYS TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS GO_SYNC_TIMER_RUNS_GENERATION ON GO_SYNC_TIMER_RUNS(GENERATION,STARTED DESC);`)
	return err
}

func initSyncTimer(ctx context.Context, tx *sql.Tx, lease MonitorLease) error {
	if lease.Timer == nil {
		return nil
	}
	if !lease.IncludeExisting || lease.Timer.FirstDue.IsZero() {
		return ErrState
	}
	if err := verifySyncTimer(ctx, tx, lease.Timer); err != nil {
		return err
	}
	if err := timerTables(ctx, tx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO GO_SYNC_TIMER_CLOCK VALUES(?,?,'')`, lease.Generation, lease.Timer.FirstDue.UTC().Format(time.RFC3339Nano))
	return err
}

func readSyncTimerClock(ctx context.Context, r unknownReader, generation string) (SyncTimerClock, error) {
	var clock SyncTimerClock
	err := r.QueryRowContext(ctx, `SELECT NEXT_DUE,ACTIVE FROM GO_SYNC_TIMER_CLOCK WHERE GENERATION=?`, generation).Scan(&clock.Next, &clock.Active)
	return clock, err
}

func (s *Store) SyncTimerClock(ctx context.Context, generation string) (SyncTimerClock, error) {
	return readSyncTimerClock(ctx, s.db, generation)
}

func readTimerRun(r interface{ Scan(...any) error }) (SyncTimerRun, error) {
	var run SyncTimerRun
	var keys string
	if err := r.Scan(&run.ID, &run.Slot, &run.Started, &run.State, &run.Candidates, &keys); err != nil {
		return run, err
	}
	if run.Candidates < 0 || run.Candidates > 2048 || len(keys) > 150<<10 || json.Unmarshal([]byte(keys), &run.Keys) != nil ||
		run.State == "observing" && len(run.Keys) != run.Candidates || run.State != "observing" && len(run.Keys) != 0 {
		return SyncTimerRun{}, ErrState
	}
	return run, nil
}

const timerRunSelect = `SELECT ID,SLOT,STARTED,STATE,CANDIDATES,KEYS FROM GO_SYNC_TIMER_RUNS`

func (s *Store) SyncTimerRun(ctx context.Context, generation, id string) (SyncTimerRun, error) {
	return readTimerRun(s.db.QueryRowContext(ctx, timerRunSelect+` WHERE GENERATION=? AND ID=?`, generation, id))
}

func (s *Store) SyncTimerRuns(ctx context.Context, generation string) ([]SyncTimerRun, error) {
	runs := []SyncTimerRun{}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SYNC_TIMER_RUNS')`).Scan(&exists); err != nil || !exists {
		return runs, err
	}
	rows, err := s.db.QueryContext(ctx, timerRunSelect+` WHERE (?='' OR GENERATION=?) ORDER BY STARTED DESC,ID LIMIT 30`, generation, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		run, err := readTimerRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func pauseSyncTimer(ctx context.Context, tx syncWriter, lease MonitorLease) error {
	if lease.Timer == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_TIMER_RUNS SET STATE='needs_review',KEYS='[]'
	WHERE GENERATION=? AND STATE='observing'`, lease.Generation); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_TIMER_CLOCK SET ACTIVE='' WHERE GENERATION=?`, lease.Generation); err != nil {
		return err
	}
	return trimSyncTimerRuns(ctx, tx)
}

func trimSyncTimerRuns(ctx context.Context, tx syncWriter) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM GO_SYNC_TIMER_RUNS WHERE STATE<>'observing' AND ID NOT IN
		(SELECT ID FROM GO_SYNC_TIMER_RUNS ORDER BY STARTED DESC,ID LIMIT 30)`)
	return err
}

func (s *Store) ClaimSyncTimer(ctx context.Context, lease MonitorLease, slot string, next, now time.Time, snapshots []MonitorSnapshot) error {
	if lease.Timer == nil || len(snapshots) > 2048 || !next.After(now) {
		return ErrState
	}
	due, err := time.Parse(time.RFC3339Nano, slot)
	if err != nil || due.After(now) {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := verifyMonitorControl(ctx, tx, lease.Generation); err != nil {
		return err
	}
	clock, err := readSyncTimerClock(ctx, tx, lease.Generation)
	if err != nil || clock.Next != slot || clock.Active != "" {
		return ErrState
	}
	keys := []string{}
	for _, snapshot := range snapshots {
		keys = append(keys, MonitorEventKey(lease.Generation, snapshot))
	}
	data, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	id := Digest([]string{lease.Generation, slot})
	if err := observeSyncMonitor(ctx, tx, lease, snapshots, now, true); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_SYNC_TIMER_RUNS VALUES(?,?,?,?,'observing',?,?)`, id, lease.Generation, slot, now.UTC().Format(time.RFC3339Nano), len(keys), string(data)); err != nil {
		return err
	}
	nextString := next.UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_TIMER_CLOCK SET NEXT_DUE=?,ACTIVE=? WHERE GENERATION=?`, nextString, id, lease.Generation); err != nil {
		return err
	}
	actual, err := readSyncTimerClock(ctx, tx, lease.Generation)
	if err != nil || actual != (SyncTimerClock{Next: nextString, Active: id}) {
		return ErrState
	}
	stored, err := readTimerRun(tx.QueryRowContext(ctx, timerRunSelect+` WHERE GENERATION=? AND ID=?`, lease.Generation, id))
	if err != nil || stored.Slot != slot || stored.State != "observing" || !slices.Equal(stored.Keys, keys) {
		return ErrState
	}
	if err := verifyMonitorControl(ctx, tx, lease.Generation); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishSyncTimer(ctx context.Context, generation, id, state string) error {
	if state != "scanned" && state != "needs_review" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	clock, err := readSyncTimerClock(ctx, tx, generation)
	if err != nil || clock.Active != id {
		return ErrState
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_TIMER_RUNS SET STATE=?,KEYS='[]' WHERE GENERATION=? AND ID=? AND STATE='observing'`, state, generation, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrState
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_TIMER_CLOCK SET ACTIVE='' WHERE GENERATION=? AND ACTIVE=?`, generation, id); err != nil {
		return err
	}
	actual, err := readSyncTimerClock(ctx, tx, generation)
	if err != nil || actual.Active != "" || actual.Next != clock.Next {
		return ErrState
	}
	run, err := readTimerRun(tx.QueryRowContext(ctx, timerRunSelect+` WHERE GENERATION=? AND ID=?`, generation, id))
	if err != nil || run.State != state {
		return ErrState
	}
	if err := trimSyncTimerRuns(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
