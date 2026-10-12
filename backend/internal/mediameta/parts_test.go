package mediameta

import "testing"

func TestReleasePartMarkersPreserveTitleAndDoNotCreateEpisodes(t *testing.T) {
	for _, test := range []struct{ title, part string }{
		{"Movie.2024.CD1.1080p.mkv", "CD1"},
		{"Movie.2024.cd.02.mkv", "CD02"},
		{"Movie.1080p.DISC-12.mkv", "DISC12"},
		{"Movie.2024.DVD2.mkv", "DVD2"},
		{"Movie.2024.DISK01.mkv", "DISK01"},
		{"Movie.2024.Part.III.mkv", "PARTIII"},
		{"Movie.2024.PARTA.mkv", "PARTA"},
		{"Movie.2024.PART.C.mkv", "PARTC"},
		{"Movie.2024.CD[01].mkv", "CD01"},
		{"Movie.2024.DISC.mkv", "DISC"},
		{"Movie.2024.PART.mkv", ""},
		{"Movie.2024.CD123.mkv", ""},
		{"Movie.2024.discfan.mkv", ""},
	} {
		t.Run(test.title, func(t *testing.T) {
			meta, err := Parse(test.title, "")
			if err != nil || meta.Part != test.part || meta.Episodes.TV || meta.Episodes.Count != 0 {
				t.Fatal(meta, err)
			}
			if meta.Title != "Movie" {
				t.Fatal("release part polluted title", meta)
			}
		})
	}
	meta, err := Parse("The.Godfather.Part.II.1974.1080p.mkv", "")
	if err != nil || meta.Part != "" || meta.Title != "The Godfather Part II" {
		t.Fatal("franchise title became a file part", meta, err)
	}
	meta, err = Parse("Show.S01E02.PART1.1080p.mkv", "")
	if err != nil || meta.Part != "PART1" || meta.Episodes.Episode == nil || *meta.Episodes.Episode != 2 || meta.Episodes.Count != 1 {
		t.Fatal(meta, err)
	}
}

func TestReleasePartPrecedenceAmbiguityAndEpisodeMasking(t *testing.T) {
	meta, err := Parse("Movie.2024.1080p", "CD2")
	if err != nil || meta.Part != "CD2" {
		t.Fatal(meta, err)
	}
	meta, err = Parse("Movie.2024.CD1", "CD2")
	if err != nil || meta.Part != "CD1" {
		t.Fatal(meta, err)
	}
	if _, err = Parse("Movie.2024.CD1.CD2", ""); err == nil {
		t.Fatal("ambiguous parts silently selected")
	}
	info, err := Episodes("Movie.2024.CD[01]", "")
	if err != nil || info.TV || info.Episode != nil {
		t.Fatal("disc number became an episode", info, err)
	}
	meta, err = Parse("Movie.20255.CD1", "")
	if err != nil || meta.Part != "" {
		t.Fatal("invalid year provided release context", meta, err)
	}
}
