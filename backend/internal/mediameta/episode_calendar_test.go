package mediameta

import (
	"strings"
	"testing"
)

func TestEpisodeCalendarTypesValidateRealDatesAndTimes(t *testing.T) {
	for _, tc := range []struct{ kind, value string }{
		{"ti", "2024-02-29"}, {"ti", "2024-02-29T23:59:59.123456789Z"}, {"ti", "2026-10-13 8:09+09:30"},
		{"ti", "２０２４-０２-２９T２３:５９:５９.１２３４５６+０９:３０"},
		{"tg", "29/February/2024 11:59:59 PM +09:30"}, {"tg", "13-10-2026"},
		{"ta", "February/29/2024 11:59 AM -930"}, {"ta", "10-13-2026"},
		{"te", "Tue, 13 Oct 2026 8:09:10 +0930"}, {"te", "13 Oct 2026 8:09 +09:30"},
		{"th", "13/Oct/2026:8:09:10 -09:30"}, {"th", "13-Oct-2026:8:09 +0930"},
		{"tc", "Tue Oct 13 8:09:10 2026"},
		{"tt", "11:59:59.999999 PM +09:30"}, {"tt", "12:00 AM"}, {"tt", ""}, {"tt", " +0930"},
		{"ts", "Feb 29 8:09:10"}, {"ts", "Oct 13 "},
	} {
		t.Run(tc.kind+"/"+tc.value, func(t *testing.T) {
			if err := validateEpisodeCalendar(tc.kind, tc.value, 2024); err != nil {
				t.Fatal(err)
			}
			if tc.kind == "ts" && strings.HasPrefix(tc.value, "Feb") {
				return
			} // runtime year is deliberately not supplied by filenames.
			start, end, err := FormattedEpisodes("Show."+tc.value+".11.mkv", "Show.{date:"+tc.kind+"}.{ep}.mkv", -10)
			if err != nil || start != 1 || end != nil {
				t.Fatal(start, end, err)
			}
		})
	}
	for _, tc := range []struct{ kind, value string }{
		{"ti", "2026-02-29"}, {"ti", "0000-01-01"}, {"ti", "2024-13-01"}, {"ti", "2024-04-31"},
		{"ti", "2024-02-29T24:00:00Z"}, {"ti", "2024-02-29T23:60:00Z"}, {"ti", "2024-02-29T23:59:60Z"},
		{"ti", "2024-02-29T12:00+24:00"}, {"ti", "2024-02-29T12:00+09:60"},
		{"tg", "29/February/2026"}, {"ta", "February/29/2026"},
		{"te", "31 Apr 2026 8:09 +0930"}, {"th", "31/Apr/2026:8:09 +0930"},
		{"tc", "Tue Apr 31 8:09:10 2026"}, {"tt", "25:00"}, {"tt", "13:00 PM"},
		{"ts", "Feb 30 8:09:10"}, {"ts", "Oct 9999999999999999999999999 "},
	} {
		t.Run("invalid/"+tc.kind+"/"+tc.value, func(t *testing.T) {
			if err := validateEpisodeCalendar(tc.kind, tc.value, 2024); err == nil {
				t.Fatal("accepted impossible calendar", tc)
			}
			if _, _, err := FormattedEpisodes("Show."+tc.value+".11.mkv", "Show.{date:"+tc.kind+"}.{ep}.mkv", 0); err == nil {
				t.Fatal("planned invalid date", tc)
			}
		})
	}
	if err := validateEpisodeCalendar("ts", "Feb 29 ", 2026); err == nil {
		t.Fatal("syslog month/day guessed a leap year")
	}
}

func TestEpisodeCalendarBindingsPaddingRepetitionAndEpisodeSeparation(t *testing.T) {
	for _, tc := range []struct{ name, format string }{
		{"Show.2024-02-29.11.2024-02-29.mkv", "Show.{date:ti}.{ep}.{date:ti}.mkv"},
		{"Show.2024-02-29.2026-10-13.11.mkv", "Show.{a:ti}.{b:ti}.{ep}.mkv"},
		{"Show.2024-02-29.11.mkv", "Show.{:ti}.{ep}.mkv"},
		{"Show.__2024-02-29.11.mkv", "Show.{date:_>12ti}.{ep}.mkv"},
		{"Show.2024-02-29__.11.mkv", "Show.{date:_<12ti}.{ep}.mkv"},
	} {
		start, end, err := FormattedEpisodes(tc.name, tc.format, -10)
		if err != nil || start != 1 || end != nil {
			t.Fatal(tc, start, end, err)
		}
	}
	for _, format := range []string{"Show.{ep:ti}.mkv", "Show.{ep:tt}.mkv", "Show.{date:tz}.{ep}.mkv", "Show.{date:ti}.{ep}.{date:tg}.mkv"} {
		if _, err := CompileEpisodeFormat(format); err == nil {
			t.Fatal("unsupported calendar selector", format)
		}
	}
	if _, _, err := FormattedEpisodes("Show.2024-02-29.11.2024-03-01.mkv", "Show.{date:ti}.{ep}.{date:ti}.mkv", 0); err == nil {
		t.Fatal("repeated calendar field became independent")
	}
}
