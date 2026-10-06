package filterconfig

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinTemplatesMatchLegacyAndRestoreAtomically(t *testing.T) {
	legacy, err := os.ReadFile("../../../scripts/sqls/init_filter.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(legacy)) != strings.TrimSpace(builtinSQL) {
		t.Fatal("embedded templates differ from original templates")
	}
	ctx := context.Background()
	templates, err := Builtins(ctx)
	if err != nil || len(templates) != 3 {
		t.Fatalf("templates: %+v %v", templates, err)
	}
	if templates[0].Group.ID != 1000 || len(templates[0].Rules) != 18 || templates[1].Group.ID != 1001 || len(templates[1].Rules) != 16 || templates[2].Group.ID != 9999 || !templates[2].Group.Default || len(templates[2].Rules) != 0 {
		t.Fatalf("unexpected template contents: %+v", templates)
	}
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
	if err := store.Restore(ctx, []int64{1000, 1001, 9999}); err != nil {
		t.Fatal(err)
	}
	actual, err := store.List(ctx)
	if err != nil || !reflect.DeepEqual(actual, templates) {
		t.Fatalf("restored templates differ: %v", err)
	}
	if err := store.Restore(ctx, []int64{1000, 1001, 9999}); err != nil {
		t.Fatal(err)
	}
	again, err := store.List(ctx)
	if err != nil || !reflect.DeepEqual(actual, again) {
		t.Fatal("restore is not idempotent")
	}
	custom, err := store.AddGroup(ctx, "Custom", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveRule(ctx, Rule{GroupID: custom, Name: "Keep", Priority: "1"}); err != nil {
		t.Fatal(err)
	}
	before, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, []int64{1000, 12345}); !errors.Is(err, ErrInvalidTemplate) {
		t.Fatalf("invalid template: %v", err)
	}
	after, err := store.List(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("invalid restore changed data")
	}
	// A historical/custom rule now occupies a template ID. Never discard it.
	if _, err := db.Exec("UPDATE CONFIG_FILTER_RULES SET GROUP_ID=? WHERE ID=10000", custom); err != nil {
		t.Fatal(err)
	}
	before, err = store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, []int64{1000, 9999}); !errors.Is(err, ErrTemplateConflict) {
		t.Fatalf("collision: %v", err)
	}
	after, err = store.List(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("conflicting restore did not roll back")
	}
	if _, err := db.Exec("UPDATE CONFIG_FILTER_RULES SET GROUP_ID='1000' WHERE ID=10000"); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, []int64{1000}); err != nil {
		t.Fatal(err)
	}
	groups, err := store.Groups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group.Default != (group.ID == custom) {
			t.Fatalf("partial restore changed unrelated default: %+v", groups)
		}
	}
}
