package filterconfig

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharingLegacyRoundTripAndAtomicImport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenWritable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	encode := func(value string) string { return base64.StdEncoding.EncodeToString([]byte(value)) }
	legacy := `{"name":"高清","rules":[{"name":"WEB","pri":"1","include":"1080p\nWEB","exclude":null,"size":null,"free":"1.0 0.0"},{"name":"Backup","pri":2,"include":"720p"}]}`
	id, err := store.Import(ctx, encode(legacy))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := store.Export(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(shared)
	if err != nil {
		t.Fatal(err)
	}
	var group sharedGroup
	if err := json.Unmarshal(decoded, &group); err != nil || group.Name != "高清" || len(group.Rules) != 2 || group.Rules[0].Include != "1080p\nWEB" || group.Rules[1].Priority != "2" {
		t.Fatalf("export = %s, %v", decoded, err)
	}
	if err := store.SetDefault(ctx, id); err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.Import(ctx, shared)
	if err != nil || duplicate != id {
		t.Fatalf("same-name import = %d, %v", duplicate, err)
	}
	groups, err := store.Groups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Default {
		t.Fatalf("same-name default = %+v, %v", groups, err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM CONFIG_FILTER_RULES").Scan(&count); err != nil || count != 4 {
		t.Fatalf("append count = %d, %v", count, err)
	}
	for _, invalid := range []string{
		"bad-base64", encode(`null`), encode(`{"name":" "}`),
		encode(`{"name":"invalid","rules":[{"name":"first","pri":1},{"name":"invalid","pri":{}}]}`),
		encode(`{"name":"invalid","rules":[{"name":"bad","pri":true}]}`),
		encode(`{"name":"invalid","rules":[{"name":"bad","pri":"1.5"}]}`),
		strings.Repeat("A", base64.StdEncoding.EncodedLen(maxShareBytes)+1),
	} {
		if _, err := store.Import(ctx, invalid); !errors.Is(err, ErrInvalidShare) {
			t.Fatalf("malformed import error = %v", err)
		}
	}
	if _, err := db.Exec("CREATE TRIGGER fail_rule_insert BEFORE INSERT ON CONFIG_FILTER_RULES WHEN NEW.ROLE_NAME='fail' BEGIN SELECT RAISE(ABORT, 'fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"New rollback", "高清"} {
		payload := sharedGroup{Name: name, Rules: []sharedRule{{Name: "first", Priority: "1"}, {Name: "fail", Priority: "2"}}}
		bytes, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Import(ctx, encode(string(bytes))); err == nil {
			t.Fatal("expected import failure")
		}
	}
	groups, err = store.Groups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("partial group persisted: %+v, %v", groups, err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM CONFIG_FILTER_RULES").Scan(&count); err != nil || count != 4 {
		t.Fatalf("partial rules persisted: %d, %v", count, err)
	}
	emptyID, err := store.AddGroup(ctx, "Empty", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Export(ctx, emptyID); !errors.Is(err, ErrEmptyGroup) {
		t.Fatalf("empty export: %v", err)
	}
	if _, err := store.Export(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing export: %v", err)
	}
}
