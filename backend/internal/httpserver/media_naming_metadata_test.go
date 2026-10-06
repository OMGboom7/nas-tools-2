package httpserver

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

func TestNativeNamingMetadataUsesVerifiedEnglishAndExternalIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  tmdb_domain: tmdb.test\n  rmt_tmdbkey: secret\nmedia:\n  tv_path: "+root+"\n  tv_name_format: '{en_title} ({year})/{imdbid}/Season {season:0>2}/unused'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	invalidID, noEnglish, failEnglish := false, false, false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "tmdb.test" || request.URL.Path != "/3/tv/100" || request.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected request=%s", request.URL.Path)
		}
		if request.URL.Query().Get("api_key") != "secret" || request.URL.Query().Get("append_to_response") != "external_ids" {
			t.Fatal("missing protected TMDB query")
		}
		if invalidID {
			return jsonResponse(request, `{"id":100,"name":"中文剧","external_ids":{"id":999,"imdb_id":"tt100"}}`), nil
		}
		if request.URL.Query().Get("language") == "en" {
			if failEnglish {
				response := jsonResponse(request, `{"secret":"never expose"}`)
				response.StatusCode = 500
				return response, nil
			}
			if noEnglish {
				return jsonResponse(request, `{"id":100,"name":"","external_ids":{"id":100,"imdb_id":"tt100"}}`), nil
			}
			return jsonResponse(request, `{"id":100,"name":"English Show","external_ids":{"id":100,"imdb_id":"tt100"}}`), nil
		}
		return jsonResponse(request, `{"id":100,"name":"中文剧","first_air_date":"2025-01-01","external_ids":{"id":100,"imdb_id":"tt100"},"seasons":[{"season_number":1,"episode_count":2}]}`), nil
	})
	store := config.NewStore(path)
	service := subscriptionService{configStore: store, client: &http.Client{Transport: transport}}
	detail, err := fetchNativeTMDBDetails(t.Context(), store, transport, "tv", "100")
	if err != nil || detail.Attributes["imdb_id"] != "tt100" {
		t.Fatalf("external identity=%v err=%v", detail.Attributes["imdb_id"], err)
	}
	directory := filepath.Join(root, "English Show (2025)", "tt100", "Season 01")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "中文剧.S01E01-E02.mkv"), []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	meta, _ := mediameta.Parse("中文剧.S01", "")
	coverage, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv")
	if err != nil || !coverage.Complete || calls != 2 {
		t.Fatalf("English/IMDb directory coverage=%+v err=%v calls=%d", coverage, err, calls)
	}
	if _, populated := detail.Attributes["en_title"]; populated || detail.EnglishTitle != nil {
		t.Fatal("localized detail was mutated")
	}
	for _, bad := range []string{"empty", "failure", "identity"} {
		noEnglish, failEnglish, invalidID = bad == "empty", bad == "failure", bad == "identity"
		if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv"); err == nil {
			t.Fatalf("%s English metadata accepted as absent media", bad)
		}
	}
	invalidID, noEnglish, failEnglish = false, false, false
	before := calls
	if err := store.Update(map[string]any{"media.tv_name_format": "{title} ({year})/Season {season}/file-{en_title}"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv"); err != nil || calls != before {
		t.Fatalf("filename-only English field made unnecessary request: %v calls=%d", err, calls-before)
	}
	if err := store.Update(map[string]any{"media.tv_name_format": "{{en_title}}/Season {season}/unused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv"); err != nil || calls != before {
		t.Fatalf("escaped field made unnecessary request: %v", err)
	}
}

func TestTMDBExternalIdentityNullAndOmissionRemainDistinct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  rmt_tmdbkey: secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		body            string
		known, rejected bool
	}{
		{`{"id":100,"external_ids":{"id":100,"imdb_id":null}}`, true, false},
		{`{"id":100,"external_ids":{"id":100}}`, false, false},
		{`{"id":100,"external_ids":{"id":101,"imdb_id":"tt101"}}`, false, true},
		{`{"id":100,"external_ids":{"id":100,"imdb_id":42}}`, false, true},
		{`{"id":100,"external_ids":{"id":100,"imdb_id":"../../escape"}}`, false, true},
	} {
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) { return jsonResponse(request, test.body), nil })
		detail, err := fetchNativeTMDBDetails(t.Context(), config.NewStore(path), transport, "tv", "100")
		_, known := detail.Attributes["imdb_id"]
		if (err != nil) != test.rejected || err == nil && known != test.known {
			t.Fatalf("%s known=%v err=%v", test.body, known, err)
		}
	}
}
