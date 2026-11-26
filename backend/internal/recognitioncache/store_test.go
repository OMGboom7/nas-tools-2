package recognitioncache

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Actual Python protocol 2 and 4 dictionaries containing app.utils.types.MediaType.TV.
const oldProtocol2 = "gAJ9cQBYGQAAAFvnlLXop4bliaddRXhhbXBsZS0yMDI0LTBxAX1xAihYAgAAAGlkcQNLZFgFAAAAdGl0bGVxBFgGAAAA5pu05q2jcQVYBAAAAHllYXJxBlgEAAAAMjAyNHEHWAQAAAB0eXBlcQhjYXBwLnV0aWxzLnR5cGVzCk1lZGlhVHlwZQpxCVgJAAAA55S16KeG5YmncQqFcQtScQxYCwAAAHBvc3Rlcl9wYXRocQ1OdXMu"
const oldProtocol4 = "gASVkAAAAAAAAAB9lIwZW+eUteinhuWJp11FeGFtcGxlLTIwMjQtMJR9lCiMAmlklEtkjAV0aXRsZZSMBuabtOato5SMBHllYXKUjAQyMDI0lIwEdHlwZZSMD2FwcC51dGlscy50eXBlc5SMCU1lZGlhVHlwZZSTlIwJ55S16KeG5YmnlIWUUpSMC3Bvc3Rlcl9wYXRolE51cy4="

func TestImportedCorrectionsSurviveConcurrentReadersAndExplicitClear(t *testing.T) {
	for _, fixture := range []string{oldProtocol2, oldProtocol4} {
		dir := t.TempDir()
		path := filepath.Join(dir, "user.db")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		original, _ := base64.StdEncoding.DecodeString(fixture)
		source := filepath.Join(dir, "tmdb.dat")
		const key = "[电视剧]Example-2024-0"
		ctx := context.Background()
		write := func(b []byte) {
			t.Helper()
			if err := os.WriteFile(source, b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		write(original[:len(original)-1])
		if _, err := Lookup(ctx, path, key, false); !errors.Is(err, errLegacy) {
			t.Fatalf("truncated import: %v", err)
		}
		write(original)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e, err := Lookup(ctx, path, key, false)
				if err != nil || e.ID() != "100" || e.Field("type") != "电视剧" || e.Field("title") != "更正" {
					t.Errorf("import %v %v", e, err)
				}
			}()
		}
		wg.Wait()
		run := func(op string, q Request) any {
			t.Helper()
			v, err := Run(ctx, path, op, q, false)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
		run("rename", Request{Key: key, Title: "O'Brien"})
		run("put", Request{Entries: map[string]Entry{key: {"id": 3, "title": "later"}}})
		got := run("list", Request{Page: 1, Size: 1, Search: "EXAMPLE"}).(Page)
		if got.Total != 1 || got.Items[0].Entry.Field("title") != "O'Brien" {
			t.Fatal(got)
		}
		if _, err := Run(ctx, path, "delete", Request{Key: key, Expected: &ExpectedEntry{"100", "old"}}, false); !errors.Is(err, ErrChanged) {
			t.Fatal(err)
		}
		run("clear", Request{})
		if v := run("get", Request{Key: key}); v != nil {
			t.Fatal(v)
		}
		if data, err := os.ReadFile(source); err != nil || string(data) != string(original) {
			t.Fatal("legacy cache changed")
		}
	}
}

func TestRecognitionCacheExpiryAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if e, err := Lookup(ctx, path, "k", false); err != nil || e != nil {
		t.Fatal(e, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ok, err := initialized(ctx, db); err != nil || ok {
		t.Fatal("preview initialized cache", err)
	}
	put := func(entries map[string]Entry) error {
		_, err := Run(ctx, path, "put", Request{Entries: entries}, false)
		return err
	}
	if err := put(map[string]Entry{"first": {"id": 1}, "negative": {"id": 0}}); err != nil {
		t.Fatal(err)
	}
	if err := put(map[string]Entry{"added": {"id": 2}, "invalid": {"id": -1}}); !errors.Is(err, ErrRequest) {
		t.Fatal(err)
	}
	if e, err := Lookup(ctx, path, "added", false); e != nil || err != nil {
		t.Fatal(e, err)
	}
	var pos, neg int64
	if err := db.QueryRow("SELECT MAX(EXPIRES),MIN(EXPIRES) FROM GO_RECOGNITION_CACHE").Scan(&pos, &neg); err != nil || pos-neg != lifetime-negativeLifetime {
		t.Fatal(pos, neg, err)
	}
	if _, err := db.Exec("UPDATE GO_RECOGNITION_CACHE SET EXPIRES=?", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		key           string
		expire, found bool
	}{{"first", false, true}, {"first", true, false}, {"negative", false, false}} {
		e, err := Lookup(ctx, path, test.key, test.expire)
		if err != nil || (e != nil) != test.found {
			t.Fatal(test, e, err)
		}
	}
	if _, err := Run(ctx, path, "delete-id", Request{ID: "1"}, false); err != nil {
		t.Fatal(err)
	}
	if v, err := Run(ctx, path, "get", Request{Key: "first"}, false); v != nil || err != nil {
		t.Fatal(v, err)
	}
}
