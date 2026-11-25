package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
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
	if result.Data.Title != "同步历史" || !result.Data.ReadOnly || !result.Data.ActionsOmitted || result.Data.CanDeleteRecords || len(result.Data.Tables) != 1 || result.Data.Tables[0].Rows[0][0] != "示例电影" {
		t.Fatalf("unexpected page: %#v", result.Data)
	}
	if len(result.Data.Tables[0].RowActions) != 2 || result.Data.Tables[0].RowActions[0] != nil || result.Data.Tables[0].RowActions[1] != nil {
		t.Fatalf("unexpected row actions: %#v", result.Data.Tables[0].RowActions)
	}
	if len(result.Data.Sections) != 2 || result.Data.Sections[1] != "详情：example.test" {
		t.Fatalf("unexpected sanitized sections: %#v", result.Data.Sections)
	}
}

func TestPluginRecordActionsRequireLocalStorageAndNeverForward(t *testing.T) {
	h, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("forwarded plugin mutation to %s", r.URL.Path)
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for id, record := range map[string]string{
		"DoubanSync": "001", "DoubanRank": "42", "MovieRandom": "9007199254740993",
		"MediaLibraryArchive": "归档_20260919080000.md",
	} {
		body := `{"recordId":"` + record + `","confirmation":"` + record + `"}`
		if w := nativeJSONRequest(h, "DELETE", "/api/v1/plugins/"+id+"/page/records", "fixture", body); w.Code != 503 {
			t.Fatal(id, w.Code, w.Body.String())
		}
		form := url.Values{"id": {id}, "action": {"delete"}, "record_id": {record}, "confirmation": {record}}
		if w := performFormRequest(h, "/api/v1/plugin/page/action", "fixture", form); w.Code != 503 {
			t.Fatal(id, w.Code, w.Body.String())
		}
		if w := performFormRequest(h, "/api/v1/plugin/page/action", "", form); w.Code != 401 {
			t.Fatal(id, w.Code)
		}
	}
	const path = "/api/v1/plugin/page/action"
	if w := nativeJSONRequest(h, "GET", path, "fixture", ""); w.Code != 405 || w.Header().Get("Allow") != "POST" {
		t.Fatal(w.Code, w.Header())
	}
	if w := performFormRequest(h, path, "fixture", url.Values{"id": {"DoubanRank"}, "action": {"run"}}); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestExternalPageInMixedDeploymentIsReadOnlyForAllClients(t *testing.T) {
	var calls int
	h, token, _ := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/plugin/page" || r.FormValue("id") != "ExternalPage" {
			t.Fatalf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
		}
		calls++
		return jsonResponse(r, `{"code":0,"data":{"title":"扩展","sections":["显示记录"],"tables":[{"columns":["名称"],"rows":[["展示"]],"row_actions":[{"type":"delete","record_id":"123"}]}],"html":"untrusted-markup"}}`), nil
	}))
	form := url.Values{"id": {"ExternalPage"}}
	modern := nativeJSONRequest(h, "GET", "/api/v1/plugins/ExternalPage/page", token, "")
	original := performFormRequest(h, "/api/v1/plugin/page", token, form)
	command := nativeJSONRequest(h, "POST", "/do", token, `{"cmd":"get_plugin_page","data":{"id":"ExternalPage"}}`)
	if command.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(command.Body.String(), `"func":null`) {
		t.Fatal(command.Body.String())
	}
	for _, w := range []*httptest.ResponseRecorder{modern, original, command} {
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "展示") || strings.Contains(body, "untrusted-markup") || strings.Contains(body, "123") {
			t.Fatal(w.Code, body)
		}
	}
	if !strings.Contains(modern.Body.String(), `"actionsOmitted":true`) || !strings.Contains(original.Body.String(), `"actions_omitted":true`) {
		t.Fatal("missing read-only status")
	}
	form.Set("action", "delete")
	form.Set("record_id", "123")
	if w := performFormRequest(h, "/api/v1/plugin/page/action", token, form); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if calls != 3 {
		t.Fatal("expected one read per client", calls)
	}
}

func TestHelperHelpAcrossModesAndLegacyClients(t *testing.T) {
	mixed, admin, db := nativeServicesFixture(t, "", nil, nil)
	native, err := newHandler(config.Config{ApplicationConfigPath: filepath.Join(filepath.Dir(db), "config.yaml"), DisableLegacy: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := systemconfig.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const route = "/api/v1/plugins/DownloaderHelper/page"
	if w := nativeJSONRequest(mixed, "GET", route, admin, ""); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := store.Set(t.Context(), "UserInstalledPlugins", `["DownloaderHelper","Customization"]`); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), helperControlKey, `{"Last":"private-job-result"}`); err != nil {
		t.Fatal(err)
	}
	if w := performFormRequest(mixed, "/api/v1/user/manage", admin, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	viewer := loginForTest(t, mixed, "viewer", "strong-password")
	for _, deployment := range []struct {
		h             http.Handler
		showAdminRuns bool
	}{{mixed, false}, {native, true}} {
		for _, actor := range []struct {
			token string
			admin bool
		}{{admin, true}, {viewer, false}} {
			h := deployment.h
			list := nativeJSONRequest(h, "GET", "/api/v1/plugins", actor.token, "")
			var catalog struct{ Data pluginsData }
			if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &catalog) != nil {
				t.Fatal("catalog read failed", list.Code)
			}
			found := false
			for _, item := range catalog.Data.Items {
				if item.ID == helperID {
					found = item.CanViewPage && item.HasPage && (!deployment.showAdminRuns || item.ActionsAvailable == actor.admin)
				}
			}
			if !found {
				t.Fatal("help link and management permissions disagree")
			}
			for _, w := range []*httptest.ResponseRecorder{
				nativeJSONRequest(h, "GET", route, actor.token, ""),
				performFormRequest(h, "/api/v1/plugin/page", actor.token, url.Values{"id": {helperID}}),
				nativeJSONRequest(h, "POST", "/do", actor.token, `{"cmd":"get_plugin_page","data":{"id":"DownloaderHelper"}}`),
			} {
				body := w.Body.String()
				if w.Code != 200 || !strings.Contains(body, "全局配置") || !strings.Contains(body, "自动删种") || strings.Contains(body, "private-job-result") != (actor.admin && deployment.showAdminRuns) {
					t.Fatal(w.Code, body)
				}
			}
		}
	}
	for _, w := range []*httptest.ResponseRecorder{
		nativeJSONRequest(mixed, "GET", "/api/v1/plugins/Customization/page", admin, ""),
		performFormRequest(mixed, "/api/v1/plugin/page", admin, url.Values{"id": {"Customization"}}),
	} {
		if w.Code != 404 {
			t.Fatal(w.Code, w.Body.String())
		}
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

func TestForwardedArchivePageOmitsRemoteActions(t *testing.T) {
	t.Parallel()
	data := normalizePluginPage(map[string]any{
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
	if data.CanDeleteRecords || !data.ActionsOmitted || data.DeleteConfirmation != "" || len(data.Tables) != 1 || len(data.Tables[0].RowActions) != 2 {
		t.Fatalf("unexpected archive page: %#v", data)
	}
	if action := data.Tables[0].RowActions[0]; action != nil {
		t.Fatalf("remote archive action exposed: %#v", action)
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
	data := normalizePluginPage(map[string]any{
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
