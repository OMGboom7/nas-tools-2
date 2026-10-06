package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

func TestLocalMediaCoverageMovieAndSeasons(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	contents := "media:\n  movie_path: " + root + "\n  tv_path:\n    - " + root + "\n"
	if err := os.WriteFile(configPath, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	service := subscriptionService{configStore: config.NewStore(configPath), client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("local lookup made network request: %s", request.URL)
		return nil, nil
	})}}
	put := func(relative string) {
		t.Helper()
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("media"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var detail tmdbMediaDetails
	if err := json.Unmarshal([]byte(`{"id":100,"title":"Movie","release_date":"2025-01-01"}`), &detail); err != nil {
		t.Fatal(err)
	}
	meta, _ := mediameta.Parse("Movie.2025", "")
	put("Movie (2025)/Movie.srt")
	coverage, err := service.nativeMediaExistence(t.Context(), meta, detail, "movie")
	if err != nil || coverage.Complete {
		t.Fatalf("subtitle-only movie=%+v %v", coverage, err)
	}
	put("精选/Movie (2025)/nested/Movie.MKV")
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "movie")
	if err != nil || !coverage.Complete {
		t.Fatalf("favorite movie=%+v %v", coverage, err)
	}
	if err := json.Unmarshal([]byte(`{"id":100,"name":"Show","first_air_date":"2025-01-01","seasons":[{"season_number":1,"episode_count":3}]}`), &detail); err != nil {
		t.Fatal(err)
	}
	put("Drama/Show (2025)/Season 1/Show.S01E01-E02.mkv")
	put("Drama/Show (2025)/Season 1/Other.S01E03.mkv")
	put("Drama/Show (2025)/Season 1/.hidden/Show.S01E03.mkv")
	put("Drama/Show (2025)/Season 1/@Recycle/Show.S01E03.mkv")
	meta, _ = mediameta.Parse("Show.S01", "")
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "tv", "Drama")
	if err != nil || coverage.Complete || len(coverage.Missing[1]) != 1 || coverage.Missing[1][0] != 3 {
		t.Fatalf("partial season=%+v %v", coverage, err)
	}
	meta, _ = mediameta.Parse("Show.S01E01-E02", "")
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "tv", "Drama")
	if err != nil || !coverage.Complete {
		t.Fatalf("episode range=%+v %v", coverage, err)
	}
	if err := service.configStore.Update(map[string]any{"media.tv_name_format": "{tmdbid}/{title}/S{season}/unused"}); err != nil {
		t.Fatal(err)
	}
	put("100/Show/S1/Show.S01E01-E02.strm")
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "tv")
	if err != nil || !coverage.Complete {
		t.Fatalf("custom template=%+v %v", coverage, err)
	}
	if err := service.configStore.Update(map[string]any{"media.tv_name_format": "{decade_long}/{title:.4}/Season {season:0>2}/unused"}); err != nil {
		t.Fatal(err)
	}
	put("2020-2029/Show/Season 01/Show.S01E01-E02.mkv")
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "tv")
	if err != nil || !coverage.Complete {
		t.Fatalf("formatted template=%+v %v", coverage, err)
	}
	if err := service.configStore.Update(map[string]any{"media.tv_name_format": "{imdbid}/{title}/Season {season:0>2}/unused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv"); err == nil {
		t.Fatal("missing IMDb metadata was treated as an empty directory field")
	}
	detail.Attributes = map[string]any{"imdb_id": "tt100"}
	put("tt100/Show/Season 01/Show.S01E01-E02.mkv")
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "tv")
	if err != nil || !coverage.Complete {
		t.Fatalf("IMDb template=%+v %v", coverage, err)
	}
	animeRoot := t.TempDir()
	if err := service.configStore.Update(map[string]any{"media.tv_name_format": "", "media.anime_path": []any{animeRoot, root}}); err != nil {
		t.Fatal(err)
	}
	detail.Attributes = map[string]any{"genres": []any{map[string]any{"id": 16}}}
	put("Show (2025)/Season 0/Show.S00E00.mkv")
	meta, _ = mediameta.Parse("Show.S00E00", "")
	coverage, err = service.nativeMediaExistence(t.Context(), meta, detail, "tv")
	if err != nil || !coverage.Complete {
		t.Fatalf("anime roots and specials=%+v %v", coverage, err)
	}
	if err := service.configStore.Update(map[string]any{"media.tv_name_format": "{episode_title}/Season {season}/unused"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.nativeMediaExistence(t.Context(), meta, detail, "tv"); err == nil {
		t.Fatal("unsupported template accepted as missing media")
	}
}

func TestLocalInventoryFailsClosedOnPathsAndCancellation(t *testing.T) {
	root := t.TempDir()
	if _, err := scanLocalMedia(t.Context(), filepath.Join(root, "unmounted"), "Movie", new(int)); err == nil {
		t.Fatal("unavailable root accepted")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "Movie")); err != nil {
		t.Fatal(err)
	}
	if _, err := scanLocalMedia(t.Context(), root, "Movie", new(int)); err == nil {
		t.Fatal("symlink directory accepted")
	}
	if err := os.Mkdir(filepath.Join(root, "Show"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := scanLocalMedia(ctx, root, "Show", new(int)); err == nil {
		t.Fatal("cancelled scan succeeded")
	}
	for _, template := range []string{"../{title}", "/{title}", "{title}/../escape", "{season:02d}", "{unknown}", "{title"} {
		if _, err := renderLocalDirectory(template, map[string]string{"title": "Movie", "season": "1"}); err == nil {
			t.Fatalf("unsafe/unsupported template accepted: %q", template)
		}
	}
	if _, err := scanLocalMedia(t.Context(), root, "../escape", new(int)); err == nil {
		t.Fatal("path traversal accepted")
	}
	budget := 10000
	if _, err := scanLocalMedia(t.Context(), root, "Show", &budget); err == nil {
		t.Fatal("scan budget ignored")
	}
	if got := localLibraryName(" A: B/ C，? ", map[string]any{}); got != "A： B C" {
		t.Fatalf("filename normalization=%q", got)
	}
}
