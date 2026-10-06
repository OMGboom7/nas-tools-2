package siteconfig

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreLifecycleAndDuplicateProtection(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	created, err := store.Upsert(ctx, Site{Name: "测试站", Priority: "1", SignURL: "https://example.com", Note: `{}`})
	if err != nil || created.ID == 0 {
		t.Fatalf("Upsert(insert) = %#v, %v", created, err)
	}
	if _, err := store.Upsert(ctx, Site{Name: "测试站", Priority: "2"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate Upsert() error = %v", err)
	}
	created.Cookie = "secret"
	if _, err := store.Upsert(ctx, created); err != nil {
		t.Fatalf("Upsert(update) error = %v", err)
	}
	item, err := store.Get(ctx, created.ID)
	if err != nil || item.Cookie != "secret" {
		t.Fatalf("Get() = %#v, %v", item, err)
	}
	items, err := store.List(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("List() = %#v, %v", items, err)
	}
	if err := store.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestLegacySchemaRenameIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Match the pre-migration table, including nullable legacy columns.
	if _, err := db.Exec("CREATE TABLE CONFIG_SITE (ID INTEGER PRIMARY KEY, NAME TEXT, PRI TEXT, RSSURL TEXT, SIGNURL TEXT, COOKIE TEXT, APIKEY TEXT, INCLUDE TEXT, EXCLUDE TEXT, SIZE TEXT, NOTE TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO CONFIG_SITE (ID, NAME, PRI, COOKIE) VALUES (42, 'Old PT', '10', 'keep-cookie')"); err != nil {
		t.Fatal(err)
	}
	tables := []string{"SITE_USER_INFO_STATS", "SITE_USER_SEEDING_INFO", "SITE_STATISTICS_HISTORY"}
	for _, table := range tables {
		if _, err := db.Exec("CREATE TABLE " + table + " (SITE TEXT, PAYLOAD TEXT)"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO " + table + " VALUES ('Old PT', 'keep-history'), ('Unrelated PT', 'unrelated')"); err != nil {
			t.Fatal(err)
		}
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	item, err := store.Get(ctx, 42)
	if err != nil || item.Cookie != "keep-cookie" || item.Note != "" {
		t.Fatalf("read old nullable schema: %+v, %v", item, err)
	}
	item.Name = "New PT"
	if _, err := store.Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	assertHistory := func() {
		t.Helper()
		for _, table := range tables {
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE (SITE='New PT' AND PAYLOAD='keep-history') OR (SITE='Unrelated PT' AND PAYLOAD='unrelated')").Scan(&count); err != nil || count != 2 {
				t.Fatalf("%s history not preserved: count=%d err=%v", table, count, err)
			}
		}
	}
	assertHistory()
	if _, err := db.Exec("CREATE TRIGGER reject_history_rename BEFORE UPDATE ON SITE_STATISTICS_HISTORY BEGIN SELECT RAISE(ABORT, 'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	item.Name, item.Cookie = "Failed rename", "changed-cookie"
	if _, err := store.Upsert(ctx, item); err == nil {
		t.Fatal("expected history write failure")
	}
	item, err = store.Get(ctx, 42)
	if err != nil || item.Name != "New PT" || item.Cookie != "keep-cookie" {
		t.Fatalf("site edit was not rolled back: %+v, %v", item, err)
	}
	assertHistory()
}
