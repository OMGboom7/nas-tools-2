package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInitializationFilesPreserveYAML(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"list", "app:\n  init_files: &files\n    - extra.sql # original comment\n  user: kept\nother: *files\n"},
		{"app-alias", "defaults: &base {user: kept, init_files: [extra.sql]} # original comment\napp: *base\n"},
		{"root-merge", "base: &base\n  app: &settings {user: kept, init_files: [extra.sql]} # original comment\n<<: *base\nother: *settings\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.input), 0600); err != nil {
				t.Fatal(err)
			}
			store := NewStore(path)
			for i := 0; i < 2; i++ {
				files, err := store.SyncInitializationFiles("new.sql")
				if err != nil || !slices.Equal(files, []string{"extra.sql", "new.sql"}) {
					t.Fatalf("files %v: %v", files, err)
				}
			}
			snapshot, err := store.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if snapshot["app"].(map[string]any)["user"] != "kept" {
				t.Fatal(snapshot)
			}
			if defaults, ok := snapshot["defaults"].(map[string]any); ok && len(defaults["init_files"].([]any)) != 1 {
				t.Fatal(snapshot)
			}
			if tc.name == "root-merge" && len(snapshot["other"].(map[string]any)["init_files"].([]any)) != 1 {
				t.Fatal(snapshot)
			}
			written, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(written), "# original comment") {
				t.Fatalf("%s %v", written, err)
			}
			original, err := os.ReadFile(path + ".bak")
			if err != nil || string(original) != tc.input {
				t.Fatalf("backup %s: %v", original, err)
			}
		})
	}
}
