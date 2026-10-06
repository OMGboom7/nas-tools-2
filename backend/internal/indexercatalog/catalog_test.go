package indexercatalog

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestBundledCatalogIsReadableWithoutPython(t *testing.T) {
	catalog, err := Load("../../../web/backend/user.sites.bin")
	if err != nil || len(catalog.Indexers) != 113 {
		t.Fatalf("bundled catalog: count=%d err=%v", len(catalog.Indexers), err)
	}
	if catalog.Indexers[0].ID != "zhuzhu" {
		t.Fatalf("catalog ID changed: %s", catalog.Indexers[0].ID)
	}
	if !catalog.HasTrackerRule("https://www.piggo.me/details/1", "FREE") || !catalog.HasTrackerRule("https://piggo.me", "2XFREE") || catalog.HasTrackerRule("https://piggo.me", "HR") {
		t.Fatal("bundled tracker capabilities were not preserved")
	}
}

func TestSelectionPreservesIDsAndFiltersCredentialsAndPublicSites(t *testing.T) {
	catalog := Catalog{Indexers: []Definition{
		{ID: "private", Name: "Original", Domain: "https://www.tracker.example/"},
		{ID: "public", Name: "Public", Domain: "https://public.example/", Public: true},
		{ID: "unselected", Name: "Other", Domain: "https://other.example/", Public: true},
	}}
	sites := []siteconfig.Site{
		{Name: "No cookie", SignURL: "https://tracker.example"},
		{Name: "Custom name", RSSURL: "https://tracker.example/rss?passkey=secret", Cookie: "secret"},
		{Name: "Duplicate", SignURL: "https://www.tracker.example", Cookie: "secret"},
		{Name: "Unknown", SignURL: "https://unknown.example", Cookie: "secret"},
	}
	items := catalog.Selected(sites, []string{"private", "public"}, true)
	if len(items) != 2 || items[0].ID != "private" || items[0].Name != "Custom name" || items[0].Domain != "tracker.example" || !items[1].Public {
		t.Fatalf("selected = %+v", items)
	}
	if items := catalog.Selected(sites, nil, true); len(items) != 0 {
		t.Fatalf("unselected = %+v", items)
	}
	if items := catalog.Selected(sites, []string{"private", "public"}, false); len(items) != 1 {
		t.Fatalf("public disabled = %+v", items)
	}
}

func TestInvalidCatalogIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sites.bin")
	for _, content := range []string{"null", `{}`, `{"indexer":[{"id":"bad","name":"Bad","domain":"https://user:secret@example.com"}]}`, `{"indexer":{}}`} {
		if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString([]byte(content))), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid catalog %s", content)
		}
	}
}
