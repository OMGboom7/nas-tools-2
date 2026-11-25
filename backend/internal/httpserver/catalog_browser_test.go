package httpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func TestRenderedCatalogCommandUsesSavedSiteAndInstalledRules(t *testing.T) {
	dir := t.TempDir()
	configPath, catalogPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "catalog.bin")
	if err := os.WriteFile(configPath, []byte("app: {user_agent: fixture-agent}\npt: {site_search_result_num: 1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if r.URL.Query().Get("q") != "Film & 中文" || r.Header.Get("Cookie") != "saved=fixture" || r.UserAgent() != "fixture-agent" {
			t.Error("unexpected browser query or session")
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<main></main><script>document.cookie="rotated=fixture; path=/";document.querySelector("main").innerHTML='<article><h2>JS result</h2></article><article><h2>Other</h2></article>';</script>`))
	}))
	defer upstream.Close()
	d := indexercatalog.Definition{ID: "custom", Name: "Fixture", Domain: upstream.URL, Parser: "RenderSpider", Search: json.RawMessage(`{"paths":[{"path":"search"}],"params":{"q":"{keyword}"}}`), Torrents: json.RawMessage(`{"list":{"selector":"article"},"fields":{"title":{"selector":"h2"}}}`)}
	encoded, _ := json.Marshal(indexercatalog.Catalog{Indexers: []indexercatalog.Definition{d}})
	if err := os.WriteFile(catalogPath, []byte(base64.StdEncoding.EncodeToString(encoded)), 0600); err != nil {
		t.Fatal(err)
	}
	sites, err := siteconfig.Open(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sites.Close()
	site, err := sites.Upsert(t.Context(), siteconfig.Site{Name: "Fixture", SignURL: upstream.URL + "/login", Cookie: "saved=fixture"})
	if err != nil {
		t.Fatal(err)
	}
	query := map[string]any{"id": d.ID, "domain": d.Domain, "siteID": site.ID, "keyword": "Film & 中文"}
	run := func() (map[string]any, error) {
		data, _ := json.Marshal(query)
		var out bytes.Buffer
		err := RenderCatalogCommand(t.Context(), configPath, catalogPath, bytes.NewReader(data), &out)
		if err != nil {
			if out.Len() != 0 {
				t.Fatal("partial private result")
			}
			return nil, err
		}
		var res map[string]any
		err = json.Unmarshal(out.Bytes(), &res)
		return res, err
	}
	for key, value := range map[string]any{"id": "missing", "domain": "https://foreign.invalid", "siteID": 0, "script": "arbitrary"} {
		old, ok := query[key]
		query[key] = value
		if _, err = run(); err == nil || hits.Load() != 0 {
			t.Fatal(key, err)
		}
		if ok {
			query[key] = old
		} else {
			delete(query, key)
		}
	}
	t.Setenv("NASTOOL_BROWSER_BINARY", filepath.Join(dir, "missing-browser"))
	if _, err = run(); err == nil || hits.Load() != 0 {
		t.Fatal("unavailable browser")
	}
	if binary := os.Getenv("NASTOOL_TEST_BROWSER_BINARY"); binary != "" {
		t.Setenv("NASTOOL_BROWSER_BINARY", binary)
		result, err := run()
		if err != nil {
			t.Fatal(err)
		}
		rows := result["rows"].([]any)
		if len(rows) != 1 || rows[0].(map[string]any)["title"] != "JS result" || result["ua"] != "fixture-agent" || !strings.Contains(result["cookie"].(string), "rotated=fixture") || hits.Load() != 1 {
			t.Fatal("invalid rendered response")
		}
		saved, err := sites.Get(t.Context(), site.ID)
		if err != nil || saved.Cookie != "saved=fixture" {
			t.Fatal("changed stored cookie", err)
		}
	}
}
