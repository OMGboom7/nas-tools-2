package mediasyncdel

import (
	"database/sql"
	"encoding/json"
	_ "modernc.org/sqlite"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	db *sql.DB
	s  *Store
	p  Review
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT);
 INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','UserInstalledPlugins','["MediaSyncDel"]'),('SystemConfig','plugin.MediaSyncDel','{"enable":true,"del_source":false,"send_notify":true,"unknown":{"n":9007199254740993}}');
 CREATE TABLE TRANSFER_HISTORY(ID INTEGER PRIMARY KEY,TYPE TEXT,TMDBID INTEGER,SEASON_EPISODE TEXT,DEST_FILENAME TEXT);
 INSERT INTO TRANSFER_HISTORY VALUES(1,'电影',100,'','movie.mkv'),(2,'电影',100,'','other-version.mkv'),(3,'电视剧',100,'S01 E01','movie.mkv'),(4,'电视剧',100,'S02 E03','episode.mkv'),(5,'电视剧',100,'S020 E01','episode.mkv'),(6,'动漫',100,'S02 E04','anime.mkv');`); err != nil {
		t.Fatal(err)
	}
	s := New(db)
	basis, _ := s.Basis(t.Context())
	raw, _ := Setting(t.Context(), db, ConfigKey)
	spec, _ := DecodeSpec(raw)
	c := Control{Epoch: strings.Repeat("a", 32), Actor: "admin", Authority: strings.Repeat("b", 64), AppDigest: strings.Repeat("c", 64), ConfigDigest: ConfigDigest(raw), MediaID: "original-media", Created: now(), Roots: []Root{}}
	p := Review{ID: strings.Repeat("d", 32), Basis: basis, Control: c, Spec: spec, Created: now()}
	return &fixture{db, s, p}
}
func allow() error { return nil }
func (f *fixture) activate(t *testing.T) {
	t.Helper()
	if err := f.s.SaveReview(t.Context(), f.p, allow); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Activate(t.Context(), f.p, allow); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) capture(t *testing.T, id, raw string) error {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Capture(t.Context(), tx, "emby", id, json.RawMessage(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

const movieCallback = `{"event_type":"media_del","item_isvirtual":"False","media_type":"Movie","media_name":"Movie","tmdb_id":"100","media_path":"/emby/movie.mkv"}`

func TestDefaultOffAndFutureReceiptAreAtomicAndNormalized(t *testing.T) {
	f := newFixture(t)
	if err := f.capture(t, strings.Repeat("1", 32), movieCallback); err != nil {
		t.Fatal(err)
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_MEDIA_SYNC_DEL_%'`).Scan(&n)
	if n != 0 {
		t.Fatal("default off wrote private tables")
	}
	f.activate(t)
	if err := f.capture(t, strings.Repeat("2", 32), movieCallback); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.List(t.Context(), 1, true)
	if err != nil || len(rows) != 1 || rows[0].State != "pending" || rows[0].Callback != strings.Repeat("2", 32) {
		t.Fatal(rows, err)
	}
	tx, _ := f.db.BeginTx(t.Context(), nil)
	if err := Capture(t.Context(), tx, "emby", strings.Repeat("3", 32), json.RawMessage(movieCallback)); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	rows, _ = f.s.List(t.Context(), 1, false)
	if len(rows) != 1 {
		t.Fatal("callback rollback retained row")
	}
	raw, _ := json.Marshal(rows[0])
	for _, private := range []string{"authority", "appDigest", "configDigest", "definition", "MediaID"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private grant escaped", string(raw))
		}
	}
}
func TestVirtualAndMissingVirtualDisablePreservesUnknownConfig(t *testing.T) {
	f := newFixture(t)
	f.activate(t)
	if err := f.capture(t, strings.Repeat("1", 32), `{"event_type":"media_del","item_isvirtual":"True"}`); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.s.List(t.Context(), 1, false)
	if len(rows) != 1 || rows[0].State != "ignored" {
		t.Fatal(rows)
	}
	if err := f.capture(t, strings.Repeat("2", 32), `{"event_type":"media_del","media_type":"Movie"}`); err != nil {
		t.Fatal(err)
	}
	_, spec, on, err := f.s.Scope(t.Context())
	if err != nil || on || spec.Enabled {
		t.Fatal(on, err)
	}
	raw, _ := Setting(t.Context(), f.db, ConfigKey)
	if !strings.Contains(raw, `"enable":false`) || !strings.Contains(raw, "9007199254740993") {
		t.Fatal(raw)
	}
	rows, _ = f.s.List(t.Context(), 1, false)
	if len(rows) != 2 || rows[0].State != "blocked" || !rows[0].Event.MissingVirtual {
		t.Fatal(rows)
	}
}
func TestMissingVirtualAndReviewSilentSQLFailuresRollBack(t *testing.T) {
	f := newFixture(t)
	f.activate(t)
	if _, err := f.db.Exec(`CREATE TRIGGER ignore_disable BEFORE UPDATE ON SYSTEM_DICT WHEN NEW.KEY='plugin.MediaSyncDel' BEGIN SELECT RAISE(IGNORE);END`); err != nil {
		t.Fatal(err)
	}
	if err := f.capture(t, strings.Repeat("1", 32), `{"event_type":"media_del"}`); err == nil {
		t.Fatal("unconfirmed disable accepted")
	}
	rows, _ := f.s.List(t.Context(), 1, false)
	if len(rows) != 0 {
		t.Fatal("partial callback accepted", rows)
	}
	_, _, on, _ := f.s.Scope(t.Context())
	if !on {
		t.Fatal("rollback changed original grant")
	}
	for _, query := range []string{`CREATE TRIGGER ignore_review BEFORE UPDATE ON GO_MEDIA_SYNC_DEL_REVIEWS BEGIN SELECT RAISE(IGNORE);END`, `CREATE TRIGGER ignore_control BEFORE INSERT ON SYSTEM_DICT WHEN NEW.KEY='GoMediaSyncDel' BEGIN SELECT RAISE(IGNORE);END`} {
		f := newFixture(t)
		if err := f.s.SaveReview(t.Context(), f.p, allow); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Activate(t.Context(), f.p, allow); err == nil {
			t.Fatal("silent write enabled scope", query)
		}
	}
}
func TestCandidateSelectionUsesCompleteTypeVersionAndExactSeason(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		e    Event
		want []int64
	}{
		{Event{Type: "Movie", TMDB: "100", Path: "/emby/movie.mkv"}, []int64{1}},
		{Event{Type: "Series", TMDB: "100", Path: "/emby/Show"}, []int64{3, 4, 5, 6}},
		{Event{Type: "Season", TMDB: "100", Season: 2, Path: "/emby/Show/Season2"}, []int64{4, 6}},
		{Event{Type: "Episode", TMDB: "100", Season: 2, Episode: 3, Path: "/emby/episode.mkv"}, []int64{4}},
	} {
		got, err := f.s.CandidateIDs(t.Context(), tc.e)
		if err != nil || !SameIDs(got, tc.want) {
			t.Fatal(tc.e, got, err)
		}
	}
	for i := int64(100); i < 251; i++ {
		if _, err := f.db.Exec(`INSERT INTO TRANSFER_HISTORY VALUES(?,'电视剧',200,'S01 E01','episode.mkv')`, i); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.s.CandidateIDs(t.Context(), Event{Type: "Series", TMDB: "200"})
	if err != nil || len(rows) != 151 {
		t.Fatal("truncated to manual history limit", len(rows), err)
	}
}
func TestReviewExpiryScopeChangesAndIgnoredOriginalStop(t *testing.T) {
	for _, age := range []time.Duration{-time.Minute, 11 * time.Minute} {
		f := newFixture(t)
		f.p.Created = time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
		if err := f.s.SaveReview(t.Context(), f.p, allow); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Activate(t.Context(), f.p, allow); err == nil {
			t.Fatal("future or expired grant")
		}
	}
	f := newFixture(t)
	f.activate(t)
	if err := f.capture(t, strings.Repeat("1", 32), movieCallback); err != nil {
		t.Fatal(err)
	}
	rows, _ := f.s.List(t.Context(), 1, false)
	if err := f.s.Ignore(t.Context(), rows[0].ID, "admin", "", allow); err == nil {
		t.Fatal("empty ignore")
	}
	if err := f.s.Ignore(t.Context(), rows[0].ID, "admin", "reviewed synthetic callback", allow); err != nil {
		t.Fatal(err)
	}
	current, err := f.s.Receipt(t.Context(), rows[0].ID)
	if err != nil || current.State != "ignored" || current.Actor != "admin" {
		t.Fatal(current, err)
	}
	_, _, on, _ := f.s.Scope(t.Context())
	if on {
		t.Fatal("ignore did not stop future actions")
	}
}
