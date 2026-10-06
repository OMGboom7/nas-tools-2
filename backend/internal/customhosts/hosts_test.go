package customhosts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseReportsBadMappingsAndPreservesValidAliases(t *testing.T) {
	valid, invalid, err := Parse(t.Context(), "# comment\n127.0.0.1 localhost local.test # local\n2001:db8::1 ipv6.test\n999.1.1.1 bad\n1.2.3.4\n1.2.3.4 -bad\nfe80::1%eth0 scoped\n")
	if err != nil || len(valid) != 2 || len(invalid) != 4 || valid[0] != "127.0.0.1\tlocalhost local.test" || invalid[0].Line != 4 {
		t.Fatalf("valid=%v invalid=%v err=%v", valid, invalid, err)
	}
	if _, _, err := Parse(t.Context(), strings.Repeat("x", 65537)); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
}

func TestRewriteManagedAndLegacyBlocks(t *testing.T) {
	original := "127.0.0.1 localhost\n# user note\n"
	first, legacy, err := Rewrite(original, []string{"1.2.3.4\tfirst.test"})
	if err != nil || legacy || !strings.HasPrefix(first, original) {
		t.Fatal(first, legacy, err)
	}
	first += "9.8.7.6 user-added.test\n"
	second, legacy, err := Rewrite(first, []string{"4.3.2.1\tsecond.test"})
	if err != nil || legacy || strings.Contains(second, "first.test") || !strings.HasSuffix(second, "9.8.7.6 user-added.test\n") {
		t.Fatal(second, legacy, err)
	}
	converted, legacy, err := Rewrite(original+BeginMarker+"\n1.1.1.1 old.test\n", []string{"2.2.2.2\tnew.test"})
	if err != nil || !legacy || strings.Contains(converted, "old.test") || !strings.Contains(converted, EndMarker) {
		t.Fatal(converted, legacy, err)
	}
	for _, body := range []string{EndMarker + "\n", BeginMarker + "\n" + BeginMarker + "\n", BeginMarker + "\n" + EndMarker + "\n" + EndMarker + "\n"} {
		if _, _, err := Rewrite(body, []string{"1.2.3.4\texample.test"}); !errors.Is(err, ErrMarkers) {
			t.Fatal("ambiguous markers accepted", err)
		}
	}
}

func TestApplyUsesExistingFileAndRetainsInodeAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	original := "127.0.0.1 localhost\n"
	if err := os.WriteFile(path, []byte(original), 0640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	result, err := Apply(t.Context(), path, "1.2.3.4 first.test\ninvalid second.test")
	if err != nil || !result.Applied || len(result.Invalid) != 1 {
		t.Fatal(result, err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatal("bind-mounted target inode/mode changed")
	}
	result, err = Apply(t.Context(), path, "5.6.7.8 next.test")
	contents, _ := os.ReadFile(path)
	if err != nil || !result.Applied || strings.Contains(string(contents), "first.test") || !strings.HasPrefix(string(contents), original) {
		t.Fatal(result, err, string(contents))
	}
	result, err = Apply(t.Context(), path, "invalid")
	unchanged, _ := os.ReadFile(path)
	if err != nil || result.Applied || string(unchanged) != string(contents) {
		t.Fatal("invalid-only input modified file")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Apply(ctx, path, "1.2.3.4 cancelled.test"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(t.Context(), link, "1.2.3.4 forbidden.test"); !errors.Is(err, ErrFile) {
		t.Fatal(err)
	}
	missing := filepath.Join(filepath.Dir(path), "missing")
	if _, err := Apply(t.Context(), missing, "1.2.3.4 missing.test"); err == nil {
		t.Fatal("missing target was created")
	}
}
