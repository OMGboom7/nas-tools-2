package filterconfig

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestMutationLifecycleAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first, err := store.AddGroup(ctx, "First", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AddGroup(ctx, "Second", true)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := store.Groups(ctx)
	if err != nil || len(groups) != 2 || groups[0].Default || !groups[1].Default {
		t.Fatalf("default groups: %+v %v", groups, err)
	}
	if err := store.SetDefault(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing default = %v", err)
	}
	if err := store.SetDefault(ctx, first); err != nil {
		t.Fatal(err)
	}
	id, err := store.AddGroup(ctx, "First", true)
	if err != nil || id != first {
		t.Fatalf("same-name group = %d %v", id, err)
	}
	rule := Rule{GroupID: first, Name: "HD", Priority: "2", Include: "1080p\nWEB", Exclude: "CAM", Size: "1,20", Free: "1.0 0.0"}
	rule.ID, err = store.SaveRule(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Rule(ctx, first, rule.ID)
	if err != nil || got != rule {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	rule.Name = "Updated"
	if _, err := store.SaveRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	rule.GroupID = second
	if _, err := store.SaveRule(ctx, rule); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-group edit = %v", err)
	}
	if _, err := db.Exec("CREATE TRIGGER reject_group_delete BEFORE DELETE ON CONFIG_FILTER_GROUP BEGIN SELECT RAISE(ABORT, 'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(ctx, first); err == nil {
		t.Fatal("expected delete failure")
	}
	got, err = store.Rule(ctx, first, rule.ID)
	if err != nil || got.Name != "Updated" {
		t.Fatalf("group delete did not roll back rules: %+v %v", got, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_group_delete"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteGroup(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rule(ctx, first, rule.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan rule: %v", err)
	}
	if err := store.SetDefault(ctx, 0); err != nil {
		t.Fatal(err)
	}
	groups, err = store.Groups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Default {
		t.Fatalf("clear default: %+v %v", groups, err)
	}
}
