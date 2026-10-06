package httpserver

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func TestMediaInventoryFallbackIsEvidenceBased(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("media:\n  media_server: emby\nemby:\n  host: http://emby.test\n  api_key: secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status, calls := 500, 0
	service := subscriptionService{configStore: config.NewStore(configPath), client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		response := jsonResponse(request, `{"Items":[]}`)
		response.StatusCode = status
		return response, nil
	})}}
	meta, _ := mediameta.Parse("Movie.2025", "")
	detail := tmdbMediaDetails{ID: 100, Title: "Movie", ReleaseDate: "2025-01-01"}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "movie"); err == nil || errors.Is(err, errLocalMediaUnavailable) {
		t.Fatalf("server failure without local config=%v", err)
	}
	if err := service.configStore.Update(map[string]any{"media.movie_path": root}); err != nil {
		t.Fatal(err)
	}
	coverage, err := service.nativeMediaExistence(t.Context(), meta, detail, "movie")
	if err != nil || coverage.Complete {
		t.Fatalf("verified empty local root=%+v %v", coverage, err)
	}
	directory := filepath.Join(root, "Movie (2025)")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "Movie.mkv"), []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, failureStatus := range []int{401, 500} {
		status = failureStatus
		coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "movie")
		if err != nil || !coverage.Complete {
			t.Fatalf("server status %d local coverage=%+v %v", status, coverage, err)
		}
	}
	status = 200
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "movie")
	if err != nil || coverage.Complete {
		t.Fatalf("valid server empty inventory must not fall back=%+v %v", coverage, err)
	}
	status = 500
	if err := service.configStore.Update(map[string]any{"media.movie_path": filepath.Join(root, "unmounted")}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "movie"); err == nil {
		t.Fatal("failed server and inaccessible local root accepted as absence")
	}
	before := calls
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.nativeMediaExistence(ctx, meta, detail, "movie"); !errors.Is(err, context.Canceled) || calls != before {
		t.Fatalf("pre-cancelled lookup=%v calls=%d", err, calls-before)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	service.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		cancel()
		return nil, errors.New("transport failure")
	})
	if _, err := service.nativeMediaExistence(ctx, meta, detail, "movie"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during server request hidden by fallback: %v", err)
	}
	if err := service.configStore.Update(map[string]any{"media.movie_path": root, "emby.api_key": ""}); err != nil {
		t.Fatal(err)
	}
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "movie")
	if err != nil || !coverage.Complete {
		t.Fatalf("incomplete server configuration with usable local root=%+v %v", coverage, err)
	}
	if err := service.configStore.Update(map[string]any{"media.movie_path": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "movie"); !errors.Is(err, mediaserver.ErrConfiguration) {
		t.Fatalf("server configuration error lost=%v", err)
	}
}
