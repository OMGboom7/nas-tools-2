package mediameta

import (
	"context"
	"testing"
)

func TestManualPartNormalizationAndExplicitClear(t *testing.T) {
	for _, value := range []string{"CD01", "DVD2", "DISK10", "DISC12", "PART1", "PARTAB", "PARTC", "PARTIII", "PART", "CD", ""} {
		part, err := NormalizePart(value)
		if err != nil || part != value {
			t.Fatal(value, part, err)
		}
	}
	part, err := NormalizePart(" cd01 ")
	if err != nil || part != "CD01" {
		t.Fatal(part, err)
	}
	for _, invalid := range []string{"../CD1", "CD123", "PARTIIII", "Part1/CD2", "CD1.CD2", "CD 1", "Custom", "PART1\x00"} {
		if _, err := NormalizePart(invalid); err == nil {
			t.Fatal("accepted unsafe part", invalid)
		}
	}
	for _, selected := range []string{"CD03", ""} {
		meta, err := ParseWithOptionsAndPart(context.Background(), "Movie.2026.CD[01].CD2.1080p.mkv", "", LabelOptions{}, &selected)
		if err != nil || meta.Part != selected || meta.Title != "Movie" || meta.Episodes.TV || meta.Resolution != "1080p" {
			t.Fatal(meta, err)
		}
	}
	if _, err := Parse("Movie.2026.CD1.CD2.mkv", ""); err == nil {
		t.Fatal("automatic recognition adopted ambiguity")
	}
	selected := "PARTIII"
	meta, err := ParseWithOptionsAndPart(context.Background(), "Show.S01E02.CD1.CD2.mkv", "", LabelOptions{}, &selected)
	if err != nil || meta.Part != selected || !meta.Episodes.TV || meta.Episodes.Episode == nil || *meta.Episodes.Episode != 2 {
		t.Fatal(meta, err)
	}
}
