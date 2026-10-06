package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestMediaCategoryListReadsOrderedNativeYAML(t *testing.T) {
	handler, token, databasePath := nativeServicesFixture(t, "media:\n  category: custom\n", nil, nil)
	categoryPath := filepath.Join(filepath.Dir(databasePath), "custom.yaml")
	if err := os.WriteFile(categoryPath, []byte("movie:\n  华语电影:\n  外语电影:\ntv:\n  国产剧:\n  未分类:\nanime:\n  国漫:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		kind string
		want []string
	}{
		{"电影", []string{"华语电影", "外语电影"}},
		{"电视剧", []string{"国产剧", "未分类"}},
		{"动漫", []string{"国漫"}},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/media/category/list", strings.NewReader("type="+test.kind))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var payload struct {
			Code     int      `json:"code"`
			Category []string `json:"category"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || response.Code != http.StatusOK || payload.Code != 0 || !sameCategoryNames(payload.Category, test.want) {
			t.Fatalf("kind %s: status=%d body=%s err=%v", test.kind, response.Code, response.Body.String(), err)
		}
	}
}

func TestMediaCategoryListFallbackAndAuthentication(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	templatePath := filepath.Join(directory, "template.yaml")
	if err := os.WriteFile(configPath, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: category-secret\nmedia:\n  category: default-category\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(templatePath, []byte("movie:\n  默认电影:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := newHandler(config.Config{ApplicationConfigPath: configPath, DefaultCategoryPath: templatePath, DisableLegacy: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/media/category/list", strings.NewReader("type=%E7%94%B5%E5%BD%B1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/media/category/list", strings.NewReader("type=%E7%94%B5%E5%BD%B1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", loginForTest(t, handler, "admin", "password"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "默认电影") {
		t.Fatalf("fallback status=%d body=%s", response.Code, response.Body.String())
	}
}

func sameCategoryNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
