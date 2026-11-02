package sitesignin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOriginalCacheRetainsUnknownBigIntegersAndConcurrentEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "temp", "signin")
	if e := os.MkdirAll(path, 0700); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(path, "52pt.json")
	original := `{"old question":[2,4],"future":{"big":9007199254740993,"nil":null}}`
	if e := os.WriteFile(file, []byte(original), 0600); e != nil {
		t.Fatal(e)
	}
	c := &FileCache{Directory: dir}
	values, token, e := c.Read(context.Background(), "52pt")
	if e != nil || strings.Join(values["old question"], ",") != "2,4" {
		t.Fatal("old numeric answer lost")
	}
	guard := func(context.Context) error { return nil }
	if e = c.Put(context.Background(), "52pt", token, "new question", []string{"3"}, guard); e != nil {
		t.Fatal(e)
	}
	saved, _ := os.ReadFile(file)
	var document map[string]json.RawMessage
	var future map[string]json.RawMessage
	if json.Unmarshal(saved, &document) != nil || json.Unmarshal(document["future"], &future) != nil || string(future["big"]) != "9007199254740993" || string(future["nil"]) != "null" {
		t.Fatal("unknown raw JSON lost")
	}
	_, token, e = c.Read(context.Background(), "52pt")
	if e != nil {
		t.Fatal(e)
	}
	concurrent := `{"concurrent":true}`
	if e = os.WriteFile(file, []byte(concurrent), 0600); e != nil {
		t.Fatal(e)
	}
	if e = c.Put(context.Background(), "52pt", token, "new", []string{"4"}, guard); !errors.Is(e, ErrScope) {
		t.Fatal("concurrent cache replaced")
	}
	saved, _ = os.ReadFile(file)
	if string(saved) != concurrent {
		t.Fatal("concurrent data changed")
	}
}
func TestInvalidOrLinkedCacheIsNotRebuilt(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "malformed", true: "symlink"}[linked], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "temp", "signin")
			if e := os.MkdirAll(path, 0700); e != nil {
				t.Fatal(e)
			}
			outside := filepath.Join(t.TempDir(), "keep.json")
			if e := os.WriteFile(outside, []byte("keep"), 0600); e != nil {
				t.Fatal(e)
			}
			file := filepath.Join(path, "tjupt.json")
			if linked {
				if e := os.Symlink(outside, file); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := os.WriteFile(file, []byte("{invalid"), 0600); e != nil {
					t.Fatal(e)
				}
			}
			c := &FileCache{Directory: dir}
			if _, _, e := c.Read(context.Background(), "tjupt"); e == nil {
				t.Fatal("invalid cache accepted")
			}
			if e := c.Put(context.Background(), "tjupt", "", "key", []string{"answer"}, func(context.Context) error { return nil }); e == nil {
				t.Fatal("invalid cache replaced")
			}
			keep, _ := os.ReadFile(outside)
			if string(keep) != "keep" {
				t.Fatal("outside data changed")
			}
		})
	}
}
func TestOriginalTenByTenPictureHash(t *testing.T) {
	raw := signinFixtureImage(t)
	hash, e := PictureHash(raw)
	if e != nil || hash != strings.Repeat("0000011111", 10) {
		t.Fatalf("legacy average hash %s %v", hash, e)
	}
}
func TestExplicitEmptySelectionAndUnknownActionsStayEmpty(t *testing.T) {
	settings, e := ParseSettings(`{"sign_sites":[],"special_sites":[],"queue_cnt":"4","future":{"big":9007199254740993}}`)
	if e != nil || len(settings.Sites) != 0 || len(settings.Select(nil, nil, false)) != 0 {
		t.Fatal("empty selection widened")
	}
	for _, state := range []string{"unknown", "stopped", "signed", "already", "authenticated"} {
		if settings.Retry(Outcome{State: state, Retryable: true}) {
			t.Fatalf("non-failure selected: %s", state)
		}
	}
}
