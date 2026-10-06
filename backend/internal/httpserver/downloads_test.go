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

func TestDownloadsListUsesLegacyOnlyForActiveWhenNativeStoresUnavailable(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "test-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		payload, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(payload))
		var body string
		switch request.URL.Path {
		case "/api/v1/download/now":
			if form.Get("force_list") != "true" {
				t.Errorf("force_list = %q", form.Get("force_list"))
			}
			body = `{"code":0,"success":true,"data":{"result":[{"id":"hash-1","title":"测试电影 (2024)","progress":48.5,"speed":"3.2 MB/s","state":"Downloading","site_url":"https://site.local/1","image":"https://media.local/poster.jpg"}]}}`
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		return jsonResponse(request, body), nil
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/downloads?historyPage=2", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Success bool          `json:"success"`
		Data    downloadsData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !result.Success || len(result.Data.Active) != 1 || len(result.Data.History) != 0 || len(result.Data.Warnings) != 1 || result.Data.Warnings[0] != "history unavailable" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Data.Active[0].Progress != 48.5 || !result.Data.Active[0].CanControl || !result.Data.Active[0].ShowProgress {
		t.Fatalf("unexpected active task: %#v", result.Data.Active[0])
	}
	if result.Data.Active[0].Image == "https://media.local/poster.jpg" {
		t.Fatalf("images were not rewritten: %#v", result.Data)
	}
}

func TestDownloadMutationsForwardValidatedForms(t *testing.T) {
	t.Parallel()
	paths := make([]string, 0, 2)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		payload, _ := io.ReadAll(request.Body)
		form, _ := url.ParseQuery(string(payload))
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/api/v1/download/search":
			if form.Get("id") != "42" || form.Get("dir") != "/media" || form.Get("setting") != "3" {
				t.Errorf("unexpected resource form: %v", form)
			}
		case "/api/v1/download/stop":
			if form.Get("id") != "hash-1" {
				t.Errorf("unexpected action form: %v", form)
			}
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		return jsonResponse(request, `{"code":0,"success":true,"message":"ok","data":{}}`), nil
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	for _, item := range []struct {
		path string
		body string
	}{
		{path: "/api/v1/downloads/resource", body: `{"resourceId":"42","directory":"/media","setting":"3"}`},
		{path: "/api/v1/downloads/hash-1/stop", body: `{}`},
	} {
		request := httptest.NewRequest(http.MethodPost, item.path, strings.NewReader(item.body))
		request.Header.Set("Authorization", "test-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", item.path, response.Code, response.Body.String())
		}
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v", paths)
	}
}

func jsonResponse(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
