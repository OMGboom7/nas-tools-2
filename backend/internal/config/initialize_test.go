package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConcurrentFirstStartsPublishOneConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 6)
	for i := 0; i < cap(results); i++ {
		go func() {
			created, err := InitializeApplication(path)
			results <- result{created, err}
		}()
	}
	creators := 0
	for i := 0; i < cap(results); i++ {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.created {
			creators++
		}
	}
	if _, err := LoadApplication(path); err != nil || creators != 1 {
		t.Fatal(creators, err)
	}
}

func TestUpgradeRefusesToReplaceBackupOrSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const original = "app: {login_user: fixture, login_password: fixture}\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".pre-go-key.bak", []byte("prior backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitializeSigningKey(path); err == nil {
		t.Fatal("overwrote existing backup")
	}
	link := filepath.Join(dir, "linked.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Skip(err)
	}
	if err := InitializeSigningKey(link); err == nil {
		t.Fatal("replaced linked config during automatic upgrade")
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != original {
		t.Fatal("configuration changed")
	}
}
