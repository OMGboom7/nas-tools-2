//go:build darwin || linux

package backuprestore

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const originalConfig = "app:\n  login_user: original\n  login_password: original-fixture\nsecurity:\n  api_key: original-fixture-key\n"
const restoredConfig = "app:\n  login_user: restored\n  login_password: restored-fixture\nsecurity:\n  api_key: restored-fixture-key\n"

func fixtureDatabase(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT);
	CREATE TABLE CONFIG_USERS(ID INTEGER PRIMARY KEY,NAME TEXT,PASSWORD TEXT,PRIS TEXT);
	CREATE TABLE Future(DATA BLOB);INSERT INTO Future(rowid,DATA)VALUES(9007199254740993,x'00FF01');`)
	if closeErr := db.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type zipEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func makeArchive(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		w, err := z.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fixtureFiles(t *testing.T) []zipEntry {
	return []zipEntry{{"config.yaml", []byte(restoredConfig), 0600}, {"default-category.yaml", []byte("movie: {}\n"), 0600}, {"user.db", fixtureDatabase(t), 0600}}
}

func restoreFixture(t *testing.T) (Workspace, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	w, err := New(filepath.Join(dir, "alternate.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	items := map[string][]byte{
		"alternate.yaml": []byte(originalConfig), "default-category.yaml": []byte("tv: {}\n"),
		"user.db": fixtureDatabase(t), "user.db-wal": []byte("original wal"), "user.db-shm": []byte("original shm"),
		"user.db.pre-go.bak": []byte("original pre-go"), "alternate.yaml.bak": []byte("original yaml backup"), "media.db": []byte("keep media db"),
	}
	for name, data := range items {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return w, items
}

func verifyOriginal(t *testing.T, w Workspace, original map[string][]byte) {
	t.Helper()
	for name, want := range original {
		current, err := os.ReadFile(filepath.Join(w.root, name))
		if err != nil || !bytes.Equal(current, want) {
			t.Fatal("original changed", name, err)
		}
	}
}

func TestRestoreStagesAppliesAndRollsBackExactOriginals(t *testing.T) {
	w, original := restoreFixture(t)
	ctx := t.Context()
	release, err := HoldStartup(filepath.Join(w.root, w.config))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	receipt, err := w.Stage(ctx, bytes.NewReader(makeArchive(t, fixtureFiles(t))))
	if err != nil {
		t.Fatal(err)
	}
	verifyOriginal(t, w, original)
	if _, err := w.Apply(ctx, receipt.ID, true); !errors.Is(err, ErrBusy) {
		t.Fatal("allowed apply while server holds lock", err)
	}
	release()
	if _, err := w.Apply(ctx, receipt.ID, false); !errors.Is(err, ErrState) {
		t.Fatal("missing offline acknowledgment accepted", err)
	}
	tx, err := w.Apply(ctx, receipt.ID, true)
	if err != nil || tx.Phase != "applied" {
		t.Fatal(tx.Phase, err)
	}
	if _, err := HoldStartup(filepath.Join(w.root, w.config)); !errors.Is(err, ErrHold) {
		t.Fatal("started with staged replacement pending review", err)
	}
	if b, err := os.ReadFile(filepath.Join(w.root, w.config)); err != nil || string(b) != restoredConfig {
		t.Fatal("incorrect restored config", err)
	}
	if _, err := w.Resume(ctx, receipt.ID, true, func(path string) error {
		if filepath.Dir(path) == w.root {
			t.Fatal("startup preflight received the live configuration")
		}
		return ErrArchive
	}); !errors.Is(err, ErrArchive) {
		t.Fatal("startup check failure did not keep the original rollback available", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := w.Resume(cancelled, receipt.ID, true, func(string) error {
		cancel()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled preflight released startup", err)
	}
	for _, name := range []string{"user.db-wal", "user.db-shm", "user.db.pre-go.bak", "alternate.yaml.bak"} {
		if _, err := os.Stat(filepath.Join(w.root, name)); !os.IsNotExist(err) {
			t.Fatal("stale sidecar in restored directory", name, err)
		}
	}
	for name, want := range original {
		if name == "media.db" {
			continue
		}
		got, err := os.ReadFile(filepath.Join(w.candidate(receipt.ID), "previous", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("wrong retained original", name, err)
		}
	}
	tx, err = w.Rollback(ctx, receipt.ID, true)
	if err != nil || tx.Phase != "rolled-back" {
		t.Fatal(tx.Phase, err)
	}
	verifyOriginal(t, w, original)
	if _, err := w.Resume(ctx, receipt.ID, false, nil); !errors.Is(err, ErrState) {
		t.Fatal("resumed without consent", err)
	}
	if _, err := w.Resume(ctx, receipt.ID, true, nil); err != nil {
		t.Fatal(err)
	}
	reopen, err := HoldStartup(filepath.Join(w.root, w.config))
	if err != nil {
		t.Fatal(err)
	}
	reopen()
	if _, err := w.Apply(ctx, receipt.ID, true); !errors.Is(err, ErrState) {
		t.Fatal("original stage replayed", err)
	}
}

func TestRestoreRecoversAfterMovingOriginalBeforeInstallingNewFile(t *testing.T) {
	w, original := restoreFixture(t)
	ctx := t.Context()
	stage, err := w.Stage(ctx, bytes.NewReader(makeArchive(t, fixtureFiles(t))))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := w.newTransaction(ctx, stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(w.journal(), tx); err != nil {
		t.Fatal(err)
	}
	for _, entry := range tx.Entries[:len(tx.Entries)-1] {
		if err := applyEntry(ctx, w.paths(tx.ID, entry), entry); err != nil {
			t.Fatal(err)
		}
	}
	last := tx.Entries[len(tx.Entries)-1]
	p := w.paths(tx.ID, last)
	if err := checkedMove(p.live, p.previous); err != nil {
		t.Fatal(err)
	}
	if _, err := HoldStartup(filepath.Join(w.root, w.config)); !errors.Is(err, ErrHold) {
		t.Fatal("opened missing config during interruption", err)
	}
	if _, err := w.Apply(ctx, stage.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Rollback(ctx, stage.ID, true); err != nil {
		t.Fatal(err)
	}
	verifyOriginal(t, w, original)
}

func TestRestoreRejectsTraversalDuplicatesSymlinksAndBadDatabase(t *testing.T) {
	w, original := restoreFixture(t)
	entries := fixtureFiles(t)
	for _, tc := range []struct {
		name string
		last zipEntry
	}{
		{"traversal", zipEntry{"../user.db", entries[2].data, 0600}},
		{"duplicate", entries[0]},
		{"symlink", zipEntry{"user.db", []byte("/etc/passwd"), os.ModeSymlink | 0600}},
		{"bad-db", zipEntry{"user.db", []byte("invalid SQLite"), 0600}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.Stage(t.Context(), bytes.NewReader(makeArchive(t, append(append([]zipEntry{}, entries[:2]...), tc.last)))); !errors.Is(err, ErrArchive) {
				t.Fatal(err)
			}
			verifyOriginal(t, w, original)
		})
	}
	list, err := os.ReadDir(w.private())
	if err != nil || len(list) != 0 {
		t.Fatal("bad uploads left sensitive stage files", len(list), err)
	}
}
