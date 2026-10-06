package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func testApplication(directory string) config.Application {
	return config.Application{
		Path: filepath.Join(directory, "config.yaml"),
		App: config.ApplicationApp{
			LoginUser: "admin", LoginPassword: "password",
		},
		Security: config.ApplicationSecurity{APIKey: "test-secret"},
	}
}

func TestServiceAuthenticatesAdminAndManagesUsers(t *testing.T) {
	service, err := NewService(testApplication(t.TempDir()))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	ctx := context.Background()
	admin, err := service.Authenticate(ctx, "admin", "password")
	if err != nil || admin.ID != 0 || !service.IsAdministrator(admin.Name) {
		t.Fatalf("Authenticate(admin) = %#v, %v", admin, err)
	}

	created, err := service.CreateUser(ctx, "viewer", "strong-password", []string{"我的媒体库", "资源搜索"})
	if err != nil || created.ID == 0 {
		t.Fatalf("CreateUser() = %#v, %v", created, err)
	}
	viewer, err := service.Authenticate(ctx, "viewer", "strong-password")
	if err != nil || viewer.Name != "viewer" || len(viewer.Permissions) != 2 {
		t.Fatalf("Authenticate(viewer) = %#v, %v", viewer, err)
	}
	if _, err := service.Authenticate(ctx, "viewer", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Authenticate(wrong) error = %v", err)
	}
	if err := service.DeleteUser(ctx, "viewer"); err != nil {
		t.Fatalf("DeleteUser() error = %v", err)
	}
}

func TestServiceRejectsInvalidUserMutations(t *testing.T) {
	service, err := NewService(testApplication(t.TempDir()))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	ctx := context.Background()
	if _, err := service.CreateUser(ctx, "bad name", "strong-password", []string{"资源搜索"}); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("CreateUser(invalid name) error = %v", err)
	}
	if _, err := service.CreateUser(ctx, "viewer", "short", []string{"资源搜索"}); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("CreateUser(short password) error = %v", err)
	}
	if _, err := service.CreateUser(ctx, "viewer", "strong-password", []string{"未知权限"}); !errors.Is(err, ErrInvalidUser) {
		t.Fatalf("CreateUser(invalid permission) error = %v", err)
	}
	if err := service.DeleteUser(ctx, "admin"); !errors.Is(err, ErrAdministratorImmutable) {
		t.Fatalf("DeleteUser(admin) error = %v", err)
	}
}

func TestAuthenticateUpgradesLegacyUserPassword(t *testing.T) {
	directory := t.TempDir()
	service, err := NewService(testApplication(directory))
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	ctx := context.Background()
	legacy := "pbkdf2:sha256:260000$nastool1$cff28e0a04428d3e410b457950db9fc19bdb08a263d337c782e955bd48afd824"
	created, err := service.users.Create(ctx, User{Name: "legacy", Password: legacy, Permissions: []string{"我的媒体库"}})
	if err != nil {
		t.Fatalf("create legacy user: %v", err)
	}
	if _, err := service.Authenticate(ctx, "legacy", "correct horse battery staple"); err != nil {
		t.Fatalf("Authenticate(legacy) error = %v", err)
	}
	upgraded, err := service.users.FindByName(ctx, "legacy")
	if err != nil {
		t.Fatalf("FindByName(upgraded) error = %v", err)
	}
	if upgraded.ID != created.ID || !strings.HasPrefix(upgraded.Password, "scrypt:32768:8:1$") {
		t.Fatalf("password was not upgraded: %#v", upgraded)
	}
}
