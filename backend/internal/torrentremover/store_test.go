package torrentremover

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fixture struct {
	s                           *Store
	control                     Control
	event                       Event
	original, transferred, seed string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, sql := range []string{
		`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT,NOTE TEXT)`,
		`CREATE TABLE DOWNLOADER(ID INTEGER PRIMARY KEY,NAME TEXT,ENABLED INTEGER,TYPE TEXT,TRANSFER INTEGER,ONLY_NASTOOL INTEGER,MATCH_PATH INTEGER,RMT_MODE TEXT,CONFIG TEXT,DOWNLOAD_DIR TEXT)`,
		`CREATE TABLE DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,TITLE TEXT,DOWNLOADER TEXT,DOWNLOAD_ID TEXT,SAVE_PATH TEXT,DATE TEXT)`,
		`CREATE TABLE PLUGIN_HISTORY(ID INTEGER PRIMARY KEY,PLUGIN_ID TEXT,KEY TEXT,VALUE TEXT,DATE TEXT)`,
		`INSERT INTO DOWNLOADER VALUES(9007199254740993,'original',1,'qbittorrent',0,0,0,'','{}','[]'),(9007199254740994,'target',1,'transmission',0,0,0,'','{}','[]')`,
		`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','plugin.TorrentRemover','{"enable":true,"opaque":9007199254740993}'),('SystemConfig','UserInstalledPlugins','["TorrentRemover"]')`,
	} {
		if _, err := s.db.Exec(sql); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{s: s, original: strings.Repeat("a", 40), transferred: strings.Repeat("b", 40), seed: strings.Repeat("c", 40)}
	_, err = s.db.Exec(`INSERT INTO DOWNLOAD_HISTORY VALUES(9007199254740993,'Fixture movie','9007199254740993',?,'/source','2026-11-06 00:00:00')`, f.original)
	if err != nil {
		t.Fatal(err)
	}
	transfer := `{"to_download":9007199254740994,"to_download_id":"` + f.transferred + `","delete_source":false,"unknown":9007199254740993}`
	seed := `[{"downloader":9007199254740994,"torrents":["` + f.seed + `"],"unknown":9007199254740993},{"downloader":9007199254740993,"torrents":["` + strings.Repeat("d", 40) + `"],"unknown":9007199254740993}]`
	if _, err = s.db.Exec(`INSERT INTO PLUGIN_HISTORY VALUES(1,'TorrentTransfer',?,?,'original-date'),(2,'IYUUAutoSeed',?,?,'seed-date')`, "9007199254740993-"+f.original, transfer, f.transferred, seed); err != nil {
		t.Fatal(err)
	}
	st, err := s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.control = Control{Exclusive: true, Epoch: strings.Repeat("a", 64), Actor: "admin", Scope: strings.Repeat("b", 64), Config: Hash(st.Raw), Targets: []Target{
		{ID: "9007199254740993", Name: "original", Type: "qbittorrent", Digest: DownloaderDigest(9007199254740993, "original", 1, "qbittorrent", 0, 0, 0, "", "{}", "[]")},
		{ID: "9007199254740994", Name: "target", Type: "transmission", Digest: DownloaderDigest(9007199254740994, "target", 1, "transmission", 0, 0, 0, "", "{}", "[]")},
	}}
	if err := s.Change(t.Context(), st, nil, nil, f.control); err != nil {
		t.Fatal(err)
	}
	f.event = Event{ID: strings.Repeat("e", 64), Removal: strings.Repeat("f", 64), History: "9007199254740993", Title: "Fixture movie", Path: "/source/Fixture/movie.mkv"}
	return f
}
func (f *fixture) capture(t *testing.T) Receipt {
	t.Helper()
	tx, err := f.s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Capture(t.Context(), tx, f.event); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.List(t.Context(), "")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	return rows[0]
}
func TestOriginalTransferAndSameDownloaderCrossSeedAreFrozenExactly(t *testing.T) {
	f := newFixture(t)
	r := f.capture(t)
	if r.State != "pending" || len(r.Tasks) != 3 || len(r.Definition.Histories) != 2 || len(r.Definition.Downloads) != 1 || r.Definition.Downloads[0].ID != "9007199254740993" {
		t.Fatal(r)
	}
	for _, task := range r.Tasks {
		if task.Role == "cross-seed" && (task.Downloader != "9007199254740994" || task.Task != f.seed || task.Parent == "") {
			t.Fatal(task)
		}
	}
	for _, h := range r.Definition.Histories {
		if !strings.Contains(h.Value, "9007199254740993") {
			t.Fatal("large unknown field lost")
		}
	}
	duplicate := f.capture(t)
	if duplicate.ID != r.ID {
		t.Fatal("same event created another receipt")
	}
}
func TestUnknownIntentCannotBeRedispatchedAfterRecovery(t *testing.T) {
	f := newFixture(t)
	r := f.capture(t)
	if err := f.s.Claim(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	task := r.Tasks[0]
	if task.Parent != "" {
		for _, t := range r.Tasks {
			if t.Parent == "" {
				task = t
				break
			}
		}
	}
	if err := f.s.Intent(t.Context(), r.ID, task.ID, strings.Repeat("1", 64), strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.Get(t.Context(), r.ID)
	if err != nil || r.State != "unknown" {
		t.Fatal(r, err)
	}
	task, _ = taskFor(r, task.ID)
	if task.State != "unknown" {
		t.Fatal(task)
	}
	if err := f.s.Claim(t.Context(), r.ID); err == nil {
		t.Fatal("interrupted receipt adopted")
	}
	if err := f.s.Intent(t.Context(), r.ID, task.ID, strings.Repeat("2", 64), strings.Repeat("d", 64)); err == nil {
		t.Fatal("unknown intent resent")
	}
	if err := f.s.Resolve(t.Context(), r.ID, task.ID, "unknown", "pending", "retry"); err == nil {
		t.Fatal("unknown reset to pending")
	}
}
func TestConfirmedRemovalCleansOnlyItsOriginalPluginRelationships(t *testing.T) {
	f := newFixture(t)
	r := f.capture(t)
	if err := f.s.Claim(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	// Cross-seeds may only run after their original/transfer parent is confirmed.
	tasks := slices.Clone(r.Tasks)
	slices.SortStableFunc(tasks, func(a, b Task) int {
		if a.Parent == "" && b.Parent != "" {
			return -1
		}
		if a.Parent != "" && b.Parent == "" {
			return 1
		}
		return 0
	})
	for _, task := range tasks {
		if err := f.s.Intent(t.Context(), r.ID, task.ID, strings.Repeat("1", 64), strings.Repeat("d", 64)); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Resolve(t.Context(), r.ID, task.ID, "intent", "completed", "已核实任务不再存在，文件保留"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.s.Finish(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM PLUGIN_HISTORY WHERE PLUGIN_ID='TorrentTransfer'`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	var raw, date string
	if err := f.s.db.QueryRow(`SELECT VALUE,DATE FROM PLUGIN_HISTORY WHERE ID=2`).Scan(&raw, &date); err != nil {
		t.Fatal(err)
	}
	var keep []map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &keep) != nil || len(keep) != 1 || !strings.Contains(raw, "9007199254740993") || date != "seed-date" {
		t.Fatal(raw, date)
	}
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM DOWNLOAD_HISTORY`).Scan(&n); err != nil || n != 1 {
		t.Fatal("download history changed", n, err)
	}
}
func TestOriginalConfigAndHistoryChangesBlockAnUnsentIntent(t *testing.T) {
	for _, change := range []string{
		`UPDATE DOWNLOADER SET CONFIG='{"changed":true}' WHERE NAME='target'`,
		`UPDATE PLUGIN_HISTORY SET VALUE='{}' WHERE ID=1`,
		`UPDATE DOWNLOAD_HISTORY SET SAVE_PATH='/other'`,
		`UPDATE SYSTEM_DICT SET VALUE='{"enable":false}' WHERE KEY='plugin.TorrentRemover'`,
	} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			r := f.capture(t)
			if err := f.s.Claim(t.Context(), r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			if err := f.s.Intent(t.Context(), r.ID, r.Tasks[0].ID, strings.Repeat("1", 64), strings.Repeat("d", 64)); err == nil {
				t.Fatal("changed source granted mutation")
			}
		})
	}
}
func TestDisabledLegacySwitchDoesNotAdoptOldOrNewEvents(t *testing.T) {
	f := newFixture(t)
	st, err := f.s.Settings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Change(t.Context(), st, nil, nil, Control{}); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Capture(context.Background(), tx, f.event); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r, err := f.s.List(t.Context(), "")
	if err != nil || len(r) != 0 {
		t.Fatal(r, err)
	}
}
func TestSourceFileMatchUsesWholePathComponents(t *testing.T) {
	f := newFixture(t)
	if !FileMatch(f.event, "Fixture/movie.mkv") || FileMatch(f.event, "../movie.mkv") || FileMatch(f.event, "ture/movie.mkv") || FileMatch(f.event, "/source/Fixture/movie.mkv") {
		t.Fatal("unsafe source-file match")
	}
	f.event.Directory = true
	f.event.Path = "/source/Fixture"
	if !FileMatch(f.event, "Fixture/movie.mkv") || FileMatch(f.event, "Fixture2/movie.mkv") {
		t.Fatal("unsafe directory match")
	}
}
