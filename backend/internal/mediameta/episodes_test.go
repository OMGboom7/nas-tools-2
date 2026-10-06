package mediameta

import "testing"

func TestEpisodeMetadata(t *testing.T) {
	for _, test := range []struct {
		title, subtitle string
		tv              bool
		count           int
	}{
		{"Movie.2025.1080p", "", false, 0},
		{"Show.S01E01-E10.1080p", "", true, 10},
		{"Show.S01.E02.1080p", "", true, 1},
		{"Show.EP001-003", "", true, 3},
		{"Show.S01.1080p", "", true, 0},
		{"节目 第二季 第十一-二十集", "", true, 10},
		{"节目", "全二十四集", true, 24},
		{"节目", "第两百零一集", true, 1},
		{"节目 二〇集全", "", true, 20},
		{"[Group] Anime - 03v2 [1080p]", "", true, 1},
		{"Show.S01E02", "共24集", true, 1},
		{"Show.S01E01E02E03.1080p", "", true, 3},
		{"Show.EP001EP003", "", true, 3},
		{"[Group] Anime [01-12v2] [1080p]", "", true, 12},
		{"[Group] Anime 【TV 03】 【1080p】", "", true, 1},
		{"[Group] Anime - 01-12v2 [1080p]", "", true, 12},
		{"Show Season 2 Episode 1-10", "", true, 10},
		{"Movie [2025] [1080p]", "", false, 0},
	} {
		result, err := Episodes(test.title, test.subtitle)
		if err != nil || result.TV != test.tv || result.Count != test.count {
			t.Errorf("%q %q => %+v %v", test.title, test.subtitle, result, err)
		}
	}
	for _, input := range []string{"Show.E10-E01", "节目 第十百集", "共0集", "Anime [12-01]", "Show.E03E01", "Show.E01E05E03", "Show.S03-S01", "Show Episode 10-2"} {
		if _, err := Episodes(input, ""); err == nil {
			t.Errorf("invalid metadata accepted: %q", input)
		}
	}
}

func TestEpisodePositionsAndTitlePrecedence(t *testing.T) {
	for _, title := range []string{"Show.S02E01E02E03", "Show Season 2 Episode 1-3", "Show 第2季 第1-3集"} {
		info, err := Episodes(title, "全24集")
		if err != nil || info.Season == nil || *info.Season != 2 || info.Episode == nil || *info.Episode != 1 || info.EndEpisode == nil || *info.EndEpisode != 3 || info.Count != 3 {
			t.Fatalf("%q => %+v err=%v", title, info, err)
		}
	}
	info, err := Episodes("Anime [0001-0012v2] [1080p]", "")
	if err != nil || info.Episode == nil || *info.Episode != 1 || info.EndEpisode == nil || *info.EndEpisode != 12 || info.Count != 12 {
		t.Fatalf("padded anime positions=%+v err=%v", info, err)
	}
}
