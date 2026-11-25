package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

func TestLibraryInventorySelection(t *testing.T) {
	var detail tmdbMediaDetails
	err := json.Unmarshal([]byte(`{"id":41,"seasons":[{"season_number":0,"episode_count":1},{"season_number":1,"episode_count":3},{"season_number":2,"episode_count":2},{"season_number":3,"episode_count":0}]}`), &detail)
	if err != nil {
		t.Fatal(err)
	}
	inventory := mediaserver.Inventory{ItemIDs: []string{"show"}, Episodes: map[int]map[int]bool{1: {1: true, 3: true}, 2: {1: true}}}
	query := inventoryQuery{Title: "剧目", Year: "2026", Kind: "电视剧"}
	got, err := describeLibraryInventory(query, detail, inventory)
	if err != nil || got.Complete || !reflect.DeepEqual(got.Missing, []missingLibrarySeason{{2, []int{2}, 2}, {1, []int{2}, 3}}) ||
		!reflect.DeepEqual(got.Messages, []string{"电视剧 剧目 (2026) 共有 2 季", "第2季 缺失集：2", "第1季 缺失集：2"}) {
		t.Fatal(got, err)
	}
	n1, n2, n3, n4 := 1, 2, 3, 4
	query.Metadata.Episodes = mediameta.EpisodeInfo{Episode: &n3}
	query.SeasonEpisode = "S01E03"
	got, err = describeLibraryInventory(query, detail, inventory)
	if err != nil || !got.Complete || len(got.Missing) != 1 || !strings.Contains(strings.Join(got.Messages, " "), "S01E03") {
		t.Fatal(got, err)
	}
	query.Metadata.Episodes = mediameta.EpisodeInfo{Season: &n1, EndSeason: &n2, Episode: &n1, EndEpisode: &n2}
	got, err = describeLibraryInventory(query, detail, inventory)
	if err != nil || got.Complete || len(got.Missing) != 2 {
		t.Fatal(got, err)
	}
	query.Metadata.Episodes = mediameta.EpisodeInfo{Season: &n2}
	query.Totals = map[int]int{2: 4}
	got, err = describeLibraryInventory(query, detail, mediaserver.Inventory{})
	if err != nil || got.Complete || !reflect.DeepEqual(got.Missing, []missingLibrarySeason{{2, []int{}, 4}}) {
		t.Fatal(got, err)
	}
	query.Metadata.Episodes.Season = &n4
	if _, err = describeLibraryInventory(query, detail, inventory); err == nil {
		t.Fatal("unknown season treated as absent")
	}
	query.Metadata.Episodes = mediameta.EpisodeInfo{Season: &n1, Episode: &n4}
	if _, err = describeLibraryInventory(query, detail, inventory); err == nil {
		t.Fatal("out-of-range episode treated as present")
	}
	query.Metadata.Episodes = mediameta.EpisodeInfo{Season: &n1}
	detail.Seasons = append(detail.Seasons, detail.Seasons[1])
	if _, err = describeLibraryInventory(query, detail, inventory); err == nil {
		t.Fatal("duplicate totals accepted")
	}
}

func TestLibraryInventoryCommandLocalAndVerifiedDetail(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "config.yaml")
	write := func(path, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg, "media:\n  tv_path: "+root+"\n  tv_name_format: '{edition}/S{season}/{title} ({year})/Season {season}/{episode}'\n  movie_path: "+root+"\n")
	write(filepath.Join(root, "user.db"), "")
	write(filepath.Join(root, "Drama", "Custom", "S1", "Example (2026)", "Season 1", "Example.S01E01E03.mkv"), "fixture")
	input := inventoryQuery{ID: "42", Kind: "电视剧", Title: "Example", Year: "2026", Category: "Drama", TVDetail: json.RawMessage(`{"id":42,"name":"Example","seasons":[{"season_number":1,"episode_count":4}]}`), Naming: map[string]string{"edition": "Custom"}}
	run := func(ok bool) libraryCheck {
		t.Helper()
		var output bytes.Buffer
		b, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		err = LibraryInventoryCommand(context.Background(), cfg, bytes.NewReader(b), &output)
		if (err == nil) != ok || !ok && output.Len() != 0 {
			t.Fatal(output.String(), err)
		}
		var r libraryCheck
		if ok {
			if err = json.Unmarshal(output.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	r := run(true)
	if !reflect.DeepEqual(r.Missing, []missingLibrarySeason{{1, []int{2, 4}, 4}}) {
		t.Fatal(r)
	}
	input.TVDetail = json.RawMessage(`{"id":43,"name":"Other"}`)
	run(false)
	input.TVDetail = nil
	input.LocalOnly = true
	input.ID = ""
	one := 1
	input.Metadata.Episodes.Season = &one
	input.Totals = map[int]int{1: 3}
	r = run(true)
	if !reflect.DeepEqual(r.Missing, []missingLibrarySeason{{1, []int{2}, 3}}) {
		t.Fatal(r)
	}
	input = inventoryQuery{ID: "12", Kind: "电影", Title: "Movie", Year: "2024"}
	if r = run(true); r.Complete || len(r.Messages) != 0 {
		t.Fatal(r)
	}
	write(filepath.Join(root, "精选", "Movie (2024)", "Movie.mkv"), "fixture")
	if r = run(true); !r.Complete || !reflect.DeepEqual(r.Messages, []string{"媒体库中已存在电影：\n • Movie (2024)"}) {
		t.Fatal(r)
	}
	write(cfg, "media: {}\n")
	run(false)
}
