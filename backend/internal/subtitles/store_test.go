package subtitles

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func subtitleStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	for _, q := range []string{`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT)`, `INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','UserInstalledPlugins','["ChineseSubFinder"]'),('SystemConfig','plugin.ChineseSubFinder','{"host":"http://csf.local","api_key":"private-key"}')`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return New(db), db
}
func emitSubtitle(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Emit(t.Context(), tx, "job-one", 0, "Movie", "/media/Movie.mkv", "电影", "origin-digest"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSubtitleEventScopePersistentIntentAndNoReplay(t *testing.T) {
	s, db := subtitleStore(t)
	rows, err := s.List(t.Context(), false)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='GO_SUBTITLE_JOBS'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("readonly created queue")
	}
	emitSubtitle(t, s)
	emitSubtitle(t, s)
	rows, err = s.List(t.Context(), true)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	expected := rows[0]
	bad := expected
	bad.LocalPath = "/other/file.mkv"
	if _, _, err := s.Claim(t.Context(), bad); !errors.Is(err, ErrState) {
		t.Fatal("hidden source scope not compared", err)
	}
	c, claimed, err := s.Claim(t.Context(), expected)
	if err != nil || c.Key != "private-key" || claimed.Payload.Path != "/media/Movie.mkv" {
		t.Fatal(err)
	}
	if rows, err := New(db).List(t.Context(), true); err != nil || len(rows) != 0 {
		t.Fatal("sending intent replayed after restart", rows, err)
	}
	if err := s.Finish(t.Context(), claimed.ID, "uncertain", ""); err != nil {
		t.Fatal(err)
	}
	emitSubtitle(t, s)
	if rows, err := s.List(t.Context(), true); err != nil || len(rows) != 0 {
		t.Fatal("uncertain event replayed", rows, err)
	}
}

func TestSubtitleConfigChangesBlockOldRecipientAndInvalidConfigDoesNotFailIntake(t *testing.T) {
	s, db := subtitleStore(t)
	emitSubtitle(t, s)
	rows, _ := s.List(t.Context(), true)
	if _, err := db.Exec(`UPDATE SYSTEM_DICT SET VALUE='new-epoch' WHERE KEY='GoSubtitlePluginEpoch'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','GoSubtitlePluginEpoch','new-epoch')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Claim(t.Context(), rows[0]); !errors.Is(err, ErrState) {
		t.Fatal("retargeted old event", err)
	}
	if rows, err := s.List(t.Context(), true); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	if _, err := db.Exec(`UPDATE SYSTEM_DICT SET VALUE='invalid-private-config' WHERE KEY='plugin.ChineseSubFinder'`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Emit(t.Context(), tx, "job-two", 0, "Movie", "/media/Other.mkv", "电影", "origin-two"); err != nil {
		t.Fatal("bad plugin failed media intake", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err = s.List(t.Context(), false)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, r := range rows {
		if r.State != "blocked" {
			t.Fatal(r)
		}
	}
}
