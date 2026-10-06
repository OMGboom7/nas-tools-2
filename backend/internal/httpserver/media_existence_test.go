package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

func TestNativeMediaExistenceUsesVerifiedTotalsAndLibraryInventory(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("media:\n  media_server: emby\nemby:\n  host: http://emby.test\n  api_key: server-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	service := subscriptionService{configStore: config.NewStore(path), client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "emby.test" {
			t.Fatalf("unexpected Python request=%s", request.URL)
		}
		if request.URL.Path == "/emby/Items" {
			return jsonResponse(request, `{"Items":[{"Id":"series","Name":"Show","ProductionYear":2025,"ProviderIds":{"Tmdb":"100"}}]}`), nil
		}
		if request.URL.Path == "/emby/Shows/series/Episodes" {
			return jsonResponse(request, `{"Items":[{"ParentIndexNumber":1,"IndexNumber":1,"IndexNumberEnd":2}]}`), nil
		}
		t.Fatalf("unexpected endpoint=%s", request.URL)
		return nil, nil
	})}}
	var detail tmdbMediaDetails
	if err := json.Unmarshal([]byte(`{"id":100,"name":"Show","first_air_date":"2025-01-01","seasons":[{"season_number":1,"episode_count":3}]}`), &detail); err != nil {
		t.Fatal(err)
	}
	meta, err := mediameta.Parse("Show.S01", "")
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv")
	if err != nil || coverage.Complete || len(coverage.Missing[1]) != 1 || coverage.Missing[1][0] != 3 {
		t.Fatalf("partial season=%+v err=%v", coverage, err)
	}
	meta, err = mediameta.Parse("Show.S01E01-E02", "")
	if err != nil {
		t.Fatal(err)
	}
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "tv")
	if err != nil || !coverage.Complete {
		t.Fatalf("explicit episode subset=%+v err=%v", coverage, err)
	}
	if err := service.configStore.Update(map[string]any{"media.media_server": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv"); !errors.Is(err, errLocalMediaUnavailable) {
		t.Fatalf("missing server silently accepted=%v", err)
	}
}
