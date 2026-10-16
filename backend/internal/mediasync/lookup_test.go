package mediasync

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func TestDisplayCacheExactIdentityTypeYearAndEpisodeUnion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	digest := strings.Repeat("a", 64)
	items := []mediaserver.SyncItem{
		{ID: "m", Library: "films", Type: "movie", Title: "Movie", Year: "2026", TMDBID: "100"},
		{ID: "old", Library: "films", Type: "movie", Title: "Movie", Year: "2025", TMDBID: "101"},
		{ID: "title", Library: "films", Type: "movie", Title: "Title Only", Year: "2023"},
		{ID: "show", Library: "tv", Type: "show", Title: "Show", Year: "2024", TMDBID: "100", Episodes: []mediaserver.SyncEpisode{{Season: 0, Episode: 0}, {Season: 1, Episode: 1}}},
		{ID: "version", Library: "tv", Type: "show", Title: "Show", Year: "2024", TMDBID: "100", Episodes: []mediaserver.SyncEpisode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}}},
	}
	published, err := s.Publish(ctx, "emby", digest, "admin", []string{"films", "tv"}, items)
	if err != nil {
		t.Fatal(err)
	}
	one, two, zero := 1, 2, 0
	queries := []CacheQuery{{TMDBID: "100", Type: "movie"}, {TMDBID: "100", Type: "show"}, {TMDBID: "100", Type: "show", Season: &one, Episode: &two}, {TMDBID: "100", Type: "show", Season: &two}, {TMDBID: "100", Type: "show", Season: &zero, Episode: &zero}, {TMDBID: "100", Type: "show", Episode: &two}, {TMDBID: "777", Title: "Movie", Year: "2026", Type: "movie"}, {Title: "Movie", Year: "2025", Type: "movie"}, {Title: "Movie", Year: "2027", Type: "movie"}, {Title: "Title Only", Year: "2023", Type: "movie"}, {TMDBID: "100", Type: "movie", Title: "Other title", Year: "2020"}}
	result, err := s.LookupBatch(ctx, "emby", digest, queries)
	if err != nil || result.Cache.State != "ready" || !result.Cache.DisplayOnly || result.Cache.Generation != published.Generation {
		t.Fatal(result, err)
	}
	want := []string{"present", "present", "present", "not-recorded", "present", "present", "not-recorded", "present", "not-recorded", "present", "present"}
	for i, state := range want {
		if result.Results[i].State != state {
			t.Fatal(i, result.Results[i], state)
		}
	}
	if result.Results[1].Episodes != 3 || result.Results[1].Seasons != 2 || result.Results[2].Episodes != 1 {
		t.Fatal(result.Results)
	}
	changed, err := s.LookupBatch(ctx, "emby", strings.Repeat("b", 64), queries[:1])
	if err != nil || changed.Cache.State != "scope-changed" || changed.Results[0].State != "unavailable" {
		t.Fatal(changed, err)
	}
	missing, err := s.LookupBatch(ctx, "plex", digest, queries[:1])
	if err != nil || missing.Cache.State != "unsynced" || missing.Results[0].State != "unavailable" {
		t.Fatal(missing, err)
	}
}
func TestDisplayCacheMalformedEpisodesAndInvalidInputsStayUnknown(t *testing.T) {
	s, d, _ := scheduleFixture(t)
	ctx := context.Background()
	items := []mediaserver.SyncItem{{ID: "show", Library: "films", Type: "show", Title: "Show", TMDBID: "100", Episodes: []mediaserver.SyncEpisode{{Season: 1, Episode: 1}}}}
	if _, err := s.Publish(ctx, d.Server, d.ConfigDigest, d.Actor, d.Libraries, items); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE MEDIASYNC_ITEMS SET JSON='[{"season_num":1}]'`); err != nil {
		t.Fatal(err)
	}
	result, err := s.LookupBatch(ctx, d.Server, d.ConfigDigest, []CacheQuery{{TMDBID: "100", Type: "show"}})
	if err != nil || result.Results[0].State != "unavailable" {
		t.Fatal(result, err)
	}
	negative := -1
	for _, query := range []CacheQuery{{TMDBID: "100", Type: "episode"}, {TMDBID: "100", Type: "movie", Season: &negative}, {TMDBID: "0100", Type: "movie"}, {TMDBID: "-1", Type: "movie"}, {Type: "movie"}, {Title: "Movie", Type: "movie", Year: "-123"}, {TMDBID: "100", Type: "show", Season: &negative}} {
		if _, err := s.LookupBatch(ctx, d.Server, d.ConfigDigest, []CacheQuery{query}); err == nil {
			t.Fatal("accepted bad display query", query)
		}
	}
}
