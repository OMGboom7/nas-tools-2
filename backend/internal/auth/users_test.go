package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestSQLiteUserStoreFindByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	_, err = database.Exec("CREATE TABLE CONFIG_USERS (ID INTEGER PRIMARY KEY, NAME TEXT, PASSWORD TEXT, PRIS TEXT); INSERT INTO CONFIG_USERS VALUES (7, 'viewer', 'password', '我的媒体库,资源搜索');")
	if err != nil {
		t.Fatalf("create fixture database: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}

	store, err := OpenSQLiteUserStore(path)
	if err != nil {
		t.Fatalf("OpenSQLiteUserStore() error = %v", err)
	}
	defer store.Close()
	user, err := store.FindByName(context.Background(), "viewer")
	if err != nil {
		t.Fatalf("FindByName() error = %v", err)
	}
	if user.ID != 7 || len(user.Permissions) != 2 || user.Permissions[1] != "资源搜索" {
		t.Fatalf("unexpected user: %#v", user)
	}
}

func TestSQLiteUserStoreCreateListDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	store, err := OpenSQLiteUserStore(path)
	if err != nil {
		t.Fatalf("OpenSQLiteUserStore() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	created, err := store.Create(ctx, User{Name: "operator", Password: "hash", Permissions: []string{"下载管理"}})
	if err != nil || created.ID == 0 {
		t.Fatalf("Create() = %#v, %v", created, err)
	}
	if _, err := store.Create(ctx, User{Name: "operator", Password: "hash"}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate Create() error = %v", err)
	}
	users, err := store.List(ctx)
	if err != nil || len(users) != 1 || users[0].Name != "operator" {
		t.Fatalf("List() = %#v, %v", users, err)
	}
	if err := store.UpdatePassword(ctx, created.ID, "new-hash"); err != nil {
		t.Fatalf("UpdatePassword() error = %v", err)
	}
	updated, err := store.FindByName(ctx, "operator")
	if err != nil || updated.Password != "new-hash" {
		t.Fatalf("FindByName(updated) = %#v, %v", updated, err)
	}
	if err := store.Delete(ctx, "operator"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "operator"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("second Delete() error = %v", err)
	}
}
