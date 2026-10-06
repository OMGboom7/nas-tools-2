package mediameta

import "testing"

func TestReleaseNameMetadata(t *testing.T) {
	for _, test := range []struct {
		raw, title, year, resolution, source string
		count                                int
	}{
		{"Example.Movie.2025.1080p.WEB-DL.H.264.DDP5.1.mkv", "Example Movie", "2025", "1080p", "WEB-DL", 0},
		{"[Group] Example.Show.S02E01-E10.2160p.BluRay.HEVC.DTS-HD.MA", "Example Show", "", "2160p", "BluRay", 10},
		{"节目 第二季 第十一-二十集 1080p WEB-DL", "节目", "", "1080p", "WEB-DL", 10},
		{"[Group] Anime - 03v2 [1080p]", "Anime", "", "1080p", "", 1},
		{"2001.A.Space.Odyssey.1968.1080p", "2001 A Space Odyssey", "1968", "1080p", "", 0},
		{"Blade.Runner.2049.(2017).2160p", "Blade Runner 2049", "2017", "2160p", "", 0},
		{"Blade Runner 2049 2017 2160p", "Blade Runner 2049", "2017", "2160p", "", 0},
		{"Example 20255", "Example 20255", "", "", "", 0},
		{"Spider-Man: No Way Home (2021)", "Spider-Man: No Way Home", "2021", "", "", 0},
		{"1917", "1917", "", "", "", 0},
		{"测试电影", "测试电影", "", "", "", 0},
		{"Example Season 2", "Example", "", "", "", 0},
		{"[Group] Anime [01-12v2] [1080p]", "Anime", "", "1080p", "", 12},
		{"[Group] Anime - 01-12v2 [1080p]", "Anime", "", "1080p", "", 12},
		{"Show.E01E02E03.1080p", "Show", "", "1080p", "", 3},
		{"Show Season 2 Episode 1-10", "Show", "", "", "", 10},
	} {
		result, err := Parse(test.raw, "")
		if err != nil || result.Title != test.title || result.Year != test.year || result.Resolution != test.resolution || result.Source != test.source || result.Episodes.Count != test.count {
			t.Errorf("%q => %+v err=%v", test.raw, result, err)
		}
	}
	result, err := Parse("Example.S00E00.1080p.H.264.DTS-HD.MA.HDR10+", "")
	if err != nil || result.VideoCodec != "H.264" || result.AudioCodec != "DTS-HD.MA" || result.Effect != "HDR10+" || result.Episodes.Season == nil || *result.Episodes.Season != 0 || result.Episodes.Episode == nil || *result.Episodes.Episode != 0 {
		t.Fatalf("quality or special episode=%+v err=%v", result, err)
	}
}
