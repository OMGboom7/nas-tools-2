package systemconfig

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestManySettingsAreOneSnapshotAndAtomicMutation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	keys := []string{"installed", "config", "state"}
	if err := s.Set(t.Context(), "config", `{"secret":"preserve"}`); err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadMany(t.Context(), keys)
	if err != nil || before["state"] != "" || before["config"] != `{"secret":"preserve"}` {
		t.Fatal(before, err)
	}
	var count int
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM SYSTEM_DICT`).Scan(&count); err != nil || count != 1 {
		t.Fatal("readonly created records", count, err)
	}
	stop := errors.New("abort")
	if err := s.UpdateMany(t.Context(), keys, func(raw map[string]string) (map[string]string, error) {
		return map[string]string{"installed": "[]"}, stop
	}); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if err := s.UpdateMany(t.Context(), keys, func(raw map[string]string) (map[string]string, error) {
		return map[string]string{"state": "{}", "outside": "overwrite"}, nil
	}); !errors.Is(err, ErrInvalidSetting) {
		t.Fatal(err)
	}
	if err := s.database.QueryRow(`SELECT COUNT(*) FROM SYSTEM_DICT`).Scan(&count); err != nil || count != 1 {
		t.Fatal("partial write survived rollback", count, err)
	}
	if err := s.UpdateMany(t.Context(), keys, func(raw map[string]string) (map[string]string, error) {
		if raw["config"] != before["config"] {
			t.Fatal("wrong snapshot")
		}
		return map[string]string{"installed": "[\"Prowlarr\"]", "state": "{}"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := s.ReadMany(t.Context(), keys)
	if err != nil || after["config"] != before["config"] || after["state"] != "{}" || after["installed"] != "[\"Prowlarr\"]" {
		t.Fatal(after, err)
	}
}

func TestManySettingsRejectIgnoredWritesAndInvalidKeySets(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.database.Exec(`CREATE TRIGGER ignore_state BEFORE INSERT ON SYSTEM_DICT WHEN NEW.KEY='state' BEGIN SELECT RAISE(IGNORE);END`); err != nil {
		t.Fatal(err)
	}
	err = s.UpdateMany(t.Context(), []string{"config", "state"}, func(raw map[string]string) (map[string]string, error) {
		return map[string]string{"config": "new", "state": "{}"}, nil
	})
	if err == nil {
		t.Fatal("ignored write accepted")
	}
	if raw, err := s.Get(t.Context(), "config"); err != nil || raw != "" {
		t.Fatal("transaction partially committed", raw, err)
	}
	for _, keys := range [][]string{nil, {"same", "same"}, {""}} {
		if _, err := s.ReadMany(t.Context(), keys); !errors.Is(err, ErrInvalidSetting) {
			t.Fatal(keys, err)
		}
	}
}
