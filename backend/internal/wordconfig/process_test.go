package wordconfig

import (
	"context"
	"strings"
	"testing"
)

func TestCustomWordProcessing(t *testing.T) {
	for _, test := range []struct {
		name, title, want string
		words             []Word
	}{
		{"literal screen and replace", "[Ad] Wrong Title", " Right Title", []Word{{Type: 1, Replaced: "[Ad]", Enabled: 1}, {Type: 2, Replaced: "Wrong", Replace: "Right", Enabled: 1}}},
		{"disabled", "Original", "Original", []Word{{Type: 2, Replaced: "Original", Replace: "Changed", Enabled: 0}}},
		{"Python numeric replacement", "Title S02", "Title Season 02 $literal", []Word{{Type: 2, Regex: 1, Replaced: `S(\d+)`, Replace: `Season \1 $literal`, Enabled: 1}}},
		{"Python named replacement", "Title S02", "Title Season 02", []Word{{Type: 2, Regex: 1, Replaced: `S(?P<season>\d+)`, Replace: `Season \g<season>`, Enabled: 1}}},
		{"numeric offset", "Title E01-E02", "Title E02-E03", []Word{{Type: 4, Front: "E", Back: "", Offset: "EP+1", Enabled: 1}}},
		{"Chinese offset", "Title 第十一集", "Title 第十二集", []Word{{Type: 4, Front: "第", Back: "集", Offset: "EP+1", Enabled: 1}}},
		{"replace and offset", "Wrong E01", "Right E03", []Word{{Type: 3, Replaced: "Wrong", Replace: "Right", Front: "E", Offset: "EP+2", Enabled: 1}}},
		{"replacement dollar literal", "Title E01", "Title $1", []Word{{Type: 2, Regex: 1, Replaced: `E\d+`, Replace: "$1", Enabled: 1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := ProcessWords(t.Context(), test.title, test.words)
			if err != nil || result.Title != test.want || len(result.Warnings) != 0 {
				t.Fatalf("result=%+v err=%v want=%q", result, err, test.want)
			}
		})
	}
}

func TestInvalidWordRestoresTitleAndAllowsFollowingWords(t *testing.T) {
	result, err := ProcessWords(t.Context(), "Wrong E01", []Word{{ID: 1, Type: 3, Enabled: 1, Replaced: "Wrong", Replace: "Changed", Front: "E", Offset: "EP/0"}, {ID: 2, Type: 2, Enabled: 1, Replaced: "Wrong", Replace: "Correct"}})
	if err != nil || result.Title != "Correct E01" || len(result.Warnings) != 1 || len(result.Replaced) != 1 {
		t.Fatalf("atomic word result=%+v err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ProcessWords(ctx, "Title", []Word{{Enabled: 1}}); err == nil {
		t.Fatal("canceled processing succeeded")
	}
}

func TestEpisodeOffsetArithmetic(t *testing.T) {
	for expression, want := range map[string]int{"EP+2": 5, "EP*2+1": 7, "EP/2": 1, "EP//2": 1, "-EP//2": -2, "EP**2": 9, "-EP**2": -9, "EP+2**3": 11} {
		got, err := evaluateOffset(expression, 3)
		if err != nil || got != want {
			t.Fatalf("%s=%d err=%v want=%d", expression, got, err, want)
		}
	}
	for _, expression := range []string{"EP/0", "EP+__import__('os')", "EP**1000", "EP+", "EP+++"} {
		if _, err := evaluateOffset(expression, 3); err == nil {
			t.Fatalf("invalid expression accepted: %s", expression)
		}
	}
}

func TestCustomWordExpansionLimitsPreserveTitle(t *testing.T) {
	for _, regex := range []int{0, 1} {
		result, err := ProcessWords(t.Context(), strings.Repeat("x", 100), []Word{{ID: 1, Type: 2, Enabled: 1, Regex: regex, Replaced: "x", Replace: strings.Repeat("y", 16000)}})
		if err != nil || result.Title != strings.Repeat("x", 100) || len(result.Warnings) != 1 || len(result.Replaced) != 0 {
			t.Fatalf("regex=%d result=%+v err=%v", regex, result, err)
		}
	}
}
