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

func TestSearchAggregatesAndNormalizesLegacyResults(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "test-token" {
			t.Errorf("Authorization = %q, want test-token", request.Header.Get("Authorization"))
		}
		var body string
		switch request.URL.Path {
		case "/api/v1/search/keyword":
			payload, _ := io.ReadAll(request.Body)
			form, _ := url.ParseQuery(string(payload))
			if form.Get("search_word") != "测试电影" {
				t.Errorf("search_word = %q", form.Get("search_word"))
			}
			body = `{"code":0}`
		case "/api/v1/search/result":
			body = `{
                  "code":0,
                  "success":true,
                  "data":{"total":1,
                  "result":{
                    "测试电影 (2024)":{
                      "key":9,"title":"测试电影","year":"2024","type":"电影","vote":"8.6",
                      "tmdbid":"100","poster":"https://media.local/poster.jpg","overview":"测试简介","fav":2,
                      "torrent_dict":[["MOV",{"group":{"group_torrents":{"unique":{"torrent_list":[{
                        "id":42,"torrent_name":"Test.Movie.2024.2160p","description":"测试资源","site":"DemoPT",
                        "pageurl":"https://site.local/details/42","size":"18.2 GB","seeders":36,
                        "respix":"4K","restype":"BluRay","reseffect":"HDR","releasegroup":"DEMO",
                        "video_encode":"H265","labels":["中字"],"uploadvalue":1,"downloadvalue":0
                      }]}}}}]]
                    }
                  }}
                }`
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})

	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/search/resources", strings.NewReader(`{"keyword":"测试电影"}`))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var result struct {
		Success bool       `json:"success"`
		Data    searchData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !result.Success || result.Data.Total != 1 || len(result.Data.Items) != 1 {
		t.Fatalf("unexpected search response: %#v", result)
	}
	media := result.Data.Items[0]
	if !media.Exists || media.Poster == "" || len(media.Resources) != 1 {
		t.Fatalf("unexpected normalized media: %#v", media)
	}
	resource := media.Resources[0]
	if resource.Resolution != "4K" || resource.DownloadFactor != 0 || resource.Seeders != 36 {
		t.Fatalf("unexpected resource: %#v", resource)
	}
}

func TestSearchReturnsEmptyResultForLegacyNoMatch(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"未搜索到任何资源"}`)),
			Request:    request,
		}, nil
	})
	handler, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/search/resources", strings.NewReader(`{"keyword":"不存在"}`))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var result struct {
		Data searchData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Data.Items) != 0 {
		t.Fatalf("items = %d, want 0", len(result.Data.Items))
	}
}
