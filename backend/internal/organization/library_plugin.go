package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type LibraryPrepared struct {
	Fingerprint string
	Definition  LibraryDefinition
	Files       []LibraryMedia
}
type LibraryPluginRun struct {
	ID      string   `json:"id"`
	Epoch   string   `json:"-"`
	State   string   `json:"state"`
	Reason  string   `json:"reason"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
	Jobs    []string `json:"jobs"`
	Empty   int      `json:"empty"`
	Source  string   `json:"source,omitempty"`
}

// LibraryPluginMutation is the single user.db transaction boundary for plugin
// revocation, consumed-once scheduling and ready jobs. No filesystem scan or
// remote call is performed while this transaction holds a database lock.
func (s *Store) LibraryPluginMutation(ctx context.Context, expected, changes map[string]string, revokeEpoch string, run *LibraryPluginRun, plans []LibraryPrepared) error {
	allowed := map[string]bool{"UserInstalledPlugins": true, "plugin.LibraryScraper": true, "GoLibraryScraper": true, "UserScraperConf": true}
	if len(expected) < 3 || len(expected) > 4 || len(plans) > 16 {
		return ErrState
	}
	for _, k := range []string{"UserInstalledPlugins", "plugin.LibraryScraper", "GoLibraryScraper"} {
		if _, ok := expected[k]; !ok {
			return ErrState
		}
	}
	for k := range expected {
		if !allowed[k] {
			return ErrState
		}
	}
	for k, v := range changes {
		if _, ok := expected[k]; !ok || k == "UserScraperConf" || v == "" {
			return ErrState
		}
	}
	count, snapshotBytes := 0, 0
	for _, p := range plans {
		n, err := LibraryInventoryBytes(p.Files)
		if err != nil {
			return err
		}
		count += len(p.Files)
		snapshotBytes += n
		if count > 50_000 || snapshotBytes > LibraryWorklistBytes {
			return ErrLimit
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, want := range expected {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, k).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if current != want {
			return ErrState
		}
	}
	if err := librarySchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_LIBRARY_SCRAPE_RUNS(ID TEXT PRIMARY KEY,EPOCH TEXT NOT NULL,DOCUMENT TEXT NOT NULL)`); err != nil {
		return err
	}
	if revokeEpoch != "" {
		if len(revokeEpoch) != 32 && revokeEpoch != "*" {
			return ErrState
		}
		if _, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_JOBS SET STATE='cancelled',REASON='plugin authorization revoked; metadata, backups and uncertain claims retained',UPDATED=? WHERE STATE IN('ready','running','needs_review') AND CASE WHEN json_valid(DEFINITION) THEN COALESCE(json_extract(DEFINITION,'$.Schedule.Epoch'),'') ELSE '' END<>'' AND (?='*' OR json_extract(DEFINITION,'$.Schedule.Epoch')=?)`, time.Now().UTC().Format(time.RFC3339Nano), revokeEpoch, revokeEpoch); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_RUNS SET DOCUMENT=json_set(DOCUMENT,'$.state','revoked','$.reason','plugin authorization revoked; effects and uncertain claims retained','$.updated',?) WHERE (?='*' OR EPOCH=?) AND json_extract(DOCUMENT,'$.state') IN('scanning','queued')`, time.Now().UTC().Format(time.RFC3339Nano), revokeEpoch, revokeEpoch); err != nil {
			return err
		}
	}
	ids := []string{}
	for _, p := range plans {
		if run == nil || p.Definition.Schedule == nil || p.Definition.Schedule.Epoch != run.Epoch || p.Definition.Schedule.Batch != run.ID {
			return ErrState
		}
		id, err := createLibraryJob(ctx, tx, p.Fingerprint, p.Definition, p.Files)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if run != nil {
		if len(run.ID) != 32 || len(run.Epoch) != 32 || len(run.Reason) > 2000 || len(run.Jobs) > 16 {
			return ErrState
		}
		if len(plans) > 0 && Digest(ids) != Digest(run.Jobs) {
			return ErrState
		}
		data, err := json.Marshal(run)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_LIBRARY_SCRAPE_RUNS(ID,EPOCH,DOCUMENT) VALUES(?,?,?) ON CONFLICT(ID) DO UPDATE SET DOCUMENT=excluded.DOCUMENT WHERE EPOCH=excluded.EPOCH`, run.ID, run.Epoch, string(data)); err != nil {
			return err
		}
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_LIBRARY_SCRAPE_RUNS WHERE ID=? AND EPOCH=?`, run.ID, run.Epoch).Scan(&actual); err != nil || actual != string(data) {
			return ErrState
		}
	}
	for k, v := range changes {
		res, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, v, k)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE) VALUES('SystemConfig',?,?,'')`, k, v); err != nil {
				return err
			}
		}
		var actual string
		if err := tx.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, k).Scan(&actual); err != nil || actual != v {
			return ErrState
		}
	}
	return tx.Commit()
}

func finishLibraryEvent(ctx context.Context, tx *sql.Tx, id, state, reason string) error {
	if state != "completed" && state != "partial" && state != "needs_review" && state != "cancelled" {
		return nil
	}
	var raw string
	var d LibraryDefinition
	if err := tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&raw); err != nil || json.Unmarshal([]byte(raw), &d) != nil {
		return ErrState
	}
	if d.Schedule == nil || !d.Schedule.Event {
		return nil
	}
	var epoch, data string
	var run LibraryPluginRun
	if err := tx.QueryRowContext(ctx, `SELECT EPOCH,DOCUMENT FROM GO_LIBRARY_SCRAPE_RUNS WHERE ID=?`, d.Schedule.Batch).Scan(&epoch, &data); err != nil {
		return err
	}
	if epoch != d.Schedule.Epoch || json.Unmarshal([]byte(data), &run) != nil || Digest(run.Jobs) != Digest([]string{id}) {
		return ErrState
	}
	run.State, run.Reason, run.Updated = state, reason, time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(run)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_RUNS SET DOCUMENT=? WHERE ID=? AND EPOCH=?`, string(b), d.Schedule.Batch, epoch); err != nil {
		return err
	}
	var actual string
	if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_LIBRARY_SCRAPE_RUNS WHERE ID=?`, d.Schedule.Batch).Scan(&actual); err != nil || actual != string(b) {
		return ErrState
	}
	return nil
}
func (s *Store) LibraryPluginRuns(ctx context.Context) ([]LibraryPluginRun, error) {
	result := []LibraryPluginRun{}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_LIBRARY_SCRAPE_RUNS')`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT EPOCH,DOCUMENT FROM GO_LIBRARY_SCRAPE_RUNS ORDER BY rowid DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var epoch, data string
		var run LibraryPluginRun
		if err := rows.Scan(&epoch, &data); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(data), &run) != nil {
			return nil, ErrState
		}
		run.Epoch = epoch
		result = append(result, run)
	}
	return result, rows.Err()
}
func (s *Store) LibraryPluginUncertain(ctx context.Context, ids []string) (bool, error) {
	if len(ids) > 16 {
		return true, ErrLimit
	}
	for _, id := range ids {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE JOB_ID=? AND STATE IN('writing','uncertain','restore_running')`, id).Scan(&n); err != nil {
			return true, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}
