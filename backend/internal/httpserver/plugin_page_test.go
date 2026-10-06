package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

func TestPluginPageReturnsOnlyNormalizedStructure(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/plugin/page" {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
		payload, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(payload))
		if form.Get("id") != "DoubanSync" {
			t.Fatalf("unexpected id: %#v", form)
		}
		return jsonResponse(request, `{"code":0,"success":true,"data":{"title":"同步历史","sections":["最近 7 天的同步结果","详情：https://user:password@example.test/private?token=hidden"],"tables":[{"columns":["标题","状态"],"rows":[["示例电影","已下载"],["伪造记录","失败"]],"row_actions":[{"type":"delete","record_id":"12345"},{"type":"delete","record_id":"../../etc"}]}],"actions_omitted":true,"html":"<script>alert('unsafe')</script>","func":"dangerous()","config":{"token":"page-secret"}}}`), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/DoubanSync/page", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, unsafe := range []string{"unsafe", "dangerous", "page-secret", "config", "html", "func", "user:password", "/private", "token=hidden"} {
		if strings.Contains(response.Body.String(), unsafe) {
			t.Fatalf("unsafe value %q leaked: %s", unsafe, response.Body.String())
		}
	}
	var result struct {
		Data pluginPageData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Data.Title != "同步历史" || !result.Data.ReadOnly || !result.Data.ActionsOmitted || !result.Data.CanDeleteRecords || len(result.Data.Tables) != 1 || result.Data.Tables[0].Rows[0][0] != "示例电影" {
		t.Fatalf("unexpected page: %#v", result.Data)
	}
	if len(result.Data.Tables[0].RowActions) != 2 || result.Data.Tables[0].RowActions[0] == nil || result.Data.Tables[0].RowActions[0].RecordID != "12345" || result.Data.Tables[0].RowActions[1] != nil {
		t.Fatalf("unexpected row actions: %#v", result.Data.Tables[0].RowActions)
	}
	if len(result.Data.Sections) != 2 || result.Data.Sections[1] != "详情：example.test" {
		t.Fatalf("unexpected sanitized sections: %#v", result.Data.Sections)
	}
}

func TestPluginPageRecordDeleteUsesWhitelistedAction(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/v1/plugin/page/action" {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
		payload, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(payload))
		if form.Get("id") != "DoubanSync" || form.Get("action") != "delete" || form.Get("record_id") != "12345" {
			t.Fatalf("unexpected action: %#v", form)
		}
		return jsonResponse(request, `{"code":0,"success":true,"message":"扩展页记录已删除"}`), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/DoubanSync/page/records", strings.NewReader(`{"recordId":"12345"}`))
	request.Header.Set("Authorization", "test-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "扩展页记录已删除") {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestPluginArchiveDeleteRequiresExactFilenameConfirmation(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		payload, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(payload))
		filename := "归档_20260919080000.md"
		if request.URL.Path != "/api/v1/plugin/page/action" || form.Get("id") != "MediaLibraryArchive" || form.Get("action") != "delete" || form.Get("record_id") != filename || form.Get("confirmation") != filename {
			t.Fatalf("unexpected archive action: path=%s form=%#v", request.URL.Path, form)
		}
		return jsonResponse(request, `{"code":0,"success":true,"message":"归档文件已删除"}`), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	filename := "归档_20260919080000.md"

	mismatch := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/MediaLibraryArchive/page/records", strings.NewReader(`{"recordId":"`+filename+`","confirmation":"错误名称"}`))
	mismatch.Header.Set("Authorization", "test-token")
	mismatchResponse := httptest.NewRecorder()
	handler.ServeHTTP(mismatchResponse, mismatch)
	if mismatchResponse.Code != http.StatusBadRequest || requests != 0 {
		t.Fatalf("mismatch status = %d, requests = %d: %s", mismatchResponse.Code, requests, mismatchResponse.Body.String())
	}

	confirmed := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/MediaLibraryArchive/page/records", strings.NewReader(`{"recordId":"`+filename+`","confirmation":"`+filename+`"}`))
	confirmed.Header.Set("Authorization", "test-token")
	confirmedResponse := httptest.NewRecorder()
	handler.ServeHTTP(confirmedResponse, confirmed)
	if confirmedResponse.Code != http.StatusOK || requests != 1 || !strings.Contains(confirmedResponse.Body.String(), "归档文件已删除") {
		t.Fatalf("confirmed status = %d, requests = %d: %s", confirmedResponse.Code, requests, confirmedResponse.Body.String())
	}
}

func TestPluginPageRecordDeleteRejectsUnsupportedOrInvalidRecord(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return jsonResponse(request, `{"code":0}`), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)

	unsupported := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/AutoSignin/page/records", strings.NewReader(`{"recordId":"12345"}`))
	unsupported.Header.Set("Authorization", "test-token")
	unsupportedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unsupportedResponse, unsupported)
	if unsupportedResponse.Code != http.StatusNotFound {
		t.Fatalf("unsupported status = %d: %s", unsupportedResponse.Code, unsupportedResponse.Body.String())
	}

	invalid := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/MovieRandom/page/records", strings.NewReader(`{"recordId":"../../etc"}`))
	invalid.Header.Set("Authorization", "test-token")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest || requests != 0 {
		t.Fatalf("invalid status = %d, requests = %d: %s", invalidResponse.Code, requests, invalidResponse.Body.String())
	}
}

func TestPluginArchivePageOnlyAcceptsExactArchiveFilenames(t *testing.T) {
	t.Parallel()
	data := normalizePluginPage("MediaLibraryArchive", map[string]any{
		"title": "归档记录",
		"tables": []any{map[string]any{
			"columns": []any{"归档名称", "归档大小"},
			"rows": []any{
				[]any{"归档_20260919080000.md", "12 KB"},
				[]any{"../配置文件", "1 KB"},
			},
			"row_actions": []any{
				map[string]any{"type": "delete", "record_id": "归档_20260919080000.md"},
				map[string]any{"type": "delete", "record_id": "../config.yaml"},
			},
		}},
	})
	if !data.CanDeleteRecords || data.DeleteConfirmation != "typeRecordId" || len(data.Tables) != 1 || len(data.Tables[0].RowActions) != 2 {
		t.Fatalf("unexpected archive page: %#v", data)
	}
	if action := data.Tables[0].RowActions[0]; action == nil || action.RecordID != "归档_20260919080000.md" || action.Confirmation != "typeRecordId" {
		t.Fatalf("valid archive action missing: %#v", action)
	}
	if data.Tables[0].RowActions[1] != nil {
		t.Fatalf("forged archive action accepted: %#v", data.Tables[0].RowActions[1])
	}
}

func TestPluginPageMapsNotFoundAndRejectsInvalidID(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return jsonResponse(request, `{"code":404,"success":false,"message":"插件扩展页不存在"}`), nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)

	invalid := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/bad.id/page", nil)
	invalid.Header.Set("Authorization", "test-token")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest || requests != 0 {
		t.Fatalf("invalid status = %d, requests = %d", invalidResponse.Code, requests)
	}

	missing := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/NoPage/page", nil)
	missing.Header.Set("Authorization", "test-token")
	missingResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingResponse, missing)
	if missingResponse.Code != http.StatusNotFound || requests != 1 {
		t.Fatalf("missing status = %d, requests = %d: %s", missingResponse.Code, requests, missingResponse.Body.String())
	}
}

func TestPluginPageIgnoresForgedActionsForReadOnlyPlugin(t *testing.T) {
	t.Parallel()
	data := normalizePluginPage("AutoSignin", map[string]any{
		"title": "签到记录",
		"tables": []any{map[string]any{
			"columns":     []any{"站点", "结果"},
			"rows":        []any{[]any{"示例站点", "成功"}},
			"row_actions": []any{map[string]any{"type": "delete", "record_id": "12345"}},
		}},
	})
	if data.CanDeleteRecords || len(data.Tables) != 1 || len(data.Tables[0].RowActions) != 1 || data.Tables[0].RowActions[0] != nil {
		t.Fatalf("forged action was accepted: %#v", data)
	}
}
