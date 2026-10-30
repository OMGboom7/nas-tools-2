package subtitles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

var ErrState = errors.New("subtitle receipt requires review")

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db: db} }

type Receipt struct {
	ID                                               string  `json:"id"`
	JobID                                            string  `json:"jobId"`
	Ordinal                                          int     `json:"ordinal"`
	Name                                             string  `json:"name"`
	State                                            string  `json:"state"`
	Reason                                           string  `json:"reason,omitempty"`
	ExternalID                                       string  `json:"externalId,omitempty"`
	Updated                                          string  `json:"updated"`
	LocalPath, MediaType, OriginDigest, ConfigDigest string  `json:"-"`
	Payload                                          Payload `json:"-"`
}

func schema(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SUBTITLE_JOBS(ID TEXT PRIMARY KEY,JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,NAME TEXT NOT NULL,LOCAL_PATH TEXT NOT NULL,MEDIA_TYPE TEXT NOT NULL,ORIGIN_DIGEST TEXT NOT NULL,CONFIG_DIGEST TEXT NOT NULL,PAYLOAD TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,EXTERNAL_ID TEXT NOT NULL DEFAULT '',UPDATED TEXT NOT NULL,UNIQUE(JOB_ID,ORDINAL))`)
	return err
}
func setting(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
func configured(ctx context.Context, tx *sql.Tx) (Config, string, bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='SYSTEM_DICT')`).Scan(&exists); err != nil || !exists {
		return Config{}, "", false, err
	}
	raw, err := setting(ctx, tx, "UserInstalledPlugins")
	if err != nil {
		return Config{}, "", false, err
	}
	var ids []string
	if raw != "" && json.Unmarshal([]byte(raw), &ids) != nil {
		return Config{}, "", true, ErrConfig
	}
	installed := false
	for _, id := range ids {
		installed = installed || id == Plugin
	}
	if !installed {
		return Config{}, "", false, nil
	}
	raw, err = setting(ctx, tx, ConfigKey)
	if err != nil {
		return Config{}, "", true, err
	}
	var values map[string]any
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &values) != nil || values == nil {
		return Config{}, "", true, ErrConfig
	}
	epoch, err := setting(ctx, tx, EpochKey)
	if err != nil {
		return Config{}, "", true, err
	}
	c, err := Parse(values)
	return c, bus.Hash([]any{values, epoch}), true, err
}

// Emit is called inside the successful intake transaction. Configuration errors
// create blocked local receipts, not failed media intake or invented requests.
func Emit(ctx context.Context, tx *sql.Tx, jobID string, ordinal int, name, file, kind, originDigest string, disc ...bool) error {
	return emit(ctx, tx, jobID, ordinal, name, file, kind, originDigest, false, disc...)
}
func EmitIntake(ctx context.Context, tx *sql.Tx, jobID string, ordinal int, name, file, kind, originDigest string, disc bool) error {
	return emit(ctx, tx, jobID, ordinal, name, file, kind, originDigest, true, disc)
}
func IntakeID(job string, ordinal int) string {
	return bus.Hash([]string{"subtitle-intake-v2", job, strconv.Itoa(ordinal)})
}
func emit(ctx context.Context, tx *sql.Tx, jobID string, ordinal int, name, file, kind, originDigest string, bound bool, disc ...bool) error {
	c, digest, enabled, err := configured(ctx, tx)
	if err != nil && !errors.Is(err, ErrConfig) {
		return err
	}
	if !enabled {
		return nil
	}
	state, reason := "pending", ""
	var payload Payload
	if err != nil {
		state, reason = "blocked", "plugin configuration unavailable; no submission attempted"
	} else {
		payload, err = c.Payload(file, kind)
		if len(disc) == 1 && disc[0] {
			payload, err = c.BluRayPayload(file, kind)
		}
		if err != nil {
			state, reason = "blocked", "unsupported media path or type; no submission attempted"
		}
	}
	if err := schema(ctx, tx); err != nil {
		return err
	}
	encoded, _ := json.Marshal(payload)
	id := bus.Hash([]string{jobID, strconv.Itoa(ordinal)})
	if bound {
		id = IntakeID(jobID, ordinal)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_SUBTITLE_JOBS(ID,JOB_ID,ORDINAL,NAME,LOCAL_PATH,MEDIA_TYPE,ORIGIN_DIGEST,CONFIG_DIGEST,PAYLOAD,STATE,REASON,UPDATED) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(ID) DO NOTHING`, id, jobID, ordinal, bus.Bounded(name, 600), file, kind, originDigest, digest, string(encoded), state, reason, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var present bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SUBTITLE_JOBS WHERE ID=? AND JOB_ID=? AND ORDINAL=? AND LOCAL_PATH=? AND MEDIA_TYPE=? AND ORIGIN_DIGEST=?)`, id, jobID, ordinal, file, kind, originDigest).Scan(&present)
		if err != nil {
			return err
		}
		if !present {
			return ErrState
		}
		return nil
	}
	if n != 1 {
		return ErrState
	}
	saved, err := scan(tx.QueryRowContext(ctx, selectSQL+` WHERE ID=?`, id))
	expected := Receipt{ID: id, JobID: jobID, Ordinal: ordinal, Name: bus.Bounded(name, 600), LocalPath: file, MediaType: kind, OriginDigest: originDigest, ConfigDigest: digest, Payload: payload, State: state, Reason: reason, Updated: now}
	if err != nil || receiptDigest(saved) != receiptDigest(expected) {
		return ErrState
	}
	return nil
}

const selectSQL = `SELECT ID,JOB_ID,ORDINAL,NAME,LOCAL_PATH,MEDIA_TYPE,ORIGIN_DIGEST,CONFIG_DIGEST,PAYLOAD,STATE,REASON,EXTERNAL_ID,UPDATED FROM GO_SUBTITLE_JOBS`

func receiptDigest(r Receipt) string {
	return bus.Hash([]any{r, r.LocalPath, r.MediaType, r.OriginDigest, r.ConfigDigest, r.Payload})
}
func scan(row interface{ Scan(...any) error }) (Receipt, error) {
	var r Receipt
	var raw string
	err := row.Scan(&r.ID, &r.JobID, &r.Ordinal, &r.Name, &r.LocalPath, &r.MediaType, &r.OriginDigest, &r.ConfigDigest, &raw, &r.State, &r.Reason, &r.ExternalID, &r.Updated)
	if err != nil {
		return r, err
	}
	if json.Unmarshal([]byte(raw), &r.Payload) != nil {
		return r, ErrState
	}
	return r, nil
}
func (s *Store) List(ctx context.Context, pending bool) ([]Receipt, error) {
	result := []Receipt{}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SUBTITLE_JOBS' AND type='table')`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	query := selectSQL + ` ORDER BY STATE IN ('pending','sending','uncertain','blocked') DESC,UPDATED DESC,ID LIMIT 100`
	args := []any{}
	if pending {
		query = selectSQL + ` WHERE STATE='pending' ORDER BY UPDATED,ID LIMIT 20`
		var waits bool
		if e := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SUBTITLE_WAITS' AND type='table')`).Scan(&waits); e != nil {
			return nil, e
		}
		if waits {
			query = selectSQL + ` WHERE STATE='pending' AND NOT EXISTS(SELECT 1 FROM GO_SUBTITLE_WAITS w WHERE w.ID=GO_SUBTITLE_JOBS.ID AND w.NEXT_CHECK>?) ORDER BY UPDATED,ID LIMIT 20`
			args = []any{time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")}
		}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) Claim(ctx context.Context, expected Receipt, source ...func(*sql.Tx, Receipt) error) (Config, Receipt, error) {
	if len(source) > 1 {
		return Config{}, expected, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Config{}, Receipt{}, err
	}
	defer tx.Rollback()
	r, err := scan(tx.QueryRowContext(ctx, selectSQL+` WHERE ID=?`, expected.ID))
	if err != nil {
		return Config{}, r, err
	}
	if r.State != "pending" || receiptDigest(r) != receiptDigest(expected) {
		return Config{}, r, ErrState
	}
	c, digest, enabled, err := configured(ctx, tx)
	if err != nil && !errors.Is(err, ErrConfig) {
		return Config{}, r, err
	}
	state, reason := "sending", ""
	payload, payloadErr := c.Payload(r.LocalPath, r.MediaType)
	if r.Payload.Bluray {
		payload, payloadErr = c.BluRayPayload(r.LocalPath, r.MediaType)
	}
	if err != nil || !enabled || digest != r.ConfigDigest || payloadErr != nil || bus.Hash(payload) != bus.Hash(r.Payload) {
		state, reason = "blocked", "plugin configuration or mapping changed; no submission attempted"
	}
	if state == "sending" && len(source) == 1 {
		if source[0] == nil {
			return Config{}, r, ErrState
		}
		if e := source[0](tx, r); errors.Is(e, bus.ErrEventWaiting) {
			return Config{}, r, e
		} else if e != nil {
			state, reason = "blocked", "original intake or metadata prerequisite changed; no submission attempted"
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE GO_SUBTITLE_JOBS SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='pending'`, state, reason, now, r.ID)
	if err != nil {
		return Config{}, r, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return Config{}, r, ErrState
	}
	next := r
	next.State = state
	next.Reason = reason
	next.Updated = now
	stored, err := scan(tx.QueryRowContext(ctx, selectSQL+` WHERE ID=?`, r.ID))
	if err != nil || receiptDigest(stored) != receiptDigest(next) {
		return Config{}, r, ErrState
	}
	if state == "sending" {
		current, currentDigest, currentEnabled, e := configured(ctx, tx)
		if e != nil || !currentEnabled || currentDigest != digest || bus.Hash(current) != bus.Hash(c) {
			return Config{}, r, ErrState
		}
		if len(source) == 1 && source[0](tx, next) != nil {
			return Config{}, r, ErrState
		}
	}
	if err := tx.Commit(); err != nil {
		return Config{}, r, err
	}
	if state == "blocked" {
		return Config{}, r, ErrState
	}
	return c, next, nil
}
func (s *Store) Finish(ctx context.Context, id, state, externalID string) error {
	if state != "submitted" && state != "uncertain" && state != "blocked" || state == "submitted" && !ValidID(externalID) || state != "submitted" && externalID != "" {
		return ErrState
	}
	reason := ""
	if state == "uncertain" {
		reason = "response not positively acknowledged; inspect original service, do not replay"
	}
	if state == "blocked" {
		reason = "original reviewed media unavailable or changed; no submission attempted"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	previous, err := scan(tx.QueryRowContext(ctx, selectSQL+` WHERE ID=?`, id))
	if err != nil {
		return err
	}
	if previous.State != "sending" || previous.ExternalID != "" {
		return ErrState
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE GO_SUBTITLE_JOBS SET STATE=?,REASON=?,EXTERNAL_ID=?,UPDATED=? WHERE ID=? AND STATE='sending'`, state, reason, externalID, now, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrState
	}
	next := previous
	next.State = state
	next.Reason = reason
	next.ExternalID = externalID
	next.Updated = now
	saved, err := scan(tx.QueryRowContext(ctx, selectSQL+` WHERE ID=?`, id))
	if err != nil || receiptDigest(saved) != receiptDigest(next) {
		return ErrState
	}
	return tx.Commit()
}
