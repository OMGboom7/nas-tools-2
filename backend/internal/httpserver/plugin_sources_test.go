package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestPluginSourceInventoryLinksAndUnavailableDirectory(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	plugins := filepath.Join(dir, "plugins")
	if inv := readPluginSourceInventory(config); inv.Incomplete || len(inv.Files) != 0 {
		t.Fatal(inv)
	}
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(config, plugins); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if inv := readPluginSourceInventory(config); !inv.Incomplete {
		t.Fatal("unreadable directory reported as empty")
	}
	if err := os.Remove(plugins); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(plugins, 0700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"linked.py": config, "directory.py": dir, "dangling.py": filepath.Join(dir, "absent")} {
		if err := os.Symlink(target, filepath.Join(plugins, name)); err != nil {
			t.Fatal(err)
		}
	}
	if inv := readPluginSourceInventory(config); inv.Incomplete || !slices.Equal(inv.Files, []string{"linked.py"}) {
		t.Fatal(inv)
	}
}

func TestExternalPluginMutationChecksVisibleClassAndLocalInstallation(t *testing.T) {
	var calls []string
	handler, token, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/plugin/apps":
			return jsonResponse(r, `{"code":0,"result":{"ExternalClass":{"installed":false}}}`), nil
		case "/api/v1/plugin/uninstall":
			return jsonResponse(r, `{"code":0}`), nil
		default:
			t.Fatalf("unexpected legacy request: %s", r.URL.Path)
			return nil, nil
		}
	}))
	store, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["HiddenClass","ExternalClass"]`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
		want         []string
	}{
		{"POST", "/api/v1/plugins/ExternalClass/install", 200, []string{"/api/v1/plugin/apps"}},
		{"DELETE", "/api/v1/plugins/HiddenClass", 404, []string{"/api/v1/plugin/apps"}},
		{"DELETE", "/api/v1/plugins/ExternalClass", 200, []string{"/api/v1/plugin/apps", "/api/v1/plugin/uninstall"}},
	} {
		calls = nil
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tc.status || !slices.Equal(calls, tc.want) {
			t.Fatalf("%s %s: status %d calls %v", tc.method, tc.path, w.Code, calls)
		}
	}
}

func TestExternalSourceSnapshotOnlyReportsObservedClassesForPresentFiles(t *testing.T) {
	inventory := pluginSourceInventory{Files: []string{"one.py", "unknown.py", "bad.py"}}
	inventory.attachClasses(`{"one.py":["PluginB","外部扩展","PluginB","PluginA"],"bad.py":["bad.id"],"absent.py":["Hidden"]}`)
	if !inventory.Incomplete || len(inventory.Classes) != 1 || !slices.Equal(inventory.Classes["one.py"], []string{"PluginA", "PluginB", "外部扩展"}) {
		t.Fatal(inventory)
	}
	older := pluginSourceInventory{Files: []string{"one.py"}}
	older.attachClasses("")
	if older.Incomplete || len(older.Classes) != 0 {
		t.Fatal("unobserved class inferred", older)
	}
}
