package mediameta

import "testing"

func TestFormattedEpisodes(t *testing.T) {
	for _, tc := range []struct {
		name, format       string
		offset, start, end int
	}{
		{"(BD)十二国記 第45話「東の海神」(1440x1080).mkv", "(BD)十二国記 第{ep}話{a}(1440x1080).mkv", -44, 1, 0},
		{"Show.EP0011-EP0013.mkv", "Show.{ep}.mkv", -10, 1, 3},
		{"Show.0002.mkv", "Show.{ep}.mkv", 1, 3, 0},
		{"[Group] Show EP2.mkv", "[{team}] Show {ep}.mkv", 0, 2, 0},
	} {
		start, end, err := FormattedEpisodes(tc.name, tc.format, tc.offset)
		if err != nil || start != tc.start || tc.end == 0 && end != nil || tc.end != 0 && (end == nil || *end != tc.end) {
			t.Fatal(tc, start, end, err)
		}
	}
}

func TestFormattedEpisodesRejectInvalidRules(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		offset       int
	}{
		{"Show.1.mkv", "Show.{ep}.mkv", -1},
		{"Show.9999.mkv", "Show.{ep}.mkv", 1},
		{"Show.0.mkv", "Show.{ep}.mkv", 1},
		{"Show.3-1.mkv", "Show.{ep}.mkv", 0},
		{"Show.12345.mkv", "Show.{ep}.mkv", 0},
		{"Show.1.mkv.bak", "Show.{ep}.mkv", 0},
		{"Show.1.mkv", "Show.{ep:d}.mkv", 0},
		{"Show.1.mkv", "Show.{a}.mkv", 0},
		{"Show.1.mkv", "Show.{ep}{a}.mkv", 0},
		{"Show.1.mkv", "Show.{ep}.{ep}.mkv", 0},
		{"Show.1.mkv", "Show.{ep.mkv", 0},
		{"Show.1.mkv", "Show.{ep}}.mkv", 0},
		{"Show.1.mkv", "Show.{ep}.mkv", 100000},
	} {
		if _, _, err := FormattedEpisodes(tc.name, tc.format, tc.offset); err == nil {
			t.Fatal("accepted", tc)
		}
	}
}
