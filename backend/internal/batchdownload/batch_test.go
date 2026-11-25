package batchdownload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

func TestBatchPreservesSeasonZeroAndCreditsOnlyAcceptedResources(t *testing.T) {
	candidates := []Candidate{
		{Movie: true, Display: "Film", Show: "2", Site: "1", Seeders: "1000"},
		{Movie: true, Display: "Film", Show: "2", Site: "9", Seeders: "8"},
		{Show: "1", Seasons: []int{0}, Episodes: []int{1}, Label: "s0"},
		{Show: "1", Seasons: []int{1}, Episodes: []int{1, 2}, Label: "s1"},
		{Show: "1", Seasons: []int{2}, Episodes: []int{1}, Label: "s2"},
	}
	for _, order := range []string{"site", "seeder"} {
		missing := map[string][]Season{"1": {{0, 1, nil}, {1, 2, nil}, {2, 3, []int{1, 3}}}}
		got, err := Run(Input{candidates, missing, order}, func(a Action) (reply, error) {
			if a.Op != "submit" {
				t.Fatal("unexpected action", a)
			}
			return reply{Accepted: a.Index != 3}, nil
		})
		if err != nil || len(got.Accepted) != 3 || got.Accepted[0] != map[string]int{"site": 1, "seeder": 0}[order] || !slices.Contains(got.Accepted, 2) || !slices.Contains(got.Accepted, 4) ||
			!reflect.DeepEqual(got.Missing["1"], []Season{{1, 2, nil}, {2, 3, []int{3}}}) {
			t.Fatal(got, err)
		}
	}
}
func TestAmbiguousBatchStopsBeforeOverlappingAlternative(t *testing.T) {
	missing := map[string][]Season{"1": {{1, 2, []int{1, 2}}}}
	in := Input{[]Candidate{{Show: "1", Seasons: []int{1}, Episodes: []int{1}, Label: "E1"}, {Show: "1", Seasons: []int{1}, Episodes: []int{1, 2}, Label: "E1E2"}}, missing, ""}
	n := 0
	got, err := Run(in, func(a Action) (reply, error) { n++; return reply{Pending: true}, nil })
	if err != nil || n != 1 || len(got.Accepted) != 0 || len(got.Missing["1"][0].Episodes) != 2 {
		t.Fatal(got, err, n)
	}
}
func TestUncertainSubmissionStillAllowsDisjointEpisodes(t *testing.T) {
	in := Input{Candidates: []Candidate{
		{Show: "1", Label: "a", Seasons: []int{1}, Episodes: []int{1}},
		{Show: "1", Label: "b", Seasons: []int{1}, Episodes: []int{2}},
	}, Missing: map[string][]Season{"1": {{Number: 1, Total: 2}}}}
	got, err := Run(in, func(a Action) (reply, error) { return reply{Accepted: a.Index == 1, Pending: a.Index == 0}, nil })
	if err != nil || !slices.Equal(got.Accepted, []int{1}) || !slices.Equal(got.Missing["1"][0].Episodes, []int{1}) {
		t.Fatal(got, err)
	}
}
func TestBatchHistoryLabelKeepsGaps(t *testing.T) {
	if got := EpisodeLabel(map[int][]int{1: {2, 4, 5, 6}, 0: {1}}); got != "S00E01 S01E02E04-E06" {
		t.Fatal(got)
	}
}

func TestSubscriptionIntersectionNeverTurnsDisjointSetsIntoWholeSeasons(t *testing.T) {
	input := `{"target":[{"season":0,"episodes":[1],"total_episodes":3},{"season":1,"episodes":[2],"total_episodes":3},{"season":2,"episodes":[],"total_episodes":3}],"library":[{"season":1,"episodes":[1,3],"total_episodes":3},{"season":2,"episodes":[3],"total_episodes":3}]}`
	var out bytes.Buffer
	if err := Intersect(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var got []Season
	if json.Unmarshal(out.Bytes(), &got) != nil || !reflect.DeepEqual(got, []Season{{2, 3, []int{3}}}) {
		t.Fatal(out.String())
	}
}

func TestSeasonPackMustProveEveryEpisodeBeforeCredit(t *testing.T) {
	// Each indexed path has one byte. An unqualified file and an executable
	// are not evidence of a season, even if the file count matches the total.
	names := []string{"Show.S00E01.mp4", "Show.S01E02.asf", "Show.S01E02.srt", "Show.S01E02.exe", "E03.mkv"}
	var files strings.Builder
	for _, name := range names {
		fmt.Fprintf(&files, "d6:lengthi1e4:pathl%d:%see", len(name), name)
	}
	raw := []byte("d4:infod5:filesl" + files.String() + "e4:name4:Show12:piece lengthi16384e6:pieces20:12345678901234567890ee")
	in := Input{Candidates: []Candidate{{Show: "s", Seasons: []int{0, 1}}}, Missing: map[string][]Season{"s": {{Number: 0, Total: 1}, {Number: 1, Total: 3}}}}
	adds := 0
	res, err := Run(in, func(a Action) (reply, error) {
		if a.Op == "inspect" {
			return reply{Torrent: raw}, nil
		}
		adds++
		if !a.Partial || a.Proof == nil || !a.Proof.Mixed {
			t.Fatal(a)
		}
		plan, got, err := Files(raw, *a.Proof)
		if err != nil || !slices.Equal(got[0], []int{1}) || !slices.Equal(got[1], []int{2}) {
			t.Fatal(got, err)
		}
		for i, f := range plan {
			if f.Selected != (i < 3) {
				t.Fatal(f)
			}
		}
		return reply{Accepted: true}, nil
	})
	if err != nil || adds != 1 || !reflect.DeepEqual(res.Missing["s"], []Season{{Number: 1, Total: 3, Episodes: []int{1, 3}}}) {
		t.Fatal(res, err, adds)
	}
}

func TestWholeSeasonCanKeepAccompanyingFilesOnWholeTaskClients(t *testing.T) {
	proof := Proof{Whole: true, Targets: []Season{{Number: 1, Total: 2}}}
	files := []torrentfiles.File{{Path: "S01E01E02.mkv", Selected: true}, {Path: "README.txt"}}
	units := map[int][]int{1: {1, 2}}
	if RequiresSelection(proof, files, units) {
		t.Fatal("whole season unexpectedly requires per-file priorities")
	}
	if !RequiresSelection(proof, append(slices.Clone(files), torrentfiles.File{Path: "S02E01.mkv"}), units) {
		t.Fatal("unrequested season included")
	}
	proof.Whole = false
	if !RequiresSelection(proof, files, units) {
		t.Fatal("a partial request silently selected the whole payload")
	}
}
