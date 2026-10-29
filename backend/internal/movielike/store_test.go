package movielike

import (
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type favoriteFixture struct {
	db *sql.DB
	s  *Store
	p  Review
}

func fixture(t *testing.T) *favoriteFixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT);
 INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','UserInstalledPlugins','["MovieLike"]'),('SystemConfig','plugin.MovieLike','{"enable":true,"dir_name":"精选","unknown":{"n":9007199254740993}}');`); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	basis, _ := s.Basis(t.Context())
	raw, _ := Setting(t.Context(), db, ConfigKey)
	spec, _ := ParseSpec(raw)
	at := Now()
	c := Control{Epoch: strings.Repeat("a", 32), Actor: "admin", Authority: strings.Repeat("b", 64), AppDigest: strings.Repeat("c", 64), CategoryDigest: strings.Repeat("d", 64), ConfigDigest: Hash(raw), MediaID: "original-server", Created: at, Roots: []Root{{ID: "movie-root", Path: "/library", Identity: "original-root", ConfiguredPath: "/library"}}, Categories: []string{"Films", "Movies"}}
	return &favoriteFixture{db, s, Review{ID: strings.Repeat("e", 32), Basis: basis, Created: at, Control: c, Spec: spec}}
}
func allow() error { return nil }
func (f *favoriteFixture) activate(t *testing.T) {
	t.Helper()
	if err := f.s.SaveReview(t.Context(), f.p, allow); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Activate(t.Context(), f.p, allow); err != nil {
		t.Fatal(err)
	}
}
func (f *favoriteFixture) capture(t *testing.T, id, server string) error {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Capture(t.Context(), tx, server, id, json.RawMessage(`{"Event":"item.rate","Item":{"Type":"Movie","Name":"Original","Path":"/emby/Films/Movie/movie.mkv"}}`)); err != nil {
		return err
	}
	return tx.Commit()
}
func TestDefaultOffFutureOnlyAndActualTransaction(t *testing.T) {
	f := fixture(t)
	if err := f.capture(t, strings.Repeat("1", 32), "emby"); err != nil {
		t.Fatal(err)
	}
	var count int
	f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_MOVIE_LIKE_%'`).Scan(&count)
	if count != 0 {
		t.Fatal("default-off created callback work")
	}
	f.activate(t)
	if err := f.capture(t, strings.Repeat("2", 32), "jellyfin"); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.List(t.Context(), 1, false)
	if err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	tx, _ := f.db.BeginTx(t.Context(), nil)
	if err := Capture(t.Context(), tx, "emby", strings.Repeat("3", 32), json.RawMessage(`{"Event":"item.rate","Item":{"Type":"Movie","Path":"/emby/Films/Movie"}}`)); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	rows, _ = f.s.List(t.Context(), 1, false)
	if len(rows) != 0 {
		t.Fatal("rollback left a callback")
	}
	if err := f.capture(t, strings.Repeat("4", 32), "emby"); err != nil {
		t.Fatal(err)
	}
	rows, err = f.s.List(t.Context(), 1, true)
	if err != nil || len(rows) != 1 || rows[0].State != "pending" {
		t.Fatal(rows, err)
	}
	if _, err := New(f.db).Receipt(t.Context(), rows[0].ID); err != nil {
		t.Fatal(err)
	}
}
func TestReviewExpiryOneUseAndExactOriginalConfiguration(t *testing.T) {
	for _, change := range []string{"expired", "future", "spec", "config"} {
		t.Run(change, func(t *testing.T) {
			f := fixture(t)
			switch change {
			case "expired", "future":
				at := time.Now().Add(-11 * time.Minute)
				if change == "future" {
					at = time.Now().Add(time.Minute)
				}
				f.p.Created = at.UTC().Format(time.RFC3339Nano)
				f.p.Control.Created = f.p.Created
			case "spec":
				f.p.Spec.Directory = "Other"
			}
			err := f.s.SaveReview(t.Context(), f.p, allow)
			if change == "spec" {
				if err == nil {
					t.Fatal("mismatched private review accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if change == "config" {
				if _, err := f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='{"enable":true,"dir_name":"Other"}' WHERE KEY='plugin.MovieLike'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.s.Activate(t.Context(), f.p, allow); err == nil {
				t.Fatal("stale review activated")
			}
		})
	}
	f := fixture(t)
	f.activate(t)
	if err := f.s.Activate(t.Context(), f.p, allow); err == nil {
		t.Fatal("used review replayed")
	}
}
func TestSilentWritesAndDamagedDocumentsNeverAcknowledgeSuccess(t *testing.T) {
	for _, table := range []string{"review", "control", "event"} {
		t.Run(table, func(t *testing.T) {
			f := fixture(t)
			if table == "event" {
				f.activate(t)
			}
			var query string
			switch table {
			case "review":
				if err := f.s.SaveReview(t.Context(), f.p, allow); err != nil {
					t.Fatal(err)
				}
				f.p.ID = strings.Repeat("f", 32)
				query = `CREATE TRIGGER ignore_review BEFORE INSERT ON GO_MOVIE_LIKE_REVIEWS BEGIN SELECT RAISE(IGNORE);END`
			case "control":
				if err := f.s.SaveReview(t.Context(), f.p, allow); err != nil {
					t.Fatal(err)
				}
				query = `CREATE TRIGGER ignore_control BEFORE INSERT ON SYSTEM_DICT WHEN NEW.KEY='GoMovieLike' BEGIN SELECT RAISE(IGNORE);END`
			case "event":
				query = `CREATE TRIGGER ignore_event BEFORE INSERT ON GO_MOVIE_LIKE_EVENTS BEGIN SELECT RAISE(IGNORE);END`
			}
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			var err error
			switch table {
			case "review":
				err = f.s.SaveReview(t.Context(), f.p, allow)
			case "control":
				err = f.s.Activate(t.Context(), f.p, allow)
			case "event":
				err = f.capture(t, strings.Repeat("1", 32), "emby")
			}
			if err == nil {
				t.Fatal("silent write became success")
			}
		})
	}
	f := fixture(t)
	f.activate(t)
	if err := f.capture(t, strings.Repeat("2", 32), "emby"); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.s.List(t.Context(), 1, true)
	if _, err := f.db.Exec(`UPDATE GO_MOVIE_LIKE_EVENTS SET DOCUMENT='{}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Receipt(t.Context(), rows[0].ID); err == nil {
		t.Fatal("damaged original was adopted")
	}
}
func TestStopPreservesUnknownConfigAndExistingFacts(t *testing.T) {
	f := fixture(t)
	f.activate(t)
	if err := f.capture(t, strings.Repeat("3", 32), "emby"); err != nil {
		t.Fatal(err)
	}
	basis, _ := f.s.Basis(t.Context())
	if err := f.s.Stop(t.Context(), basis, allow); err != nil {
		t.Fatal(err)
	}
	_, _, on, err := f.s.Scope(t.Context())
	if err != nil || on {
		t.Fatal(on, err)
	}
	raw, _ := Setting(t.Context(), f.db, ConfigKey)
	if !strings.Contains(raw, "9007199254740993") {
		t.Fatal(raw)
	}
	if err := f.capture(t, strings.Repeat("4", 32), "emby"); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.s.List(t.Context(), 1, false)
	if len(rows) != 1 || rows[0].State != "pending" {
		t.Fatal(rows)
	}
	if err := f.s.Stop(t.Context(), basis, allow); err == nil {
		t.Fatal("old stop fingerprint reused")
	}
}
