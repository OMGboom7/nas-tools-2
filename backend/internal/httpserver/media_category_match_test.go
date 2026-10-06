package httpserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"gopkg.in/yaml.v3"
)

func TestCategoryMatcherUsesOrderedTMDBConditions(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(`movie:
  Both:
    genre_ids: '16,99'
    original_language: 'zh,cn'
  Country:
    production_countries: 'CN,TW'
  Fallback:
tv:
  Ended:
    status: 'Ended,Canceled'
    origin_country: 'JP,KR'
anime:
  Animation:
    genre_ids: '16'
`), &root); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		kind string
		info map[string]any
		want string
	}{
		{"movie", map[string]any{"genre_ids": []int{99}, "original_language": "ZH", "production_countries": []map[string]any{{"iso_3166_1": "CN"}}}, "Both"},
		{"movie", map[string]any{"genre_ids": []int{99}, "original_language": "en", "production_countries": []map[string]any{{"iso_3166_1": "cn"}}}, "Country"},
		{"movie", map[string]any{"genre_ids": []int{18}}, "Fallback"},
		{"anime", map[string]any{"genres": []any{map[string]any{"id": float64(16)}}}, "Animation"},
		{"tv", map[string]any{"status": "Ended", "origin_country": []string{"JP"}}, "Ended"},
		{"tv", map[string]any{"status": "Returning Series", "origin_country": []string{"JP"}}, ""},
		{"movie", nil, ""},
	} {
		got, err := matchMediaCategory(root, test.kind, test.info)
		if err != nil || got != test.want {
			t.Errorf("%s %+v => %q err=%v", test.kind, test.info, got, err)
		}
	}
}

func TestDefaultCategoryTemplateMatchesLegacyORNotCommentedAND(t *testing.T) {
	contents, err := os.ReadFile("../../../config/default-category.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(contents, &root); err != nil {
		t.Fatal(err)
	}
	// Despite the template comment, the old implementation ORs comma values.
	for _, test := range []struct {
		kind string
		info map[string]any
		want string
	}{
		{"movie", map[string]any{"genre_ids": []int{99}}, "演唱会"},
		{"movie", map[string]any{"genre_ids": []int{16}}, "动画电影"},
		{"movie", map[string]any{"original_language": "zh"}, "华语电影"},
		{"movie", map[string]any{"original_language": "en"}, "外语电影"},
		{"tv", map[string]any{"origin_country": []string{"US"}}, "欧美剧"},
		{"anime", map[string]any{"genre_ids": []int{16}, "status": "Ended"}, "完结动漫"},
	} {
		got, err := matchMediaCategory(root, test.kind, test.info)
		if err != nil || got != test.want {
			t.Fatalf("%+v => %q err=%v", test.info, got, err)
		}
	}
}

func TestCategoryMatcherReloadsFilesAndRejectsMalformedConditions(t *testing.T) {
	directory := t.TempDir()
	configPath, categoryPath := filepath.Join(directory, "config.yaml"), filepath.Join(directory, "custom.yaml")
	if err := os.WriteFile(configPath, []byte("media:\n  category: custom\n"), 0600); err != nil {
		t.Fatal(err)
	}
	api := mediaCategoryAPI{config: config.NewStore(configPath), configPath: configPath}
	for _, name := range []string{"First", "Updated"} {
		if err := os.WriteFile(categoryPath, []byte("movie:\n  "+name+":\n    original_language: 'zh'\n"), 0600); err != nil {
			t.Fatal(err)
		}
		got, err := api.match("movie", map[string]any{"original_language": "zh"})
		if err != nil || got != name {
			t.Fatalf("match=%q err=%v", got, err)
		}
	}
	if err := os.WriteFile(categoryPath, []byte("movie:\n  Invalid:\n    genre_ids: [16]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := api.match("movie", map[string]any{"genre_ids": []int{16}}); err == nil {
		t.Fatal("invalid rule silently accepted")
	}
}

func TestCategoryAliasesPreserveMatching(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte("rule: &rule\n  original_language: &language 'zh'\nmovie:\n  Shared: *rule\ntv:\n  Shared:\n    original_language: *language\n"), &root); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"movie", "tv"} {
		got, err := matchMediaCategory(root, kind, map[string]any{"original_language": "zh"})
		if err != nil || got != "Shared" {
			t.Fatalf("alias %s=%q err=%v", kind, got, err)
		}
	}
}
