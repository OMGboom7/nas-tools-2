package systemconfig

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func TestStoreUpdateIsAtomicAndRollsBackValidation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var wait sync.WaitGroup
	for index := 0; index < 30; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := store.Update(t.Context(), "Counter", func(raw string) (string, error) { count, _ := strconv.Atoi(raw); return strconv.Itoa(count + 1), nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	current, err := store.Get(t.Context(), "Counter")
	if err != nil || current != "30" {
		t.Fatalf("concurrent setting=%q %v", current, err)
	}
	rejected := errors.New("rejected setting")
	if err := store.Update(t.Context(), "Counter", func(string) (string, error) { return "0", rejected }); !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	current, err = store.Get(t.Context(), "Counter")
	if err != nil || current != "30" {
		t.Fatal("callback error committed mutation")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Update(ctx, "Counter", func(string) (string, error) { t.Fatal("cancelled callback ran"); return "0", nil }); err == nil {
		t.Fatal("cancelled update succeeded")
	}
}

func TestStoreSetInsertsAndUpdates(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Set(ctx, "DefaultDownloader", "4"); err != nil {
		t.Fatalf("Set(insert) error = %v", err)
	}
	if err := store.Set(ctx, "DefaultDownloader", "8"); err != nil {
		t.Fatalf("Set(update) error = %v", err)
	}
	value, err := store.Get(ctx, "DefaultDownloader")
	if err != nil || value != "8" {
		t.Fatalf("Get() = %q, %v", value, err)
	}
}
