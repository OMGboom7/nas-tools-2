package organization

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

type SyncConfigJob struct {
	ID       string `json:"id"`
	ConfigID string `json:"configId"`
	Mode     string `json:"mode"`
}
type SyncConfigReview struct {
	Action       string          `json:"action"`
	Actor        string          `json:"actor"`
	Before       []SyncRecord    `json:"before"`
	After        []SyncRecord    `json:"after"`
	Jobs         []SyncConfigJob `json:"jobs"`
	ConfigDigest string          `json:"configDigest,omitempty"`
	Roots        []string        `json:"roots,omitempty"`
	RestoreID    string          `json:"restoreId,omitempty"`
	Fingerprint  string          `json:"fingerprint"`
}
type SyncConfigReceipt struct {
	ID        string           `json:"id"`
	Review    SyncConfigReview `json:"review"`
	After     []SyncRecord     `json:"after"`
	Created   string           `json:"created"`
	RestoreID string           `json:"restoreId,omitempty"`
}

func formatSyncID(id int64) string { return strconv.FormatInt(id, 10) }
func FinalizeSyncReview(review SyncConfigReview) SyncConfigReview {
	review.Fingerprint = ""
	review.Fingerprint = Digest(review)
	return review
}

func syncAffectedJobs(ctx context.Context, reader syncReader, before, after []SyncRecord) ([]SyncConfigJob, error) {
	changed := map[int64]bool{}
	next := map[int64]SyncRecord{}
	for _, r := range after {
		next[r.ID] = r
	}
	for _, r := range before {
		if n, ok := next[r.ID]; !ok || Digest(r) != Digest(n) {
			changed[r.ID] = true
		}
	}
	graphChanged := SyncGraphDigest(before) != SyncGraphDigest(after)
	rows, err := reader.QueryContext(ctx, `SELECT ID,DEFINITION FROM GO_ORGANIZATION_JOBS j WHERE STATE='active' AND EXISTS(SELECT 1 FROM GO_ORGANIZATION_ITEMS i WHERE i.JOB_ID=j.ID AND (i.STATE!='completed' OR i.REASON!='')) ORDER BY ID LIMIT 1025`)
	if err != nil {
		return nil, err
	}
	jobs := []SyncConfigJob{}
	count := 0
	for rows.Next() {
		count++
		if count > 1024 {
			rows.Close()
			return nil, ErrLimit
		}
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var d Definition
		if json.Unmarshal([]byte(raw), &d) != nil {
			rows.Close()
			return nil, ErrState
		}
		if d.Sync != nil && (graphChanged || changed[d.Sync.Record.ID]) {
			jobs = append(jobs, SyncConfigJob{id, formatSyncID(d.Sync.Record.ID), d.Mode})
		}
	}
	err = rows.Err()
	rows.Close()
	return jobs, err
}
func (s *Store) SyncAffectedJobs(ctx context.Context, before, after []SyncRecord) ([]SyncConfigJob, error) {
	return syncAffectedJobs(ctx, s.db, before, after)
}

func syncSQLValues(r SyncRecord) []any {
	nulls := map[string]bool{}
	for _, key := range r.NullFields {
		nulls[key] = true
	}
	fields := []struct {
		key   string
		value any
	}{{"source", r.Source}, {"dest", r.Dest}, {"unknown", r.Unknown}, {"mode", r.Mode}, {"compatibility", r.Compatibility}, {"rename", r.Rename}, {"enabled", r.Enabled}, {"note", r.Note}, {"locating", r.Locating}}
	values := []any{}
	for _, field := range fields {
		if nulls[field.key] {
			values = append(values, nil)
		} else {
			values = append(values, field.value)
		}
	}
	return values
}
func syncConfigTables(ctx context.Context, tx syncReader) error {
	// SQL execution is separate below; this helper only verifies the expected
	// schema before deleting rows, so unknown application columns aren't lost.
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(CONFIG_SYNC_PATHS)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	known := map[string]bool{"ID": true, "SOURCE": true, "DEST": true, "UNKNOWN": true, "MODE": true, "COMPATIBILITY": true, "RENAME": true, "ENABLED": true, "NOTE": true, "LOCATING": true}
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var def any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
			return err
		}
		if !known[strings.ToUpper(name)] {
			return ErrState
		}
	}
	return rows.Err()
}

func (s *Store) ApplySyncConfig(ctx context.Context, review SyncConfigReview, allowJobInvalidation bool) (SyncConfigReceipt, error) {
	if review.Actor == "" || review.Fingerprint != FinalizeSyncReview(review).Fingerprint {
		return SyncConfigReceipt{}, ErrState
	}
	if len(review.Before) > 128 || len(review.After) > 128 {
		return SyncConfigReceipt{}, ErrLimit
	}
	seen := map[int64]bool{}
	for _, r := range review.After {
		if r.ID < 0 || seen[r.ID] {
			return SyncConfigReceipt{}, ErrState
		}
		seen[r.ID] = true
	}
	switch review.Action {
	case "create", "update", "delete", "status", "restore":
	default:
		return SyncConfigReceipt{}, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SyncConfigReceipt{}, err
	}
	defer tx.Rollback()
	current, err := listSync(ctx, tx)
	if err != nil {
		return SyncConfigReceipt{}, err
	}
	if Digest(current) != Digest(review.Before) {
		return SyncConfigReceipt{}, ErrState
	}
	jobs, err := syncAffectedJobs(ctx, tx, current, review.After)
	if err != nil {
		return SyncConfigReceipt{}, err
	}
	if Digest(jobs) != Digest(review.Jobs) || len(jobs) > 0 && !allowJobInvalidation {
		return SyncConfigReceipt{}, ErrState
	}
	if review.Action == "restore" {
		receipt, err := readSyncConfigReceipt(ctx, tx, review.RestoreID)
		if err != nil {
			return SyncConfigReceipt{}, err
		}
		if receipt.RestoreID != "" || Digest(receipt.After) != Digest(current) || Digest(receipt.Review.Before) != Digest(review.After) {
			return SyncConfigReceipt{}, ErrState
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS CONFIG_SYNC_PATHS(ID INTEGER PRIMARY KEY,SOURCE TEXT,DEST TEXT,UNKNOWN TEXT,MODE TEXT,COMPATIBILITY INTEGER,RENAME INTEGER,ENABLED INTEGER,NOTE TEXT,LOCATING INTEGER)`); err != nil {
		return SyncConfigReceipt{}, err
	}
	old := map[int64]SyncRecord{}
	next := map[int64]SyncRecord{}
	for _, r := range current {
		old[r.ID] = r
	}
	for _, r := range review.After {
		next[r.ID] = r
	}
	for _, r := range current {
		if _, ok := next[r.ID]; !ok {
			if err := syncConfigTables(ctx, tx); err != nil {
				return SyncConfigReceipt{}, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM CONFIG_SYNC_PATHS WHERE ID=?`, r.ID); err != nil {
				return SyncConfigReceipt{}, err
			}
		}
	}
	after := append([]SyncRecord{}, review.After...)
	for i, r := range after {
		values := syncSQLValues(r)
		if prior, ok := old[r.ID]; ok {
			if Digest(prior) == Digest(r) {
				continue
			}
			values = append(values, r.ID)
			if _, err := tx.ExecContext(ctx, `UPDATE CONFIG_SYNC_PATHS SET SOURCE=?,DEST=?,UNKNOWN=?,MODE=?,COMPATIBILITY=?,RENAME=?,ENABLED=?,NOTE=?,LOCATING=? WHERE ID=?`, values...); err != nil {
				return SyncConfigReceipt{}, err
			}
		} else if r.ID == 0 {
			if err := tx.QueryRowContext(ctx, `INSERT INTO CONFIG_SYNC_PATHS(SOURCE,DEST,UNKNOWN,MODE,COMPATIBILITY,RENAME,ENABLED,NOTE,LOCATING) VALUES(?,?,?,?,?,?,?,?,?) RETURNING ID`, values...).Scan(&after[i].ID); err != nil {
				return SyncConfigReceipt{}, err
			}
		} else {
			if review.Action != "restore" {
				return SyncConfigReceipt{}, ErrState
			}
			values = append([]any{r.ID}, values...)
			if _, err := tx.ExecContext(ctx, `INSERT INTO CONFIG_SYNC_PATHS(ID,SOURCE,DEST,UNKNOWN,MODE,COMPATIBILITY,RENAME,ENABLED,NOTE,LOCATING) VALUES(?,?,?,?,?,?,?,?,?,?)`, values...); err != nil {
				return SyncConfigReceipt{}, err
			}
		}
	}
	sort.Slice(after, func(i, j int) bool { return after[i].ID < after[j].ID })
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return SyncConfigReceipt{}, err
	}
	receipt := SyncConfigReceipt{ID: hex.EncodeToString(random[:]), Review: review, After: after, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SYNC_CONFIG_CHANGES(ID TEXT PRIMARY KEY,RECEIPT TEXT NOT NULL,RESTORE TEXT NOT NULL DEFAULT '')`); err != nil {
		return SyncConfigReceipt{}, err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return SyncConfigReceipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_SYNC_CONFIG_CHANGES(ID,RECEIPT) VALUES(?,?)`, receipt.ID, string(raw)); err != nil {
		return SyncConfigReceipt{}, err
	}
	if review.Action == "restore" {
		if _, err := tx.ExecContext(ctx, `UPDATE GO_SYNC_CONFIG_CHANGES SET RESTORE=? WHERE ID=? AND RESTORE=''`, receipt.ID, review.RestoreID); err != nil {
			return SyncConfigReceipt{}, err
		}
	}
	if err := pauseMonitorControl(ctx, tx, "sync configuration changed; explicit reactivation required"); err != nil {
		return SyncConfigReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return SyncConfigReceipt{}, err
	}
	return receipt, nil
}

func readSyncConfigReceipt(ctx context.Context, reader unknownReader, id string) (SyncConfigReceipt, error) {
	var raw, restore string
	receipt := SyncConfigReceipt{}
	if err := reader.QueryRowContext(ctx, `SELECT RECEIPT,RESTORE FROM GO_SYNC_CONFIG_CHANGES WHERE ID=?`, id).Scan(&raw, &restore); err != nil {
		return receipt, err
	}
	if json.Unmarshal([]byte(raw), &receipt) != nil || receipt.ID != id || receipt.Created == "" || receipt.Review.Actor == "" || receipt.Review.Fingerprint != FinalizeSyncReview(receipt.Review).Fingerprint {
		return receipt, ErrState
	}
	if len(receipt.After) != len(receipt.Review.After) {
		return receipt, ErrState
	}
	expected := map[int64]SyncRecord{}
	var inserted *SyncRecord
	for _, r := range receipt.Review.After {
		if _, ok := expected[r.ID]; ok {
			return receipt, ErrState
		}
		expected[r.ID] = r
		if r.ID == 0 {
			copy := r
			inserted = &copy
		}
	}
	actual := map[int64]bool{}
	for _, r := range receipt.After {
		if r.ID < 1 || actual[r.ID] {
			return receipt, ErrState
		}
		actual[r.ID] = true
		want, ok := expected[r.ID]
		if !ok {
			if inserted == nil {
				return receipt, ErrState
			}
			want = *inserted
			want.ID = r.ID
			inserted = nil
		} else {
			delete(expected, r.ID)
		}
		if Digest(want) != Digest(r) {
			return receipt, ErrState
		}
	}
	delete(expected, 0)
	if len(expected) != 0 || inserted != nil {
		return receipt, ErrState
	}
	receipt.RestoreID = restore
	return receipt, nil
}
func (s *Store) GetSyncConfigReceipt(ctx context.Context, id string) (SyncConfigReceipt, error) {
	return readSyncConfigReceipt(ctx, s.db, id)
}
func (s *Store) ListSyncConfigReceipts(ctx context.Context) ([]SyncConfigReceipt, error) {
	items := []SyncConfigReceipt{}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SYNC_CONFIG_CHANGES')`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return items, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_SYNC_CONFIG_CHANGES ORDER BY rowid DESC LIMIT 30`)
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
		receipt, err := s.GetSyncConfigReceipt(ctx, id)
		if err != nil {
			return nil, err
		}
		items = append(items, receipt)
	}
	return items, nil
}
