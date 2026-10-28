package speedlimit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db} }

type Reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func table(ctx context.Context, r Reader, name string) (bool, error) {
	var yes bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&yes)
	return yes, err
}
func setting(ctx context.Context, r Reader, key string) (string, error) {
	yes, err := table(ctx, r, "SYSTEM_DICT")
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
func RawConfigDigest(raw string) string {
	var v any
	d := json.NewDecoder(stringsReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return ""
	}
	return Digest(v)
}
func installed(ctx context.Context, r Reader) (bool, error) {
	raw, err := setting(ctx, r, "UserInstalledPlugins")
	if err != nil || raw == "" {
		return false, err
	}
	var ids []string
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &ids) != nil || len(ids) > 1024 {
		return false, ErrState
	}
	for _, id := range ids {
		if id == Plugin {
			return true, nil
		}
	}
	return false, nil
}
func Targets(ctx context.Context, r Reader, spec Spec) ([]Target, error) {
	if len(spec.Downloaders) > 64 || len(spec.Ratios) > 0 && len(spec.Ratios) != len(spec.Downloaders) {
		return nil, ErrState
	}
	out := []Target{}
	yes, err := table(ctx, r, "DOWNLOADER")
	if err != nil || !yes {
		return out, err
	}
	for i, id := range spec.Downloaders {
		var name, kind, config string
		var enabled int
		err := r.QueryRowContext(ctx, `SELECT COALESCE(NAME,''),COALESCE(TYPE,''),COALESCE(CONFIG,''),COALESCE(ENABLED,0) FROM DOWNLOADER WHERE ID=?`, id).Scan(&name, &kind, &config, &enabled)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if enabled == 0 {
			continue
		}
		if kind != "qbittorrent" && kind != "transmission" {
			return nil, ErrState
		}
		weight := int64(1)
		if len(spec.Ratios) > 0 {
			weight = spec.Ratios[i]
		}
		out = append(out, Target{ID: id, Name: name, Kind: kind, Digest: Digest([]any{id, name, kind, config, enabled}), Weight: weight})
	}
	return out, nil
}
func (s *Store) Targets(ctx context.Context, spec Spec) ([]Target, error) {
	return Targets(ctx, s.db, spec)
}
func readScope(ctx context.Context, r Reader) (Control, Spec, bool, error) {
	c, spec := Control{}, Spec{}
	raw, err := setting(ctx, r, ControlKey)
	if err != nil {
		return c, spec, false, err
	}
	c, err = DecodeControl(raw)
	if err != nil || !c.Authorized {
		return c, spec, false, err
	}
	yes, err := installed(ctx, r)
	if err != nil || !yes {
		return c, spec, false, err
	}
	raw, err = setting(ctx, r, ConfigKey)
	if err != nil {
		return c, spec, false, err
	}
	spec, err = DecodeSpec(raw)
	if err != nil || RawConfigDigest(raw) != c.ConfigDigest {
		return c, spec, false, ErrState
	}
	targets, err := Targets(ctx, r, spec)
	if err != nil || len(targets) != len(c.Baselines) {
		return c, spec, false, ErrState
	}
	for i, target := range targets {
		if target != c.Baselines[i].Target {
			return c, spec, false, ErrState
		}
	}
	return c, spec, true, nil
}
func (s *Store) Scope(ctx context.Context) (Control, Spec, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Control{}, Spec{}, false, err
	}
	defer tx.Rollback()
	c, spec, active, err := readScope(ctx, tx)
	if err != nil {
		return c, spec, false, err
	}
	return c, spec, active, tx.Commit()
}
func schema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_SPEED_LIMITER_REVIEWS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FP TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SPEED_LIMITER_OWNERS(EPOCH TEXT NOT NULL,TARGET_ID TEXT NOT NULL,RATES TEXT NOT NULL,FP TEXT NOT NULL,PRIMARY KEY(EPOCH,TARGET_ID))`,
		`CREATE TABLE IF NOT EXISTS GO_SPEED_LIMITER_TRIGGERS(ID TEXT PRIMARY KEY,EPOCH TEXT NOT NULL,SOURCE TEXT NOT NULL,REFERENCE TEXT NOT NULL,DUE TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS GO_SPEED_LIMITER_DUE ON GO_SPEED_LIMITER_TRIGGERS(STATE,DUE,ID)`,
		`CREATE TABLE IF NOT EXISTS GO_SPEED_LIMITER_JOBS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FP TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SPEED_LIMITER_OPERATIONS(JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,EPOCH TEXT NOT NULL,TARGET_ID TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,ACTOR TEXT NOT NULL,UPDATED TEXT NOT NULL,PRIMARY KEY(JOB_ID,ORDINAL))`,
		`CREATE INDEX IF NOT EXISTS GO_SPEED_LIMITER_UNRESOLVED ON GO_SPEED_LIMITER_OPERATIONS(TARGET_ID,STATE)`,
		`CREATE TABLE IF NOT EXISTS GO_SPEED_LIMITER_PLAYSTATE(EPOCH TEXT PRIMARY KEY,PLAYING INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SPEED_LIMITER_JOB_FINALS(JOB_ID TEXT PRIMARY KEY,STATE TEXT NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func setControl(ctx context.Context, tx *sql.Tx, c Control) error {
	raw, _ := json.Marshal(c)
	if _, err := DecodeControl(string(raw)); err != nil {
		return err
	}
	var previous int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, ControlKey).Scan(&previous); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, string(raw), ControlKey)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != previous {
		return ErrState
	}
	if previous == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE) VALUES('SystemConfig',?,?,'')`, ControlKey, string(raw))
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrState
		}
	}
	var total, matches int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(VALUE=?),0) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, string(raw), ControlKey).Scan(&total, &matches); err != nil || total == 0 || total != matches {
		return ErrState
	}
	return nil
}
func rawBasis(ctx context.Context, r Reader) (string, error) {
	values := map[string]string{}
	for _, key := range []string{"UserInstalledPlugins", ConfigKey, ControlKey} {
		v, err := setting(ctx, r, key)
		if err != nil {
			return "", err
		}
		values[key] = v
	}
	return Digest(values), nil
}
func (s *Store) Basis(ctx context.Context) (string, error) { return rawBasis(ctx, s.db) }

type Review struct {
	ID      string  `json:"id"`
	Basis   string  `json:"-"`
	Control Control `json:"-"`
	Spec    Spec    `json:"spec"`
	Created string  `json:"created"`
}

func reviewDocument(p Review) any {
	return struct {
		ID, Basis string
		Control   Control
		Spec      Spec
		Created   string
	}{p.ID, p.Basis, p.Control, p.Spec, p.Created}
}
func (s *Store) SaveReview(ctx context.Context, p Review, validate func() error) error {
	if !Hex(p.ID, 32) || !Hex(p.Basis, 64) || p.Control.Authorized || len(p.Control.Baselines) == 0 || validate == nil {
		return ErrState
	}
	controlRaw, _ := json.Marshal(p.Control)
	if _, err := DecodeControl(string(controlRaw)); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, p.Created); err != nil {
		return ErrState
	}
	raw, _ := json.Marshal(reviewDocument(p))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	basis, err := rawBasis(ctx, tx)
	if err != nil || basis != p.Basis {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	if err := schema(ctx, tx); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_REVIEWS VALUES(?,?,?,'open',?)`, p.ID, string(raw), Digest(string(raw)), p.Created)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var saved, fp, state string
	if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT,FP,STATE FROM GO_SPEED_LIMITER_REVIEWS WHERE ID=?`, p.ID).Scan(&saved, &fp, &state); err != nil || saved != string(raw) || fp != Digest(saved) || state != "open" {
		return ErrState
	}
	basis, err = rawBasis(ctx, tx)
	if err != nil || basis != p.Basis {
		return ErrState
	}
	for _, b := range p.Control.Baselines {
		if _, err := targetConfig(ctx, tx, b.Target); err != nil {
			return err
		}
	}
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit()
}
func review(ctx context.Context, r Reader, id string) (Review, string, error) {
	p := Review{}
	if !Hex(id, 32) {
		return p, "", ErrMissing
	}
	yes, err := table(ctx, r, "GO_SPEED_LIMITER_REVIEWS")
	if err != nil {
		return p, "", err
	}
	if !yes {
		return p, "", ErrMissing
	}
	var raw, fp, state string
	err = r.QueryRowContext(ctx, `SELECT DOCUMENT,FP,STATE FROM GO_SPEED_LIMITER_REVIEWS WHERE ID=?`, id).Scan(&raw, &fp, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return p, "", ErrMissing
	}
	if err != nil {
		return p, "", err
	}
	var v struct {
		ID, Basis string
		Control   Control
		Spec      Spec
		Created   string
	}
	if len(raw) > 256<<10 || fp != Digest(raw) || json.Unmarshal([]byte(raw), &v) != nil || v.ID != id || !Hex(v.Basis, 64) {
		return p, state, ErrState
	}
	p = Review{v.ID, v.Basis, v.Control, v.Spec, v.Created}
	controlRaw, _ := json.Marshal(p.Control)
	if _, err := DecodeControl(string(controlRaw)); err != nil {
		return p, state, err
	}
	return p, state, nil
}
func (s *Store) Review(ctx context.Context, id string) (Review, error) {
	p, state, err := review(ctx, s.db, id)
	if err == nil && state != "open" {
		err = ErrState
	}
	return p, err
}
func unresolved(ctx context.Context, r Reader, id string) (bool, error) {
	yes, err := table(ctx, r, "GO_SPEED_LIMITER_OPERATIONS")
	if err != nil || !yes {
		return false, err
	}
	var found bool
	err = r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SPEED_LIMITER_OPERATIONS WHERE TARGET_ID=? AND STATE IN('intent','uncertain'))`, id).Scan(&found)
	return found, err
}
func (s *Store) Unresolved(ctx context.Context, id string) (bool, error) {
	return unresolved(ctx, s.db, id)
}
func (s *Store) Activate(ctx context.Context, p Review, validate func() error) error {
	if validate == nil {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, state, err := review(ctx, tx, p.ID)
	if err != nil || state != "open" || Digest(reviewDocument(current)) != Digest(reviewDocument(p)) {
		return ErrState
	}
	when, err := time.Parse(time.RFC3339Nano, p.Created)
	if err != nil || time.Since(when) < 0 || time.Since(when) > 10*time.Minute {
		return ErrState
	}
	basis, err := rawBasis(ctx, tx)
	if err != nil || basis != p.Basis {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	targets, err := Targets(ctx, tx, p.Spec)
	if err != nil || len(targets) != len(p.Control.Baselines) {
		return ErrState
	}
	for i, target := range targets {
		if target != p.Control.Baselines[i].Target {
			return ErrState
		}
		if yes, err := unresolved(ctx, tx, target.ID); err != nil || yes {
			return ErrState
		}
	}
	c := p.Control
	c.Authorized = true
	c.Created = nowString()
	if err := setControl(ctx, tx, c); err != nil {
		return err
	}
	for _, b := range c.Baselines {
		raw, _ := json.Marshal(b.Rates)
		res, err := tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_OWNERS VALUES(?,?,?,?)`, c.Epoch, b.Target.ID, string(raw), Digest(b.Rates))
		if err != nil {
			return err
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return ErrState
		}
		if saved, err := owner(ctx, tx, c.Epoch, b.Target); err != nil || saved != b.Rates {
			return ErrState
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_SPEED_LIMITER_REVIEWS SET STATE='used' WHERE ID=? AND STATE='open'`, p.ID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var used string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_SPEED_LIMITER_REVIEWS WHERE ID=?`, p.ID).Scan(&used); err != nil || used != "used" {
		return ErrState
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_PLAYSTATE VALUES(?,0)`, c.Epoch)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var playing int
	if err := tx.QueryRowContext(ctx, `SELECT PLAYING FROM GO_SPEED_LIMITER_PLAYSTATE WHERE EPOCH=?`, c.Epoch).Scan(&playing); err != nil || playing != 0 {
		return ErrState
	}
	final, _, active, err := readScope(ctx, tx)
	if err != nil || !active || Digest(final) != Digest(c) {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Stop(ctx context.Context, expected string, validate func() error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	basis, err := rawBasis(ctx, tx)
	if err != nil || basis != expected {
		return ErrState
	}
	raw, err := setting(ctx, tx, ControlKey)
	if err != nil {
		return err
	}
	c, err := DecodeControl(raw)
	if err != nil || c.Epoch == "" {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	c.Authorized = false
	if err := setControl(ctx, tx, c); err != nil {
		return err
	}
	if err := validate(); err != nil {
		return err
	}
	return tx.Commit()
}
func owner(ctx context.Context, r Reader, epoch string, t Target) (Rates, error) {
	var rates Rates
	var raw, fp string
	if err := r.QueryRowContext(ctx, `SELECT RATES,FP FROM GO_SPEED_LIMITER_OWNERS WHERE EPOCH=? AND TARGET_ID=?`, epoch, t.ID).Scan(&raw, &fp); err != nil {
		return rates, err
	}
	if len(raw) > 4096 || json.Unmarshal([]byte(raw), &rates) != nil || !rates.Valid(t.Kind) || fp != Digest(rates) {
		return rates, ErrState
	}
	return rates, nil
}
func (s *Store) Owner(ctx context.Context, c Control, t Target) (Rates, error) {
	return owner(ctx, s.db, c.Epoch, t)
}
func saveOwner(ctx context.Context, tx *sql.Tx, c Control, t Target, before, after Rates) error {
	current, err := owner(ctx, tx, c.Epoch, t)
	if err != nil || current != before {
		return ErrState
	}
	raw, _ := json.Marshal(after)
	res, err := tx.ExecContext(ctx, `UPDATE GO_SPEED_LIMITER_OWNERS SET RATES=?,FP=? WHERE EPOCH=? AND TARGET_ID=? AND FP=?`, string(raw), Digest(after), c.Epoch, t.ID, Digest(before))
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	saved, err := owner(ctx, tx, c.Epoch, t)
	if err != nil || saved != after {
		return ErrState
	}
	return nil
}

type Trigger struct {
	ID        string `json:"id"`
	Epoch     string `json:"-"`
	Source    string `json:"source"`
	Reference string `json:"reference"`
	Due       string `json:"due"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Created   string `json:"created"`
}

func enqueue(ctx context.Context, tx *sql.Tx, c Control, source, key, ref string, due time.Time) error {
	if err := schema(ctx, tx); err != nil {
		return err
	}
	p := Trigger{ID: Digest([]string{c.Epoch, source, key}), Epoch: c.Epoch, Source: source, Reference: ref, Due: due.UTC().Format(time.RFC3339Nano), State: "pending", Created: nowString()}
	_, err := tx.ExecContext(ctx, `INSERT INTO GO_SPEED_LIMITER_TRIGGERS VALUES(?,?,?,?,?,'pending','',?) ON CONFLICT(ID) DO NOTHING`, p.ID, p.Epoch, p.Source, p.Reference, p.Due, p.Created)
	if err != nil {
		return err
	}
	var epoch, kind, reference string
	if err := tx.QueryRowContext(ctx, `SELECT EPOCH,SOURCE,REFERENCE FROM GO_SPEED_LIMITER_TRIGGERS WHERE ID=?`, p.ID).Scan(&epoch, &kind, &reference); err != nil || epoch != p.Epoch || kind != source || reference != ref {
		return ErrState
	}
	return nil
}

// Capture records only an authorized future start/stop callback. Raw callback
// values are never retained here or treated as current playback evidence.
func Capture(ctx context.Context, tx *sql.Tx, server, id string, raw json.RawMessage) error {
	c, _, active, err := readScope(ctx, tx)
	if errors.Is(err, ErrState) {
		return nil
	}
	if err != nil {
		return err
	}
	if !active || c.Server != server {
		return nil
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil {
		return ErrState
	}
	key := "Event"
	if server == "plex" {
		key = "event"
	}
	if server == "jellyfin" {
		key = "NotificationType"
	}
	var action string
	if json.Unmarshal(data[key], &action) != nil {
		return nil
	}
	valid := action == "playback.start" || action == "playback.stop"
	if server == "jellyfin" {
		valid = action == "PlaybackStart" || action == "PlaybackStop"
	}
	if server == "plex" {
		valid = action == "media.play" || action == "media.stop"
	}
	if !valid {
		return nil
	}
	due := time.Now()
	if server == "plex" {
		due = due.Add(3 * time.Second)
	}
	if err := enqueue(ctx, tx, c, "webhook", id, "media-webhook:"+id, due); err != nil {
		return err
	}
	final, _, active, err := readScope(ctx, tx)
	if err != nil || !active || Digest(final) != Digest(c) {
		return ErrState
	}
	return nil
}
func (s *Store) Timer(ctx context.Context, c Control, spec Spec, now time.Time) error {
	if spec.Interval < 1 || spec.Interval > 86400 {
		return ErrState
	}
	created, err := time.Parse(time.RFC3339Nano, c.Created)
	if err != nil {
		return err
	}
	index := int64(now.Sub(created)/time.Second) / spec.Interval
	if index < 1 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, _, active, err := readScope(ctx, tx)
	if err != nil || !active || Digest(current) != Digest(c) {
		return ErrState
	}
	key := strconv.FormatInt(index, 10)
	if err := enqueue(ctx, tx, c, "timer", key, "timer:"+key, created.Add(time.Duration(index*spec.Interval)*time.Second)); err != nil {
		return err
	}
	final, _, active, err := readScope(ctx, tx)
	if err != nil || !active || Digest(final) != Digest(c) {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) Triggers(ctx context.Context, pending bool) ([]Trigger, error) {
	out := []Trigger{}
	yes, err := table(ctx, s.db, "GO_SPEED_LIMITER_TRIGGERS")
	if err != nil || !yes {
		return out, err
	}
	query := `SELECT ID,EPOCH,SOURCE,REFERENCE,DUE,STATE,REASON,CREATED FROM GO_SPEED_LIMITER_TRIGGERS ORDER BY CREATED DESC LIMIT 30`
	args := []any{}
	if pending {
		query = `SELECT ID,EPOCH,SOURCE,REFERENCE,DUE,STATE,REASON,CREATED FROM GO_SPEED_LIMITER_TRIGGERS WHERE STATE='pending' AND DUE<=? ORDER BY DUE,ID LIMIT 10`
		args = append(args, nowString())
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Trigger
		if err := rows.Scan(&p.ID, &p.Epoch, &p.Source, &p.Reference, &p.Due, &p.State, &p.Reason, &p.Created); err != nil {
			return nil, err
		}
		if !Hex(p.ID, 64) || !Hex(p.Epoch, 32) {
			return nil, ErrState
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) BlockTrigger(ctx context.Context, p Trigger, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE GO_SPEED_LIMITER_TRIGGERS SET STATE='blocked',REASON=? WHERE ID=? AND EPOCH=? AND STATE='pending'`, reason, p.ID, p.Epoch)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	var state, saved string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,REASON FROM GO_SPEED_LIMITER_TRIGGERS WHERE ID=?`, p.ID).Scan(&state, &saved); err != nil || state != "blocked" || saved != reason {
		return ErrState
	}
	return tx.Commit()
}
func targetConfig(ctx context.Context, r Reader, t Target) (string, error) {
	var name, kind, config string
	var enabled int
	err := r.QueryRowContext(ctx, `SELECT COALESCE(NAME,''),COALESCE(TYPE,''),COALESCE(CONFIG,''),COALESCE(ENABLED,0) FROM DOWNLOADER WHERE ID=?`, t.ID).Scan(&name, &kind, &config, &enabled)
	if err != nil || enabled == 0 || kind != t.Kind || name != t.Name || Digest([]any{t.ID, name, kind, config, enabled}) != t.Digest {
		return "", ErrState
	}
	return config, nil
}
func (s *Store) TargetConfig(ctx context.Context, t Target) (string, error) {
	return targetConfig(ctx, s.db, t)
}

type Choice struct {
	ID      string
	Name    string
	Enabled bool
}

func (s *Store) Choices(ctx context.Context) ([]Choice, error) {
	out := []Choice{}
	yes, err := table(ctx, s.db, "DOWNLOADER")
	if err != nil || !yes {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(ENABLED,0) FROM DOWNLOADER WHERE TYPE IN('qbittorrent','transmission') ORDER BY ID LIMIT 1001`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		var name string
		var enabled int
		if err := rows.Scan(&n, &name, &enabled); err != nil || n < 1 {
			return nil, ErrState
		}
		out = append(out, Choice{strconv.FormatInt(n, 10), name, enabled != 0})
	}
	if len(out) > 1000 {
		return nil, ErrState
	}
	return out, rows.Err()
}

// Keep JSON decoders consistent with configuration digests, including large
// integer extension fields that do not belong to the limiter's own settings.
func stringsReader(raw string) *bytes.Reader { return bytes.NewReader([]byte(raw)) }
