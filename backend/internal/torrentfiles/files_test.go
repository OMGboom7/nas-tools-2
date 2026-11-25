package torrentfiles

import (
	"slices"
	"testing"
)

func manifestFixture() []File {
	return []File{{Index: 0, Path: "Show.S01/Show.S01E01.mkv", Size: 11}, {Index: 1, Path: "Show.S01/Show.S01E02.mkv", Size: 12}, {Index: 2, Path: "Show.S01/Show.S01E03.mkv", Size: 13}, {Index: 3, Path: "Show.S01/Show.S01E02.zh.srt", Size: 2}, {Index: 4, Path: "Show.S01/readme.exe", Size: 1}}
}
func TestPlanWholeFilesAndCompanionsNeverTurnsAnIntersectionIntoWholePack(t *testing.T) {
	files, episodes, err := Plan(manifestFixture(), 1, 3, []int{2, 3})
	if err != nil || !slices.Equal(episodes, []int{2, 3}) || !slices.Equal(IDs(files, true), []int{1, 2, 3}) {
		t.Fatal(files, episodes, err)
	}
	chain := []File{{Index: 5, Path: "Show.S01/Show.S01E01E03.mkv", Size: 14}}
	if _, _, err := Plan(chain, 1, 3, []int{1}); err == nil {
		t.Fatal("split indivisible multi-episode file")
	}
	files, episodes, err = Plan(chain, 1, 3, []int{1, 3})
	if err != nil || !slices.Equal(episodes, []int{1, 3}) || len(files) != 1 {
		t.Fatal(files, episodes, err)
	}
	rangeFile := []File{{Index: 1, Path: "Show.S01/Show.S01E01-E03.mkv", Size: 14}}
	if _, _, err := Plan(rangeFile, 1, 3, []int{2, 3}); err == nil {
		t.Fatal("accepted partial multi-episode file")
	}
}
func TestPlanRespectsSeasonDirectoriesAndDoesNotSelectUnknownOrExecutableFiles(t *testing.T) {
	files := []File{{Index: 0, Path: "Show/Season 2/E02.mkv", Size: 12}, {Index: 1, Path: "Show/Season 1/E02.mkv", Size: 12}, {Index: 2, Path: "Show/S01E02.exe", Size: 12}, {Index: 3, Path: "Show/sample.S01E02.mp4", Size: 12}, {Index: 4, Path: "Show/Season 1/02.mkv", Size: 12}, {Index: 5, Path: "Show/S01-S02/E02.mkv", Size: 12}, {Index: 6, Path: "Show/S01E99.mkv", Size: 12}}
	selected, eps, err := Plan(files, 1, 3, []int{2})
	if err != nil || !slices.Equal(eps, []int{2}) || !slices.Equal(IDs(selected, true), []int{1}) {
		t.Fatal(selected, eps, err)
	}
}
func TestNormalizeRejectsTraversalDuplicateIDsAliasesParentsAndOverflow(t *testing.T) {
	for _, files := range [][]File{
		{{Index: 0, Path: "../x", Size: 1}}, {{Index: 0, Path: "a\\x", Size: 1}}, {{Index: 0, Path: "C:/x", Size: 1}}, {{Index: 0, Path: "a//x", Size: 1}},
		{{Index: 0, Path: "a", Size: 1}, {Index: 1, Path: "A", Size: 1}},
		{{Index: 0, Path: "a", Size: 1}, {Index: 1, Path: "a-b", Size: 1}, {Index: 2, Path: "a/c", Size: 1}},
		{{Index: 0, Path: "a", Size: 1}, {Index: 0, Path: "b", Size: 1}}, {{Index: 0, Path: "a", Size: -1}},
		{{Index: 0, Path: "a", Size: 9223372036854775807}, {Index: 1, Path: "b", Size: 1}},
	} {
		if _, err := Normalize(files); err == nil {
			t.Fatal("invalid manifest accepted", files)
		}
	}
}
func TestMatchingUsesAllPathsAndSizesNotClientArrayOrderOrDisplayTitle(t *testing.T) {
	expected := manifestFixture()
	expected[1].Selected = true
	actual := append([]File{}, expected...)
	for i := range actual {
		actual[i].Index = 100 - i
	}
	if !Match(expected, actual) || !Wanted(expected, actual) {
		t.Fatal("different remote indexes changed semantic identity")
	}
	actual[0].Selected = true
	if Wanted(expected, actual) {
		t.Fatal("extra file selected")
	}
	actual[0].Selected = false
	actual[3].Size++
	if Match(expected, actual) {
		t.Fatal("changed file size")
	}
}

func TestExactSeasonNeverAssignsUnqualifiedFilesToSeveralSeasons(t *testing.T) {
	files := []File{
		{Index: 1, Path: "Show.S01-S02/E01.mkv", Size: 12},
		{Index: 2, Path: "Show.S01-S02/S01/E01.mkv", Size: 12},
		{Index: 3, Path: "Show.S01-S02/S02/E01.mkv", Size: 12},
		{Index: 4, Path: "Show.S01-S02/Show.S02E02.mkv", Size: 12},
		{Index: 5, Path: "Show.S01-S02/Show.S02E02.zh.srt", Size: 1},
	}
	one, eps, err := PlanExactSeason(files, 1, 2, []int{1, 2})
	if err != nil || !slices.Equal(eps, []int{1}) || !slices.Equal(IDs(one, true), []int{2}) {
		t.Fatal(one, eps, err)
	}
	two, eps, err := PlanExactSeason(files, 2, 2, []int{1, 2})
	if err != nil || !slices.Equal(eps, []int{1, 2}) || !slices.Equal(IDs(two, true), []int{3, 4, 5}) {
		t.Fatal(two, eps, err)
	}
	if _, _, err := PlanExactSeason([]File{{Index: 1, Path: "Show/E01.mkv", Size: 12}}, 1, 2, []int{1}); err == nil {
		t.Fatal("guessed unknown season")
	}
	// A fixed single-season workflow keeps its existing implicit scope.
	if _, eps, err := Plan([]File{{Index: 1, Path: "Show/E01.mkv", Size: 12}}, 1, 2, []int{1}); err != nil || !slices.Equal(eps, []int{1}) {
		t.Fatal(eps, err)
	}
}

func TestRepeatedPaddingRetainsMultiplicityAndCannotHidePayload(t *testing.T) {
	payload := File{Index: 0, Path: "show/.pad/movie", Size: 11}
	pad := File{Index: 1, Path: "show/.pad/5", Size: 5, Padding: true}
	second := pad
	second.Index = 2
	manifest := []File{payload, pad, second}
	if normalized, err := Normalize(manifest); err != nil || !slices.Equal(normalized, manifest) {
		t.Fatal(normalized, err)
	}
	for _, live := range [][]File{{payload}, {payload, second}, manifest} {
		if !Match(manifest, live) || !MatchTransfer(manifest, live) {
			t.Fatal("valid client layout rejected", live)
		}
	}
	visible := pad
	visible.Padding, visible.Selected = false, true
	live := []File{payload, visible}
	if !MatchTransfer(manifest, live) || Match(manifest, live) || Wanted(manifest, live) {
		t.Fatal("wrong rules for selected client-visible padding")
	}
	extra := pad
	extra.Index = 3
	disguised := payload
	disguised.Padding = true
	for _, invalid := range [][]File{{pad}, {disguised}, {payload, pad, second, extra}} {
		if MatchTransfer(manifest, invalid) {
			t.Fatal("accepted inconsistent client files", invalid)
		}
	}
	for _, change := range []func(*File){
		func(f *File) { f.Index = pad.Index },
		func(f *File) { f.Padding = false },
		func(f *File) { f.Selected = true },
		func(f *File) { f.Size++ },
		func(f *File) { f.Path = "show/.PAD/5" },
	} {
		other := second
		change(&other)
		for _, pair := range [][]File{{pad, other}, {other, pad}} {
			if _, err := Normalize(pair); err == nil {
				t.Fatal("accepted ambiguous file paths", pair)
			}
		}
	}
}
