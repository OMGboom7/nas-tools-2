package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

// Exercise the actual shared cache, durable manual receipt and cloud client.
// Keep the legacy URL configured in fixture creation to detect accidental proxy
// fallback, then rebuild the native-only runtime for restart assertions.
func nativePikPakSearchFixture(t *testing.T) *manualDownloadFixture {
	t.Helper()
	f := manualHTTPFixture(t, "pikpak", false)
	original := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "indexer.local" {
			return original.RoundTrip(r)
		}
		if r.Header.Get("X-Api-Key") != "private-key" {
			t.Fatal("lost indexer credentials")
		}
		switch r.URL.Path {
		case "/api/v1/indexerstats":
			return jsonResponse(r, `{"indexers":[{"indexerId":42,"indexerName":"Selected"}]}`), nil
		case "/api/v1/search":
			return jsonResponse(r, `[{"indexerId":42,"title":"Movie.2026.1080p.WEB-DL","size":1024,"seeders":3,"guid":"https://tracker.test/details?private=secret","downloadUrl":"`+testMagnet+`"}]`), nil
		default:
			t.Fatal("unexpected indexer request", r.URL)
			return nil, nil
		}
	})
	s, err := systemconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for key, value := range map[string]string{"UserInstalledPlugins": `["Prowlarr"]`, "UserIndexerSites": `["Selected-prowlarr"]`, "plugin.Prowlarr": `{"host":"http://indexer.local","api_key":"private-key"}`} {
		if err := s.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	f.rebuild(t)
	return f
}

func currentSearchResource(t *testing.T, f *manualDownloadFixture) string {
	t.Helper()
	w := nativeJSONRequest(f.handler, "POST", "/api/v1/search/resources", f.token, `{"keyword":"Movie","quick":true}`)
	var result struct{ Data searchData }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.Total != 1 || len(result.Data.Items) != 1 || len(result.Data.Items[0].Resources) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, secret := range []string{"private-key", "private=secret", testMagnet} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("search locator leaked")
		}
	}
	return result.Data.Items[0].Resources[0].ID
}

func submitSearchID(f *manualDownloadFixture, id string) int {
	body, _ := json.Marshal(addResourceRequest{ResourceID: id})
	return nativeJSONRequest(f.handler, "POST", "/api/v1/downloads/resource", f.token, string(body)).Code
}

func TestSearchNativePikPakResourceDownloadAndCacheRestartReuseDurableReceipt(t *testing.T) {
	f := nativePikPakSearchFixture(t)
	id := currentSearchResource(t, f)
	for i := 0; i < 2; i++ {
		if code := submitSearchID(f, id); code != 200 || f.adds != 1 {
			t.Fatal(code, f.adds)
		}
	}
	rows := manualReceipts(t, f)
	if len(rows) != 1 || rows[0].State != "submitted" || rows[0].TaskID != "Task_CaseSensitive" {
		t.Fatal(rows)
	}
	assertManualCommitted(t, f, 1)
	// A fresh search ID cannot bypass the persistent exact hash scope.
	newID := currentSearchResource(t, f)
	if newID == id {
		t.Fatal("search ID reused")
	}
	if code := submitSearchID(f, newID); code != 200 || f.adds != 1 {
		t.Fatal(code, f.adds)
	}
	f.rebuild(t)
	if code := submitSearchID(f, id); code != 404 || f.adds != 1 {
		t.Fatal("restart accepted stale ID or replayed", code, f.adds)
	}
	if code := submitSearchID(f, "42"); code != 409 || f.adds != 1 {
		t.Fatal("legacy ID replayed", code, f.adds)
	}
	if code := submitSearchID(f, currentSearchResource(t, f)); code != 200 || f.adds != 1 {
		t.Fatal("fresh cache after restart replayed", code, f.adds)
	}
	assertManualCommitted(t, f, 1)
}

func TestSearchNativePikPakUncertainResponseCannotReplayAfterNewSearchOrRestart(t *testing.T) {
	f := nativePikPakSearchFixture(t)
	f.lost = true
	if code := submitSearchID(f, currentSearchResource(t, f)); code != 409 || f.adds != 1 {
		t.Fatal(code, f.adds)
	}
	f.rebuild(t)
	if code := submitSearchID(f, currentSearchResource(t, f)); code != 409 || f.adds != 1 {
		t.Fatal("pending scope bypassed", code, f.adds)
	}
	rows := manualReceipts(t, f)
	if len(rows) != 1 || rows[0].State != "pending" {
		t.Fatal(rows)
	}
}
