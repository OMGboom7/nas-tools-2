package downloaderconfig

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestTaskHistoryReadsNewestLegacyRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE DOWNLOAD_HISTORY (ID INTEGER PRIMARY KEY, TITLE TEXT, YEAR TEXT, SE TEXT, POSTER TEXT, DOWNLOADER TEXT, DOWNLOAD_ID TEXT, DATE TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO DOWNLOAD_HISTORY VALUES (1,'Old','2023','S01','old.jpg','1','hash','2025-01-01'),(2,'New',NULL,NULL,'new.jpg','1','hash','2026-01-01')"); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := store.TaskHistory(context.Background(), "1", "hash")
	if err != nil || history.Title != "New" || history.Year != "" || history.Poster != "new.jpg" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	missing, err := store.TaskHistory(context.Background(), "1", "missing")
	if err != nil || missing != (TaskHistory{}) {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
}

func TestTaskHistoryHandlesFreshDatabase(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history, err := store.TaskHistory(context.Background(), "1", "hash")
	if err != nil || history != (TaskHistory{}) {
		t.Fatalf("history = %+v, %v", history, err)
	}
}

func TestListDownloadHistoryReturnsNewestTitlePerPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE DOWNLOAD_HISTORY (ID INTEGER PRIMARY KEY, TITLE TEXT, YEAR TEXT, TYPE TEXT, TMDBID TEXT, POSTER TEXT, TORRENT TEXT, SITE TEXT, DATE TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO DOWNLOAD_HISTORY VALUES
		(1,'Movie A','2024','电影','101','old.jpg','Old.Release','OldSite','2026-01-01 00:00:00'),
		(2,'Movie A','2025','电影','101','new.jpg','New.Release','NewSite','2026-03-01 00:00:00'),
		(3,'Show B','2026','电视剧','202',NULL,'Show.Release',NULL,'2026-02-01 00:00:00'),
		(4,'Movie C','2023','电影','303','c.jpg','C.Release','SiteC','2026-01-15 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	_ = database.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.ListDownloadHistory(context.Background(), 1, 2)
	if err != nil || len(first) != 2 || first[0].Title != "Movie A" || first[0].Year != "2025" || first[0].Torrent != "New.Release" || first[1].Title != "Show B" || first[1].Poster != "" || first[1].Site != "" {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	second, err := store.ListDownloadHistory(context.Background(), 2, 2)
	if err != nil || len(second) != 1 || second[0].Title != "Movie C" {
		t.Fatalf("second page = %#v, %v", second, err)
	}
	if _, err := store.ListDownloadHistory(context.Background(), 0, 30); err == nil {
		t.Fatal("accepted invalid history page")
	}
}

func TestListDownloadHistoryHandlesFreshDatabase(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	items, err := store.ListDownloadHistory(context.Background(), 1, 30)
	if err != nil || len(items) != 0 {
		t.Fatalf("history = %#v, %v", items, err)
	}
}

func TestStoreLifecycle(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	created, err := store.Upsert(ctx, Downloader{Name: "主下载器", Enabled: 1, Type: "qbittorrent", Transfer: 1, OnlyNastool: 1, RmtMode: "link", Config: `{"host":"qb.local"}`, DownloadDir: `[]`})
	if err != nil || created.ID == 0 {
		t.Fatalf("Upsert(insert) = %#v, %v", created, err)
	}
	created.MatchPath = 1
	if _, err := store.Upsert(ctx, created); err != nil {
		t.Fatalf("Upsert(update) error = %v", err)
	}
	if err := store.SetFlag(ctx, created.ID, "enabled", 0); err != nil {
		t.Fatalf("SetFlag() error = %v", err)
	}
	item, err := store.Get(ctx, created.ID)
	if err != nil || item.Enabled != 0 || item.MatchPath != 1 {
		t.Fatalf("Get() = %#v, %v", item, err)
	}
	items, err := store.List(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("List() = %#v, %v", items, err)
	}
	if err := store.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(deleted) error = %v", err)
	}
}

func TestDownloadSettingLifecycle(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	created, err := store.UpsertSetting(ctx, DownloadSetting{Name: "高清", Tags: "NASTOOL", RatioLimit: 150, DownloaderID: "3"})
	if err != nil || created.ID == 0 {
		t.Fatalf("UpsertSetting(insert) = %#v, %v", created, err)
	}
	created.DownloadLimit = 2048
	if _, err := store.UpsertSetting(ctx, created); err != nil {
		t.Fatalf("UpsertSetting(update) error = %v", err)
	}
	item, err := store.GetSetting(ctx, created.ID)
	if err != nil || item.DownloadLimit != 2048 || item.RatioLimit != 150 {
		t.Fatalf("GetSetting() = %#v, %v", item, err)
	}
	items, err := store.ListSettings(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListSettings() = %#v, %v", items, err)
	}
	if err := store.DeleteSetting(ctx, created.ID); err != nil {
		t.Fatalf("DeleteSetting() error = %v", err)
	}
	if _, err := store.GetSetting(ctx, created.ID); !errors.Is(err, ErrSettingNotFound) {
		t.Fatalf("GetSetting(deleted) error = %v", err)
	}
}
