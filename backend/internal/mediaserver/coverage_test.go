package mediaserver

import (
	"errors"
	"reflect"
	"testing"
)

func TestCoverageChecksExplicitEpisodesAndWholeSeasons(t *testing.T) {
	inventory := Inventory{ItemIDs: []string{"series"}, Episodes: map[int]map[int]bool{0: {0: true}, 1: {1: true, 2: true}, 2: {1: true}}}
	for _, test := range []struct {
		name      string
		selection CoverageSelection
		complete  bool
		missing   map[int][]int
	}{
		{"movie exists", CoverageSelection{}, true, map[int][]int{}},
		{"explicit episodes", CoverageSelection{TV: true, Episodes: []int{1, 2}}, true, map[int][]int{}},
		{"explicit episode missing", CoverageSelection{TV: true, Seasons: []int{1}, Episodes: []int{1, 3}}, false, map[int][]int{1: {3}}},
		{"special zero", CoverageSelection{TV: true, Seasons: []int{0}, Episodes: []int{0}}, true, map[int][]int{}},
		{"partial season", CoverageSelection{TV: true, Seasons: []int{1}, SeasonTotals: map[int]int{1: 3}}, false, map[int][]int{1: {3}}},
		{"whole season", CoverageSelection{TV: true, Seasons: []int{1}, SeasonTotals: map[int]int{1: 2}}, true, map[int][]int{}},
		{"whole series excludes specials", CoverageSelection{TV: true, SeasonTotals: map[int]int{0: 5, 1: 2, 2: 2}}, false, map[int][]int{2: {2}}},
		{"dedup sorted", CoverageSelection{TV: true, Seasons: []int{2, 2}, Episodes: []int{3, 2, 3}}, false, map[int][]int{2: {2, 3}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := inventory.Coverage(test.selection)
			if err != nil || got.Complete != test.complete || !reflect.DeepEqual(got.Missing, test.missing) {
				t.Fatalf("coverage=%+v err=%v", got, err)
			}
		})
	}
	for _, selection := range []CoverageSelection{{TV: true}, {TV: true, Seasons: []int{1}}, {TV: true, Seasons: []int{1}, SeasonTotals: map[int]int{1: 0}}} {
		if _, err := inventory.Coverage(selection); !errors.Is(err, ErrUnknownCoverage) {
			t.Fatalf("unknown totals accepted: %v", err)
		}
	}
	result, err := (Inventory{}).Coverage(CoverageSelection{TV: true, Seasons: []int{1}, SeasonTotals: map[int]int{1: 2}})
	if err != nil || result.Complete || !reflect.DeepEqual(result.Missing, map[int][]int{1: {1, 2}}) {
		t.Fatalf("missing series=%+v err=%v", result, err)
	}
}
