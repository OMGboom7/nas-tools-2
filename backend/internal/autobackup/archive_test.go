//go:build darwin || linux

package autobackup

import (
	"archive/zip"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveLegacyLayoutFromLiveWAL(t *testing.T) {
	dir := t.TempDir()
	config, category, source := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "default-category.yaml"), filepath.Join(dir, "user.db")
	for _, file := range []string{config, category} {
		if err := os.WriteFile(file, []byte("fixture: true\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(source, "rwc"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
		CREATE TABLE TRANSFER_HISTORY(ID INTEGER, SECRET TEXT);
		INSERT INTO TRANSFER_HISTORY VALUES(1,'fixture-history');
		CREATE TABLE Future (ID INTEGER PRIMARY KEY, DATA BLOB);
		INSERT INTO Future VALUES(9007199254740993,x'00FF01');
		CREATE TABLE LegacyWithoutPK(VALUE TEXT);
		INSERT INTO LegacyWithoutPK(rowid,VALUE) VALUES(9007199254740993,'original');`); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(source + "-wal"); err != nil || info.Size() == 0 {
		t.Fatal("WAL fixture not created", err)
	}
	path := filepath.Join(dir, "backup_file")
	target, err := Review(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("review touched destination", err)
	}
	target, err = Prepare(target)
	if err != nil {
		t.Fatal(err)
	}
	for index, full := range []bool{false, true} {
		id := strings.Repeat(string(rune('a'+index)), 64)
		stage, err := Build(t.Context(), Sources{Config: config, Category: category, Database: source}, target, id, time.Now(), full)
		if err != nil {
			t.Fatal(err)
		}
		defer stage.Close()
		if err := stage.Publish(); err != nil {
			t.Fatal(err)
		}
		if err := stage.Publish(); err == nil {
			t.Fatal("publication overwrote existing file")
		}
		f, err := Open(t.Context(), target, stage.Archive)
		if err != nil {
			t.Fatal(err)
		}
		z, err := zip.NewReader(f, stage.Archive.Size)
		if err != nil {
			t.Fatal(err)
		}
		if len(z.File) != 3 || z.File[0].Name != "config.yaml" || z.File[1].Name != "default-category.yaml" || z.File[2].Name != "user.db" {
			t.Fatal("unexpected ZIP layout")
		}
		entry, err := z.File[2].Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(entry)
		entry.Close()
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		snapshot := filepath.Join(t.TempDir(), "user.db")
		if err := os.WriteFile(snapshot, b, 0600); err != nil {
			t.Fatal(err)
		}
		saved, err := sql.Open("sqlite", sqliteDSN(snapshot, "ro"))
		if err != nil {
			t.Fatal(err)
		}
		var future string
		err = saved.QueryRow("SELECT CAST(ID AS TEXT)||':'||HEX(DATA) FROM Future").Scan(&future)
		if err != nil || future != "9007199254740993:00FF01" {
			t.Fatal(future, err)
		}
		var rowID string
		if err := saved.QueryRow("SELECT CAST(rowid AS TEXT) FROM LegacyWithoutPK").Scan(&rowID); err != nil || rowID != "9007199254740993" {
			t.Fatal("original implicit row ID changed", rowID, err)
		}
		var exists bool
		err = saved.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='TRANSFER_HISTORY')").Scan(&exists)
		saved.Close()
		if err != nil || exists != full {
			t.Fatal(exists, full, err)
		}
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM TRANSFER_HISTORY").Scan(&n); err != nil || n != 1 {
		t.Fatal("live DB was altered", err)
	}
}

func TestArchiveRetentionRejectsReplacementAndSymlink(t *testing.T) {
	dir := t.TempDir()
	target, err := Review(dir)
	if err != nil {
		t.Fatal(err)
	}
	name := "bk_20261105000000_" + strings.Repeat("a", 64) + ".zip"
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	called := false
	err = Remove(t.Context(), target, Archive{name, 4, strings.Repeat("0", 64)}, func(run func() error) error { called = true; return run() })
	if err == nil || called {
		t.Fatal("attempted to remove unrelated file")
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "keep" {
		t.Fatal("unrelated file changed", err)
	}
}

func TestRetentionRecoversOriginalReceiptAfterUnlink(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "user.db")
	db, err := sql.Open("sqlite", sqliteDSN(dbPath, "rwc"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT);
		INSERT INTO SYSTEM_DICT VALUES(1,'SystemConfig','UserInstalledPlugins','["AutoBackup"]');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target, err := Review(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	create := func(id string) Run {
		r, err := store.Start(t.Context(), st, st.Control, Run{ID: id, Kind: "manual", Target: target})
		if err != nil {
			t.Fatal(err)
		}
		finished := r
		finished.State = "complete"
		finished.Archive.Name = "bk_20261105000000_" + id + ".zip"
		if err := store.Replace(t.Context(), r, finished, &st, false); err != nil {
			t.Fatal(err)
		}
		return finished
	}
	old, owner := create(strings.Repeat("a", 64)), create(strings.Repeat("b", 64))
	leaf := filepath.Join(dir, old.Archive.Name)
	if err := os.WriteFile(leaf, []byte("old fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	err = store.Prune(t.Context(), st, owner, old, func() error {
		if err := os.Remove(leaf); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	})
	if err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
	pending, err := store.PendingPrunes(t.Context())
	if err != nil || len(pending) != 1 || pending[0].Retiring != owner.ID {
		t.Fatal(pending, err)
	}
	if err := store.ReconcilePrune(t.Context(), pending[0]); err != nil {
		t.Fatal(err)
	}
	after, err := store.Run(t.Context(), old.ID)
	if err != nil || !after.Retired || after.Retiring != "" {
		t.Fatal(after, err)
	}
	newer, err := store.Run(t.Context(), owner.ID)
	if err != nil || newer.Pruned != 1 {
		t.Fatal(newer, err)
	}
}
