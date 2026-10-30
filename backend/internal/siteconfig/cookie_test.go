package siteconfig

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCookieUpdateRetainsUnknownRawValuesAndRejectsSilentReplacement(t *testing.T) {
	for _, mode := range []string{"normal", "ignore", "replace", "ambiguous"} {
		t.Run(mode, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "user.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			note := `{"ua":"old","unknown":9007199254740993,"nested":{"n":9007199254740993},"extra":null}`
			if mode == "ambiguous" {
				note = `{"ua":"one","ua":"two"}`
			}
			original, err := s.Upsert(t.Context(), Site{Name: "Fixture", Cookie: "old=private", Note: note})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ignore" {
				_, err = s.database.Exec(`CREATE TRIGGER ignore_cookie BEFORE UPDATE OF COOKIE ON CONFIG_SITE BEGIN SELECT RAISE(IGNORE);END`)
			}
			if mode == "replace" {
				_, err = s.database.Exec(`CREATE TRIGGER replace_cookie AFTER UPDATE OF COOKIE ON CONFIG_SITE BEGIN UPDATE CONFIG_SITE SET COOKIE='foreign=private' WHERE ID=NEW.ID;END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = s.UpdateCookieAndAgent(t.Context(), original.ID, "new=private", "new-agent")
			current, readErr := s.Get(t.Context(), original.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "normal" {
				if err != nil || current.Cookie != "new=private" || strings.Count(current.Note, "9007199254740993") != 2 || !strings.Contains(current.Note, `"extra":null`) || !strings.Contains(current.Note, "new-agent") {
					t.Fatal(current, err)
				}
			} else if err == nil || current != original {
				t.Fatal("unconfirmed cookie update was published", current, err)
			}
		})
	}
}
