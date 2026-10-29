package movielike

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type Reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db} }
func Exists(ctx context.Context, r Reader, name string) (bool, error) {
	var b bool
	e := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&b)
	return b, e
}
func Setting(ctx context.Context, r Reader, key string) (string, error) {
	yes, e := Exists(ctx, r, "SYSTEM_DICT")
	if e != nil || !yes {
		return "", e
	}
	var s string
	e = r.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&s)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return s, e
}
func Set(ctx context.Context, tx *sql.Tx, key, value string) error {
	var n int
	if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, key).Scan(&n); e != nil {
		return e
	}
	var res sql.Result
	var err error
	if n == 0 {
		res, err = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig',?,?)`, key, value)
		n = 1
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, value, key)
	}
	if err != nil {
		return err
	}
	got, e := res.RowsAffected()
	if e != nil || got != int64(n) {
		return ErrState
	}
	actual, e := Setting(ctx, tx, key)
	if e != nil || actual != value {
		return ErrState
	}
	return nil
}
func WriteControl(ctx context.Context, tx *sql.Tx, c Control) error {
	b, _ := json.Marshal(c)
	if _, e := DecodeControl(string(b)); e != nil {
		return e
	}
	return Set(ctx, tx, ControlKey, string(b))
}
func Basis(ctx context.Context, r Reader) (string, error) {
	v := []string{}
	for _, key := range []string{"UserInstalledPlugins", ConfigKey, ControlKey} {
		s, e := Setting(ctx, r, key)
		if e != nil {
			return "", e
		}
		v = append(v, s)
	}
	return Hash(v), nil
}
func Scope(ctx context.Context, r Reader) (Control, Spec, bool, error) {
	c := Control{}
	s := Spec{}
	raw, e := Setting(ctx, r, ConfigKey)
	if e != nil {
		return c, s, false, e
	}
	s, e = ParseSpec(raw)
	if e != nil {
		return c, s, false, e
	}
	ids, e := Setting(ctx, r, "UserInstalledPlugins")
	if e != nil {
		return c, s, false, e
	}
	var installed []string
	if ids != "" && (len(ids) > 64<<10 || json.Unmarshal([]byte(ids), &installed) != nil) {
		return c, s, false, ErrState
	}
	yes := false
	for _, id := range installed {
		if id == Plugin {
			yes = true
		}
	}
	ctl, e := Setting(ctx, r, ControlKey)
	if e != nil {
		return c, s, false, e
	}
	c, e = DecodeControl(ctl)
	if e != nil {
		return c, s, false, e
	}
	return c, s, yes && s.Enabled && s.Directory != "" && c.Authorized && c.ConfigDigest == Hash(raw), nil
}
func (s *Store) Scope(ctx context.Context) (Control, Spec, bool, error) { return Scope(ctx, s.db) }
func (s *Store) Basis(ctx context.Context) (string, error)              { return Basis(ctx, s.db) }
func schema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_MOVIE_LIKE_REVIEWS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FP TEXT NOT NULL,STATE TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_MOVIE_LIKE_EVENTS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FP TEXT NOT NULL,STATE TEXT NOT NULL,PHASE TEXT NOT NULL,REASON TEXT NOT NULL,ACTOR TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,WORK TEXT NOT NULL DEFAULT '',WORK_FP TEXT NOT NULL DEFAULT '',PROOF TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS GO_MOVIE_LIKE_PENDING ON GO_MOVIE_LIKE_EVENTS(STATE,CREATED)`,
	} {
		if _, e := tx.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) SaveReview(ctx context.Context, p Review, validate func() error) error {
	if !Hex(p.ID, 32) || validate == nil {
		return ErrState
	}
	raw, _ := json.Marshal(p)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	basis, e := Basis(ctx, tx)
	if e != nil || basis != p.Basis || validate() != nil {
		return ErrState
	}
	if e := schema(ctx, tx); e != nil {
		return e
	}
	config, e := Setting(ctx, tx, ConfigKey)
	if e != nil {
		return e
	}
	spec, e := ParseSpec(config)
	if e != nil || Hash(spec) != Hash(p.Spec) || p.Control.ConfigDigest != Hash(config) || p.Control.Created != p.Created || p.Control.Authorized {
		return ErrState
	}
	probe := p.Control
	probe.Authorized = true
	encoded, _ := json.Marshal(probe)
	if _, e := DecodeControl(string(encoded)); e != nil {
		return e
	}
	result, e := tx.ExecContext(ctx, `INSERT INTO GO_MOVIE_LIKE_REVIEWS VALUES(?,?,?,'open')`, p.ID, string(raw), Hash(p))
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return ErrState
	}
	got, state, e := readReview(ctx, tx, p.ID)
	if e != nil || state != "open" || Hash(got) != Hash(p) || validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
func readReview(ctx context.Context, r Reader, id string) (Review, string, error) {
	p := Review{}
	if !Hex(id, 32) {
		return p, "", ErrMissing
	}
	var raw, fp, state string
	e := r.QueryRowContext(ctx, `SELECT DOCUMENT,FP,STATE FROM GO_MOVIE_LIKE_REVIEWS WHERE ID=?`, id).Scan(&raw, &fp, &state)
	if e != nil {
		return p, state, e
	}
	if len(raw) > 256<<10 || json.Unmarshal([]byte(raw), &p) != nil || Hash(p) != fp || p.ID != id {
		return p, state, ErrState
	}
	return p, state, nil
}
func (s *Store) Review(ctx context.Context, id string) (Review, error) {
	p, state, e := readReview(ctx, s.db, id)
	if e == nil && state != "open" {
		e = ErrState
	}
	return p, e
}
func (s *Store) Activate(ctx context.Context, p Review, validate func() error) error {
	if validate == nil {
		return ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	got, state, e := readReview(ctx, tx, p.ID)
	if e != nil || state != "open" || Hash(got) != Hash(p) {
		return ErrState
	}
	at, e := time.Parse(time.RFC3339Nano, p.Created)
	if e != nil || time.Since(at) < 0 || time.Since(at) > 10*time.Minute {
		return ErrState
	}
	basis, e := Basis(ctx, tx)
	if e != nil || basis != p.Basis || validate() != nil {
		return ErrState
	}
	c := p.Control
	c.Authorized = true
	if e := WriteControl(ctx, tx, c); e != nil {
		return e
	}
	res, e := tx.ExecContext(ctx, `UPDATE GO_MOVIE_LIKE_REVIEWS SET STATE='used' WHERE ID=? AND STATE='open'`, p.ID)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return ErrState
	}
	_, state, e = readReview(ctx, tx, p.ID)
	if e != nil || state != "used" {
		return ErrState
	}
	current, _, on, e := Scope(ctx, tx)
	if e != nil || !on || Hash(current) != Hash(c) || validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) Stop(ctx context.Context, basis string, validate func() error) error {
	if validate == nil {
		return ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	b, e := Basis(ctx, tx)
	if e != nil || b != basis || validate() != nil {
		return ErrState
	}
	raw, e := Setting(ctx, tx, ControlKey)
	if e != nil {
		return e
	}
	c, e := DecodeControl(raw)
	if e != nil || c.Epoch == "" {
		return ErrState
	}
	c.Authorized = false
	if e := WriteControl(ctx, tx, c); e != nil {
		return e
	}
	if validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
func Capture(ctx context.Context, tx *sql.Tx, server, callback string, raw json.RawMessage) error {
	if server != "emby" {
		return nil
	}
	c, s, on, err := Scope(ctx, tx)
	if errors.Is(err, ErrState) {
		return nil
	}
	if err != nil || !on {
		return err
	}
	e, handles, parseErr := Parse(raw)
	if !handles {
		return nil
	}
	state, reason := "pending", ""
	if parseErr != nil {
		e = Event{}
		state, reason = "blocked", "原电影评分回调路径或字段无效，没有移动目录"
	}
	if !Hex(callback, 32) {
		return ErrState
	}
	if err := schema(ctx, tx); err != nil {
		return err
	}
	d := Definition{c, s, callback, e}
	doc, _ := json.Marshal(d)
	id := Hash([]string{c.Epoch, callback})
	at := Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_MOVIE_LIKE_EVENTS(ID,DOCUMENT,FP,STATE,PHASE,REASON,ACTOR,CREATED,UPDATED)VALUES(?,?,?,?,'',?,'',?,?)`, id, string(doc), Hash(d), state, reason, at, at)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return ErrState
	}
	p, err := Read(ctx, tx, id)
	if err != nil || p.State != state || p.Fingerprint != Hash(d) {
		return ErrState
	}
	current, _, active, err := Scope(ctx, tx)
	if err != nil || !active || Hash(current) != Hash(c) {
		return ErrState
	}
	return nil
}
func Read(ctx context.Context, r Reader, id string) (Receipt, error) {
	p := Receipt{}
	if !Hex(id, 64) {
		return p, ErrMissing
	}
	yes, e := Exists(ctx, r, "GO_MOVIE_LIKE_EVENTS")
	if e != nil {
		return p, e
	}
	if !yes {
		return p, ErrMissing
	}
	var raw, work, proof string
	e = r.QueryRowContext(ctx, `SELECT ID,DOCUMENT,FP,STATE,PHASE,REASON,ACTOR,CREATED,UPDATED,WORK,WORK_FP,PROOF FROM GO_MOVIE_LIKE_EVENTS WHERE ID=?`, id).Scan(&p.ID, &raw, &p.Fingerprint, &p.State, &p.Phase, &p.Reason, &p.Actor, &p.Created, &p.Updated, &work, &p.WorkFingerprint, &proof)
	if errors.Is(e, sql.ErrNoRows) {
		return p, ErrMissing
	}
	if e != nil {
		return p, e
	}
	if len(raw) > 256<<10 || json.Unmarshal([]byte(raw), &p.Definition) != nil || p.Fingerprint != Hash(p.Definition) || p.ID != Hash([]string{p.Definition.Control.Epoch, p.Definition.Callback}) {
		return p, ErrState
	}
	ctl, _ := json.Marshal(p.Definition.Control)
	if _, e := DecodeControl(string(ctl)); e != nil || !p.Definition.Control.Authorized {
		return p, ErrState
	}
	switch p.State {
	case "pending", "planned", "running", "needs_review", "completed", "blocked", "ignored":
	default:
		return p, ErrState
	}
	if work != "" {
		if len(work) > 64<<20 || !json.Valid([]byte(work)) || Hash(json.RawMessage(work)) != p.WorkFingerprint {
			return p, ErrState
		}
		p.Work = json.RawMessage(work)
	} else if p.WorkFingerprint != "" || p.Phase != "" || proof != "" || p.State == "planned" || p.State == "running" || p.State == "completed" {
		return p, ErrState
	}
	if proof != "" {
		if len(proof) > 32<<20 || !json.Valid([]byte(proof)) {
			return p, ErrState
		}
		p.Proof = json.RawMessage(proof)
	}
	p.Event, p.Callback = p.Definition.Event, p.Definition.Callback
	return p, nil
}
func (s *Store) Receipt(ctx context.Context, id string) (Receipt, error) { return Read(ctx, s.db, id) }
func (s *Store) List(ctx context.Context, page int, pending bool) ([]Receipt, error) {
	out := []Receipt{}
	if page < 1 || page > 10000 {
		return out, ErrLimit
	}
	yes, e := Exists(ctx, s.db, "GO_MOVIE_LIKE_EVENTS")
	if e != nil || !yes {
		return out, e
	}
	query := `SELECT ID FROM GO_MOVIE_LIKE_EVENTS ORDER BY CREATED DESC,ID DESC LIMIT 30 OFFSET ?`
	args := []any{(page - 1) * 30}
	if pending {
		query = `SELECT ID FROM GO_MOVIE_LIKE_EVENTS WHERE STATE='pending' ORDER BY CREATED,ID LIMIT 10`
		args = nil
	}
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e := rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for _, id := range ids {
		p, e := s.Receipt(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, p)
	}
	return out, nil
}
func CheckAutomatic(ctx context.Context, r Reader, p Receipt) error {
	c, _, on, e := Scope(ctx, r)
	if e != nil || !on || Hash(c) != Hash(p.Definition.Control) {
		return ErrState
	}
	return nil
}
func Advance(ctx context.Context, tx *sql.Tx, p Receipt, state, phase, reason, actor string, work, proof json.RawMessage) error {
	if len(work) > 64<<20 || len(proof) > 32<<20 {
		return ErrLimit
	}
	if work == nil {
		work = p.Work
	}
	if proof == nil {
		proof = p.Proof
	}
	fp := ""
	if len(work) > 0 {
		if !json.Valid(work) {
			return ErrState
		}
		fp = Hash(work)
	}
	if len(proof) > 0 && !json.Valid(proof) {
		return ErrState
	}
	at := Now()
	res, e := tx.ExecContext(ctx, `UPDATE GO_MOVIE_LIKE_EVENTS SET STATE=?,PHASE=?,REASON=?,ACTOR=?,UPDATED=?,WORK=?,WORK_FP=?,PROOF=? WHERE ID=? AND STATE=? AND UPDATED=? AND WORK_FP=?`, state, phase, reason, actor, at, string(work), fp, string(proof), p.ID, p.State, p.Updated, p.WorkFingerprint)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return ErrState
	}
	got, e := Read(ctx, tx, p.ID)
	if e != nil || got.State != state || got.Phase != phase || got.Reason != reason || got.Actor != actor || got.Updated != at || got.WorkFingerprint != fp || string(got.Proof) != string(proof) {
		return ErrState
	}
	return nil
}
func (s *Store) Resolve(ctx context.Context, p Receipt, state, reason string) error {
	if p.State != "pending" || (state != "ignored" && state != "blocked") {
		return ErrState
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e := Advance(ctx, tx, p, state, "", reason, "", nil, nil); e != nil {
		return e
	}
	return tx.Commit()
}

// Every filesystem phase participates in the caller's real transaction.
func (s *Store) Transaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e := fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}
