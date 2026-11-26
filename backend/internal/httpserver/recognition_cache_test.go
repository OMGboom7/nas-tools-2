package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/recognitioncache"
)

func TestCacheAdministrationChangesNativeRecognition(t *testing.T) {
	h, token, db := nativeServicesFixture(t, "", nil, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "tmdb.test" || r.URL.Path != "/3/tv/100" {
			t.Fatalf("unexpected lookup: %s", r.URL)
		}
		return jsonResponse(r, `{"id":100,"name":"Original","first_air_date":"2024-01-01"}`), nil
	}))
	if err := config.NewStore(filepath.Join(filepath.Dir(db), "config.yaml")).Update(map[string]any{"app.rmt_tmdbkey": "fixture", "app.tmdb_domain": "tmdb.test"}); err != nil {
		t.Fatal(err)
	}
	call := func(op, body, auth string, status int) string {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/v1/recognition-cache/"+op, strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != status {
			t.Fatalf("%s got %d %s", op, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	ctx := context.Background()
	key := "[电视剧]Example-2024-0"
	r := nativeRecognition{cacheKey: key, detail: tmdbMediaDetails{ID: 100}, data: map[string]any{"title": "Original", "type": "电视剧", "year": "2024"}}
	if err := r.rememberRSSIdentity(ctx, db); err != nil {
		t.Fatal(err)
	}
	call("list", `{"page":1}`, "", 401)
	if list := call("list", `{"page":1,"search":"EXAMPLE"}`, token, 200); !strings.Contains(list, key) {
		t.Fatal(list)
	}
	edit := `{"key":"[电视剧]Example-2024-0","title":"剧集修订","expected":{"id":"100","title":"Original"}}`
	call("rename", edit, token, 200)
	call("delete", edit, token, 409)
	resp := performFormRequest(h, "/api/v1/service/name/test", token, url.Values{"name": {"Example.2024.S00E01"}})
	var got struct{ Data map[string]any }
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil || got.Data["title"] != "剧集修订" || got.Data["season_episode"] != "S00E01" {
		t.Fatalf("%d %s", resp.Code, resp.Body.String())
	}
	call("clear", "{}", token, 400)
	call("clear", `{"confirm":true}`, token, 200)
	if e, err := recognitioncache.Lookup(ctx, db, key, false); e != nil || err != nil {
		t.Fatal(e, err)
	}
}
