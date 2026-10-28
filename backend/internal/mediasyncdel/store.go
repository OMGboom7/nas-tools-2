package mediasyncdel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"time"
)

type Reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type Store struct{ db *sql.DB }

func New(db *sql.DB) *Store { return &Store{db} }
func exists(ctx context.Context, r Reader, name string) (bool, error) {
	var ok bool
	err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name).Scan(&ok)
	return ok, err
}
func Setting(ctx context.Context, r Reader, key string) (string, error) {
	yes, err := exists(ctx, r, "SYSTEM_DICT")
	if err != nil || !yes {
		return "", err
	}
	var v string
	err = r.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
func basis(ctx context.Context, r Reader) (string, error) {
	v := map[string]string{}
	for _, k := range []string{"UserInstalledPlugins", ConfigKey, ControlKey} {
		raw, err := Setting(ctx, r, k)
		if err != nil {
			return "", err
		}
		v[k] = raw
	}
	return Hash(v), nil
}
func (s *Store) Basis(ctx context.Context) (string, error) { return basis(ctx, s.db) }
func Scope(ctx context.Context, r Reader) (Control, Spec, bool, error) {
	raw, err := Setting(ctx, r, ControlKey)
	if err != nil {
		return Control{}, Spec{}, false, err
	}
	c, err := DecodeControl(raw)
	if err != nil || !c.Authorized {
		return c, Spec{}, false, err
	}
	raw, err = Setting(ctx, r, "UserInstalledPlugins")
	if err != nil {
		return c, Spec{}, false, err
	}
	var ids []string
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &ids) != nil {
		return c, Spec{}, false, ErrState
	}
	yes := false
	for _, id := range ids {
		yes = yes || id == Plugin
	}
	if !yes {
		return c, Spec{}, false, ErrState
	}
	raw, err = Setting(ctx, r, ConfigKey)
	if err != nil {
		return c, Spec{}, false, err
	}
	spec, err := DecodeSpec(raw)
	if err != nil || !spec.Enabled || ConfigDigest(raw) != c.ConfigDigest || spec.DeleteSource && len(c.Roots) == 0 {
		return c, spec, false, ErrState
	}
	return c, spec, true, nil
}
func (s *Store) Scope(ctx context.Context) (Control, Spec, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Control{}, Spec{}, false, err
	}
	defer tx.Rollback()
	c, spec, on, err := Scope(ctx, tx)
	if err != nil {
		return c, spec, false, err
	}
	return c, spec, on, tx.Commit()
}
func setSetting(ctx context.Context, tx *sql.Tx, key, value string) error {
	var old int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, key).Scan(&old); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE SYSTEM_DICT SET VALUE=? WHERE TYPE='SystemConfig' AND KEY=?`, value, key)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != old {
		return ErrState
	}
	if old == 0 {
		res, err = tx.ExecContext(ctx, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE,NOTE) VALUES('SystemConfig',?,?,'')`, key, value)
		if err != nil {
			return err
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return ErrState
		}
	}
	var count, matches int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(VALUE=?),0) FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=?`, value, key).Scan(&count, &matches); err != nil || count == 0 || matches != count {
		return ErrState
	}
	return nil
}
func WriteControl(ctx context.Context, tx *sql.Tx, c Control) error {
	raw, _ := json.Marshal(c)
	if _, err := DecodeControl(string(raw)); err != nil {
		return err
	}
	return setSetting(ctx, tx, ControlKey, string(raw))
}
func schema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_MEDIA_SYNC_DEL_REVIEWS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FP TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_MEDIA_SYNC_DEL_EVENTS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FP TEXT NOT NULL,STATE TEXT NOT NULL,PARENT TEXT NOT NULL,PARENT_FP TEXT NOT NULL,LINK_FP TEXT NOT NULL,COUNT INTEGER NOT NULL,IMAGE TEXT NOT NULL,REASON TEXT NOT NULL,ACTOR TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS GO_MEDIA_SYNC_DEL_PENDING ON GO_MEDIA_SYNC_DEL_EVENTS(STATE,CREATED,ID)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

type Review struct {
	ID      string
	Basis   string
	Control Control
	Spec    Spec
	Created string
}

func (s *Store) SaveReview(ctx context.Context, p Review, validate func() error) error {
	raw, _ := json.Marshal(p)
	if !Hex(p.ID, 32) || !Hex(p.Basis, 64) || p.Control.Authorized || !p.Spec.Enabled || validate == nil {
		return ErrState
	}
	cRaw, _ := json.Marshal(p.Control)
	if _, err := DecodeControl(string(cRaw)); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := basis(ctx, tx)
	if err != nil || b != p.Basis || validate() != nil {
		return ErrState
	}
	if err := schema(ctx, tx); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_MEDIA_SYNC_DEL_REVIEWS VALUES(?,?,?,'open',?)`, p.ID, string(raw), Hash(p), p.Created)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	saved, _, err := readReview(ctx, tx, p.ID)
	if err != nil || Hash(saved) != Hash(p) {
		return ErrState
	}
	b, err = basis(ctx, tx)
	if err != nil || b != p.Basis || validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
func readReview(ctx context.Context, r Reader, id string) (Review, string, error) {
	p := Review{}
	if !Hex(id, 32) {
		return p, "", ErrMissing
	}
	yes, err := exists(ctx, r, "GO_MEDIA_SYNC_DEL_REVIEWS")
	if err != nil {
		return p, "", err
	}
	if !yes {
		return p, "", ErrMissing
	}
	var raw, fp, state string
	err = r.QueryRowContext(ctx, `SELECT DOCUMENT,FP,STATE FROM GO_MEDIA_SYNC_DEL_REVIEWS WHERE ID=?`, id).Scan(&raw, &fp, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return p, "", ErrMissing
	}
	if err != nil {
		return p, "", err
	}
	if len(raw) > 256<<10 || json.Unmarshal([]byte(raw), &p) != nil || p.ID != id || Hash(p) != fp || !Hex(p.Basis, 64) {
		return p, state, ErrState
	}
	cRaw, _ := json.Marshal(p.Control)
	if _, err := DecodeControl(string(cRaw)); err != nil {
		return p, state, err
	}
	return p, state, nil
}
func (s *Store) Review(ctx context.Context, id string) (Review, error) {
	p, state, err := readReview(ctx, s.db, id)
	if err == nil && state != "open" {
		err = ErrState
	}
	return p, err
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
	original, state, err := readReview(ctx, tx, p.ID)
	if err != nil || state != "open" || Hash(original) != Hash(p) {
		return ErrState
	}
	created, err := time.Parse(time.RFC3339Nano, p.Created)
	if err != nil || time.Since(created) < 0 || time.Since(created) > 10*time.Minute {
		return ErrState
	}
	b, err := basis(ctx, tx)
	if err != nil || b != p.Basis || validate() != nil {
		return ErrState
	}
	c := p.Control
	c.Authorized = true
	if err := WriteControl(ctx, tx, c); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_MEDIA_SYNC_DEL_REVIEWS SET STATE='used' WHERE ID=? AND STATE='open'`, p.ID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return ErrState
	}
	_, state, err = readReview(ctx, tx, p.ID)
	if err != nil || state != "used" {
		return ErrState
	}
	final, _, on, err := Scope(ctx, tx)
	if err != nil || !on || Hash(final) != Hash(c) || validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) Stop(ctx context.Context, expected string, validate func() error) error {
	if validate == nil {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := basis(ctx, tx)
	if err != nil || b != expected || validate() != nil {
		return ErrState
	}
	raw, err := Setting(ctx, tx, ControlKey)
	if err != nil {
		return err
	}
	c, err := DecodeControl(raw)
	if err != nil || c.Epoch == "" {
		return ErrState
	}
	c.Authorized = false
	if err := WriteControl(ctx, tx, c); err != nil {
		return err
	}
	if validate() != nil {
		return ErrState
	}
	return tx.Commit()
}
func CandidateIDs(ctx context.Context, r Reader, e Event) ([]int64, error) {
	if e.Type != "Movie" && e.Type != "Series" && e.Type != "Season" && e.Type != "Episode" {
		return nil, ErrState
	}
	out := []int64{}
	yes, err := exists(ctx, r, "TRANSFER_HISTORY")
	if err != nil || !yes {
		return out, err
	}
	n, err := strconv.ParseInt(e.TMDB, 10, 64)
	if err != nil || n < 1 {
		return nil, ErrState
	}
	types := []string{"电视剧", "TV", "tv", "Series", "动漫", "ANIME", "Anime"}
	if e.Type == "Movie" {
		types = []string{"电影", "MOV", "movie", "Movie", "电影", "MOV", "movie"}
	}
	rows, err := r.QueryContext(ctx, `SELECT ID,COALESCE(SEASON_EPISODE,''),COALESCE(DEST_FILENAME,'') FROM TRANSFER_HISTORY WHERE TMDBID=? AND TYPE IN(?,?,?,?,?,?,?) ORDER BY ID`, n, types[0], types[1], types[2], types[3], types[4], types[5], types[6])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var episode, filename string
		if err := rows.Scan(&id, &episode, &filename); err != nil || id < 1 {
			return nil, ErrState
		}
		if e.Type == "Season" && !SeasonMatches(episode, e.Season) || e.Type == "Episode" && episode != EpisodeKey(e) {
			continue
		}
		if (e.Type == "Movie" || e.Type == "Episode") && filename != path.Base(e.Path) {
			continue
		}
		out = append(out, id)
		if len(out) > HistoryLimit {
			return nil, ErrLimit
		}
	}
	return out, rows.Err()
}
func (s *Store) CandidateIDs(ctx context.Context, e Event) ([]int64, error) {
	return CandidateIDs(ctx, s.db, e)
}
func SameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
