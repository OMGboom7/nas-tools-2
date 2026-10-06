package filterconfig

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestGroupsReadLegacySchemaWithoutCachingOrWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE CONFIG_FILTER_GROUP (ID INTEGER PRIMARY KEY, GROUP_NAME TEXT, IS_DEFAULT TEXT, NOTE TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO CONFIG_FILTER_GROUP VALUES (8, 'HD', 'Y', NULL), (3, 'Other', NULL, 'keep-note')"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	groups, err := store.Groups(context.Background())
	if err != nil || len(groups) != 2 || groups[0].ID != 3 || groups[0].Default || groups[0].Note != "keep-note" || !groups[1].Default || groups[1].Note != "" {
		t.Fatalf("legacy groups = %+v, %v", groups, err)
	}
	if _, err := store.database.Exec("DELETE FROM CONFIG_FILTER_GROUP"); err == nil {
		t.Fatal("reader unexpectedly allowed writes")
	}
	if _, err := db.Exec("UPDATE CONFIG_FILTER_GROUP SET GROUP_NAME='Renamed' WHERE ID=8"); err != nil {
		t.Fatal(err)
	}
	groups, err = store.Groups(context.Background())
	if err != nil || len(groups) != 2 || groups[1].Name != "Renamed" {
		t.Fatalf("groups retained stale values: %+v, %v", groups, err)
	}
}

func TestGroupsWithNoLegacyTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE unrelated (ID INTEGER)"); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	groups, err := store.Groups(context.Background())
	if err != nil || groups == nil || len(groups) != 0 {
		t.Fatalf("empty groups = %+v, %v", groups, err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("reader changed schema: count=%d err=%v", count, err)
	}
}
