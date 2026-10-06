package wordconfig

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func sharingFixture(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "user.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close(); database.Close() })
	return store, database
}

func TestLegacyWordSharingRoundTrip(t *testing.T) {
	store, database := sharingFixture(t)
	code := base64.StdEncoding.EncodeToString([]byte(`{"-1":{"id":-1,"title":"通用","type":1,"words":{"7":{"id":7,"replaced":"Original","replace":"New","front":null,"back":null,"offset":null,"type":2,"season":-2,"regex":0,"help":"说明"}}}}@@@@@@legacy note@@@@@@extra`))
	groups, note, err := ParseShare(code)
	if err != nil || note != "legacy note@@@@@@extra" {
		t.Fatalf("legacy parse note=%q err=%v", note, err)
	}
	if err := store.Import(t.Context(), groups, []Selection{{-1, 7}}); err != nil {
		t.Fatal(err)
	}
	var replacement string
	var enabled int
	if err := database.QueryRow(`SELECT "REPLACE",ENABLED FROM CUSTOM_WORDS`).Scan(&replacement, &enabled); err != nil || replacement != "New" || enabled != 1 {
		t.Fatalf("import replacement=%q enabled=%d err=%v", replacement, enabled, err)
	}
	exported, err := store.Export(t.Context(), []Selection{{-1, 1}}, note)
	if err != nil {
		t.Fatal(err)
	}
	parsed, parsedNote, err := ParseShare(exported)
	if err != nil || parsedNote != note || parsed["-1"].Words["1"].Replaced != "Original" {
		t.Fatalf("round trip=%+v note=%q err=%v", parsed, parsedNote, err)
	}
}

func TestWordImportDuplicateRollsBackGroupAndEarlierWords(t *testing.T) {
	store, database := sharingFixture(t)
	if _, err := database.Exec(`INSERT INTO CUSTOM_WORDS (REPLACED,TYPE,GROUP_ID) VALUES ('duplicate',2,-1)`); err != nil {
		t.Fatal(err)
	}
	groups := map[string]SharedGroup{"9": {ID: 9, Title: "Import group", Type: 2, TMDBID: 200, Seasons: 2, Words: map[string]SharedWord{
		"1": {ID: 1, Replaced: "new", Type: 2, Season: -1},
		"2": {ID: 2, Replaced: "duplicate", Type: 2, Season: -1},
	}}}
	err := store.Import(t.Context(), groups, []Selection{{9, 1}, {9, 2}})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate err=%v", err)
	}
	var groupsCount, wordsCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM CUSTOM_WORD_GROUPS`).Scan(&groupsCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM CUSTOM_WORDS`).Scan(&wordsCount); err != nil || groupsCount != 0 || wordsCount != 1 {
		t.Fatalf("partial import groups=%d words=%d err=%v", groupsCount, wordsCount, err)
	}
}

func TestWordShareRejectsInvalidInputBeforeImport(t *testing.T) {
	for _, raw := range []string{
		`{"-1":{"id":-1,"title":"通用","type":1,"words":{"1":{"id":1,"type":4,"offset":"__import__('os')"}}}}@@@@@@note`,
		`{"5":{"id":6,"title":"Wrong ID","type":2,"tmdbid":200,"words":{}}}@@@@@@note`,
		strings.Repeat("x", (1<<20)+1),
	} {
		if _, _, err := ParseShare(base64.StdEncoding.EncodeToString([]byte(raw))); !errors.Is(err, ErrInvalidShare) {
			t.Fatalf("invalid share accepted: %v", err)
		}
	}
}
