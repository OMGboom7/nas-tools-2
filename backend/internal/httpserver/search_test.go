package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestSearchMissingNativeRuntimeNeverUsesConfiguredLegacy(t *testing.T) {
	calls := 0
	h, err := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("Python must not receive a search or account token")
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"keyword":"Movie","quick":true}`, `{"keyword":"Movie","indexers":["Selected-prowlarr"]}`} {
		w := nativeJSONRequest(h, "POST", "/api/v1/search/resources", "private-token", body)
		if w.Code != 503 || strings.Contains(w.Body.String(), "private-token") || calls != 0 {
			t.Fatal(w.Code, w.Body.String(), calls)
		}
	}
	if w := nativeJSONRequest(h, "POST", "/api/v1/search/resources", "", `{"keyword":"Movie"}`); w.Code != 401 || calls != 0 {
		t.Fatal(w.Code, calls)
	}
}

func TestSearchUnconfiguredNativeIndexersReturnsRealEmptyResult(t *testing.T) {
	calls := 0
	h, token, _ := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("no indexers configured")
	}))
	for _, quick := range []string{"false", "true"} {
		w := nativeJSONRequest(h, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":`+quick+`}`)
		var result struct {
			Success bool
			Data    searchData
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Success || result.Data.Total != 0 || len(result.Data.Items) != 0 || calls != 0 {
			t.Fatal(w.Code, w.Body.String(), calls)
		}
	}
}

func TestSearchInvalidRequestsDoNotStartAnyBackend(t *testing.T) {
	h, token, _ := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid search reached backend", r.URL)
		return nil, nil
	}))
	for _, body := range []string{`{}`, `{"keyword":""}`, `{"keyword":"Movie","unknown":true}`, `{"keyword":"Movie"} {"keyword":"Other"}`, `{"keyword":"Movie"} trailing`, `{"keyword":"` + strings.Repeat("x", 121) + `"}`, `{"keyword":"Movie"}` + strings.Repeat(" ", 1<<20)} {
		if w := nativeJSONRequest(h, "POST", "/api/v1/search/resources", token, body); w.Code != 400 {
			t.Fatal("invalid request accepted", w.Code, w.Body.String())
		}
	}
}

func TestSearchInvalidSavedSelectionDoesNotHideErrorWithLegacyResults(t *testing.T) {
	calls := 0
	h, token, path := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, fmt.Errorf("no remote call allowed")
	}))
	s, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Set(t.Context(), "UserIndexerSites", `["Missing-prowlarr"]`); err != nil {
		t.Fatal(err)
	}
	for _, installed := range []string{`[]`, `["Prowlarr"]`, `["unmigrated-provider"]`} {
		if err := s.Set(t.Context(), "UserInstalledPlugins", installed); err != nil {
			t.Fatal(err)
		}
		w := nativeJSONRequest(h, "POST", "/api/v1/search/resources", token, `{"keyword":"Movie","quick":true}`)
		if w.Code != 502 || calls != 0 || strings.Contains(w.Body.String(), "success\":true") {
			t.Fatal(w.Code, w.Body.String(), calls)
		}
	}
}
