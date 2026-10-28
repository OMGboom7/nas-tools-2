package systemconfig

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUpdateManyIgnoredExistingWriteCannotCreateDuplicateAuthority(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Set(ctx, "control", "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.Exec(`CREATE TRIGGER IGNORE_CONTROL BEFORE UPDATE ON SYSTEM_DICT WHEN NEW.KEY='control' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	err = s.UpdateMany(ctx, []string{"control"}, func(map[string]string) (map[string]string, error) { return map[string]string{"control": "open"}, nil })
	if err == nil {
		t.Fatal("ignored authority update reported saved")
	}
	var count int
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM SYSTEM_DICT WHERE KEY='control'`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if v, err := s.Get(ctx, "control"); err != nil || v != "closed" {
		t.Fatal(v, err)
	}
}

func TestUpdateManyLaterTriggerCannotRevokeEarlierVerifiedValueSilently(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	s.Set(ctx, "a", "closed")
	s.Set(ctx, "b", "closed")
	// Whichever key is updated last corrupts the other; final verification must
	// observe both keys, independent of Go map iteration order.
	if _, err := s.database.Exec(`CREATE TRIGGER MUTATE_OTHER AFTER UPDATE ON SYSTEM_DICT WHEN NEW.VALUE='open' BEGIN UPDATE SYSTEM_DICT SET VALUE='altered' WHERE KEY<>NEW.KEY; END`); err != nil {
		t.Fatal(err)
	}
	err = s.UpdateMany(ctx, []string{"a", "b"}, func(map[string]string) (map[string]string, error) {
		return map[string]string{"a": "open", "b": "open"}, nil
	})
	if err == nil {
		t.Fatal("partial grant committed")
	}
	for _, key := range []string{"a", "b"} {
		if v, err := s.Get(ctx, key); err != nil || v != "closed" {
			t.Fatal(key, v, err)
		}
	}
}
