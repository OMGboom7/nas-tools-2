package sitelogin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func (s *Store) Start(ctx context.Context, id string, guard Guard) error {
	return s.change(ctx, id, guard, true, func(r *Record, _ *sql.Tx) error {
		if r.State != "queued" {
			return ErrState
		}
		r.State = "running"
		return nil
	})
}
func (s *Store) Step(ctx context.Context, id, binding, kind, signature string, guard Guard) (int, error) {
	index := -1
	err := s.change(ctx, id, guard, true, func(r *Record, _ *sql.Tx) error {
		if !Active(r.State) || r.State == "queued" || Hash(r.Source) != binding || !Hex(signature, 64) || !Text(kind, 80) || kind == "" || len(r.Operations) >= MaxOperations {
			return ErrState
		}
		for _, op := range r.Operations {
			if op.Signature == signature || op.State == "uncertain" || op.State == "denied" || op.State == "intent" && !strings.HasPrefix(op.Kind, "action:") {
				return ErrState
			}
		}
		index = len(r.Operations)
		r.Operations = append(r.Operations, Operation{Kind: kind, Signature: signature, State: "intent"})
		return nil
	})
	return index, err
}
func (s *Store) FinishStep(ctx context.Context, id, binding string, index int, state, digest string) error {
	if state != "received" && state != "uncertain" && state != "denied" || digest != "" && !Hex(digest, 64) {
		return ErrState
	}
	// A response receipt may still be acknowledged after source revocation or a
	// local stop. It never authorizes a new request or cookie publication.
	return s.change(ctx, id, func(ctx context.Context) error { return ctx.Err() }, false, func(r *Record, _ *sql.Tx) error {
		if Hash(r.Source) != binding || index < 0 || index >= len(r.Operations) || r.Operations[index].State != "intent" {
			return ErrState
		}
		r.Operations[index].State, r.Operations[index].Digest = state, digest
		return nil
	})
}
func (s *Store) Prompt(ctx context.Context, id, kind, nonce string, deadline time.Time, guard Guard) error {
	return s.change(ctx, id, guard, true, func(r *Record, _ *sql.Tx) error {
		if r.State != "running" || !Hex(nonce, 64) || !deadline.After(time.Now()) || time.Until(deadline) > 2*time.Minute {
			return ErrState
		}
		switch kind {
		case "captcha", "two_step", "email", "email_code":
		default:
			return ErrInput
		}
		r.State, r.Need, r.Nonce, r.Deadline = "waiting_input", kind, nonce, deadline.UTC().Format(time.RFC3339Nano)
		return nil
	})
}
func (s *Store) ConsumePrompt(ctx context.Context, id, nonce string, guard Guard) error {
	return s.change(ctx, id, guard, true, func(r *Record, _ *sql.Tx) error {
		deadline, err := time.Parse(time.RFC3339Nano, r.Deadline)
		if r.State != "waiting_input" || r.Nonce != nonce || err != nil || !time.Now().Before(deadline) {
			return ErrInput
		}
		r.State, r.Need, r.Nonce, r.Deadline = "running", "", "", ""
		return nil
	})
}
func (s *Store) Finish(ctx context.Context, id, state, reason string) error {
	switch state {
	case "failed", "blocked", "uncertain", "stopped":
	default:
		return ErrState
	}
	if !Text(reason, 800) {
		return ErrState
	}
	return s.change(ctx, id, func(ctx context.Context) error { return ctx.Err() }, false, func(r *Record, _ *sql.Tx) error {
		if !Active(r.State) {
			return ErrState
		}
		for _, op := range r.Operations {
			if op.State == "intent" || op.State == "uncertain" {
				state = "uncertain"
			}
		}
		r.State, r.Reason, r.Need, r.Nonce, r.Deadline = state, reason, "", "", ""
		return nil
	})
}
func (s *Store) SaveCookie(ctx context.Context, id, cookie, agent, outcome string, guard Guard) error {
	if !Text(cookie, 16000) || cookie == "" || !Text(agent, 512) || agent == "" || outcome != "existing" && outcome != "acquired" {
		return ErrState
	}
	return s.change(ctx, id, guard, true, func(r *Record, tx *sql.Tx) error {
		if r.State != "running" {
			return ErrState
		}
		for _, op := range r.Operations {
			if op.State != "received" {
				return ErrState
			}
		}
		site, err := readSite(ctx, tx, r.Source.SiteID)
		if err != nil || !SourceMatches(r.Source, site) {
			return ErrScope
		}
		note, err := siteconfig.CookieNote(site.Note, agent)
		if err != nil {
			return ErrScope
		}
		res, err := tx.ExecContext(ctx, "UPDATE CONFIG_SITE SET COOKIE=?,NOTE=? WHERE ID=?", cookie, note, site.ID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n != 1 {
			return ErrState
		}
		expected := site
		expected.Cookie, expected.Note = cookie, note
		saved, err := readSite(ctx, tx, site.ID)
		if err != nil || Hash(saved) != Hash(expected) {
			return ErrState
		}
		if guard(ctx) != nil {
			return ErrScope
		}
		r.State, r.LoginState, r.Reason = "completed", outcome, "original site session verified and saved"
		r.Need, r.Nonce, r.Deadline = "", "", ""
		return nil
	})
}

// Recover is called only while holding the application's cross-process cookie
// workflow guard. It closes interrupted work and never restores credentials.
func (s *Store) Recover(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT ID FROM GO_SITE_LOGIN_JOBS WHERE STATE IN('queued','running','waiting_input') LIMIT 501")
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(ids) > 500 {
		return ErrState
	}
	for _, id := range ids {
		r, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		if err = s.Finish(ctx, id, Interrupted(r), "original process ended; no request replayed"); err != nil {
			return err
		}
	}
	rows, err = s.db.QueryContext(ctx, `SELECT ID FROM GO_SITE_LOGIN_BATCHES WHERE json_extract(DOCUMENT,'$.RememberState')='pending' LIMIT 501`)
	if err != nil {
		return err
	}
	batches := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		batches = append(batches, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(batches) > 500 {
		return ErrState
	}
	for _, id := range batches {
		if err = s.RememberResult(ctx, id, "uncertain", "original process ended; stored credential update not replayed"); err != nil {
			return err
		}
	}
	return nil
}
func Saved(ctx context.Context, q rowReader) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT COALESCE(VALUE,'') FROM SYSTEM_DICT WHERE KEY=? ORDER BY ID DESC LIMIT 1", SavedKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
func (s *Store) Saved(ctx context.Context) (string, error) { return Saved(ctx, s.db) }

// Remember retains the original CookieUserInfo key and all unknown values.
// One-time codes supplied for this login do not replace its old fields.
func (s *Store) Remember(ctx context.Context, expected, username, password string, guard Guard) error {
	if guard == nil || guard(ctx) != nil || !Text(username, 1024) || username == "" || !Text(password, 4096) || password == "" {
		return ErrScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := Saved(ctx, tx)
	if err != nil || before != expected {
		return ErrScope
	}
	values := map[string]json.RawMessage{}
	if before != "" && before != "null" {
		if len(before) > 64<<10 || json.Unmarshal([]byte(before), &values) != nil || values == nil {
			return ErrScope
		}
	}
	values["username"], _ = json.Marshal(username)
	values["password"], _ = json.Marshal(password)
	raw, err := json.Marshal(values)
	if err != nil {
		return ErrScope
	}
	var id int64
	err = tx.QueryRowContext(ctx, "SELECT ID FROM SYSTEM_DICT WHERE KEY=? ORDER BY ID DESC LIMIT 1", SavedKey).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, "INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)", SavedKey, string(raw))
	} else if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE SYSTEM_DICT SET VALUE=? WHERE ID=? AND KEY=?", string(raw), id, SavedKey)
	}
	if err != nil {
		return err
	}
	after, err := Saved(ctx, tx)
	if err != nil || after != string(raw) {
		return ErrState
	}
	if guard(ctx) != nil {
		return ErrScope
	}
	return tx.Commit()
}
func (s *Store) RememberResult(ctx context.Context, id, state, reason string) error {
	switch state {
	case "saved", "preserved", "blocked", "uncertain":
	default:
		return ErrState
	}
	if !Text(reason, 800) {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, "SELECT DOCUMENT FROM GO_SITE_LOGIN_BATCHES WHERE ID=?", id).Scan(&raw); err != nil {
		return err
	}
	var b Batch
	if json.Unmarshal([]byte(raw), &b) != nil || b.ID != id || !b.Remember || b.RememberState != "pending" {
		return ErrState
	}
	b.RememberState, b.RememberReason, b.Updated = state, reason, time.Now().UTC().Format(time.RFC3339Nano)
	next := mustJSON(b)
	result, err := tx.ExecContext(ctx, "UPDATE GO_SITE_LOGIN_BATCHES SET DOCUMENT=? WHERE ID=? AND DOCUMENT=?", next, id, raw)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrState
	}
	var saved string
	if err = tx.QueryRowContext(ctx, "SELECT DOCUMENT FROM GO_SITE_LOGIN_BATCHES WHERE ID=?", id).Scan(&saved); err != nil || saved != next {
		return ErrState
	}
	return tx.Commit()
}
