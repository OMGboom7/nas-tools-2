package rsstaskconfig

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecordDownloadWritesStatusAndHistoryAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.database.Exec(`INSERT INTO CONFIG_USER_RSS (ID,NAME,USES) VALUES (7,'RSS','D')`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload(ctx, 7, "Movie", "2026", "magnet:?xt=test", "qB"); err != nil {
		t.Fatal(err)
	}
	processed, err := store.IsProcessed(ctx, "D", "Movie", "2026", "magnet:?xt=test")
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	items, err := store.History(ctx, 7)
	if err != nil || len(items) != 1 || items[0].Title != "Movie" || items[0].Downloader != "qB" {
		t.Fatalf("history=%v err=%v", items, err)
	}
	if _, err := store.database.Exec(`CREATE TRIGGER block_history BEFORE INSERT ON USERRSS_TASK_HISTORY BEGIN SELECT RAISE(ABORT,'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload(ctx, 7, "Second", "", "magnet:?xt=second", "qB"); err == nil {
		t.Fatal("expected history failure")
	}
	var count int
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM RSS_TORRENTS WHERE ENCLOSURE='magnet:?xt=second'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial status=%d err=%v", count, err)
	}
	if err := store.RecordDownload(ctx, 999, "Unknown", "", "magnet:?xt=unknown", "qB"); err != ErrNotFound {
		t.Fatalf("missing task=%v", err)
	}
	if err := store.database.QueryRow(`SELECT COUNT(*) FROM USERRSS_TASK_HISTORY WHERE TASK_ID='999'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("missing history=%d err=%v", count, err)
	}
}
