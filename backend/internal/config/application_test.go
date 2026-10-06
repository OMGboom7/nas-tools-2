package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadApplication(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	contents := []byte("app:\n  login_user: admin\n  login_password: '[hash]scrypt:32768:8:1$salt$digest'\nsecurity:\n  api_key: test-secret\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}

	application, err := LoadApplication(path)
	if err != nil {
		t.Fatalf("LoadApplication() error = %v", err)
	}
	if application.App.LoginUser != "admin" || application.Security.APIKey != "test-secret" {
		t.Fatalf("unexpected application config: %#v", application)
	}
	if got := application.UserDatabasePath(); got != filepath.Join(directory, "user.db") {
		t.Fatalf("UserDatabasePath() = %q", got)
	}
}

func TestLoadApplicationRequiresAuthenticationSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  login_user: admin\n"), 0o600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	if _, err := LoadApplication(path); err == nil {
		t.Fatal("LoadApplication() error = nil, want validation error")
	}
}
