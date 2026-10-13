package mediameta

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEpisodeTemplateLegacyTextAndTypedFields(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		start, end   int
	}{
		{"show.EP0012.MKV", "SHOW.{ep}.mkv", 12, 0},
		{"Show.１２.mkv", "Show.{ep}.mkv", 12, 0},
		{"Show.EP١٢-EP١٣.mkv", "Show.{ep}.mkv", 12, 13},
		{"Show.𝟙𝟚.mkv", "Show.{ep}.mkv", 12, 0},
		{"甲.乙.Show.12.mkv", "{_a}.{_a}.Show.{ep}.mkv", 12, 0},
		{"日本組.Show.12.日本組.mkv", "{組}.Show.{ep}.{組}.mkv", 12, 0},
		{"{Show}.12.mkv", "{{Show}}.{ep}.mkv", 12, 0},
		{"Group.Show.12.mkv", "{}.Show.{ep}.mkv", 12, 0},
		{"A.B.Show.12.mkv", "{0}.{0}.Show.{ep}.mkv", 12, 0},
		{"Group.Show.12.Group.mkv", "{a}.Show.{ep}.{a}.mkv", 12, 0},
		{"Group.Show.12.group.mkv", "{a}.Show.{ep}.{a}.mkv", 12, 0},
		{"x-1-y-2-x-1-y.mkv", "{a}-{ep}-{a}.mkv", 2, 0},
		{"Show.12.12.mkv", "Show.{ep}.{ep}.mkv", 12, 0},
		{"Show.12-12.mkv", "Show.{ep}.mkv", 12, 0},
		{"Show.0012.mkv", "Show.{ep:4}.mkv", 12, 0},
		{"Show.____0012.mkv", "Show.{ep:_>4}.mkv", 12, 0},
		{"Show.字字0012.mkv", "Show.{ep:字>4}.mkv", 12, 0},
		{"Show.0012___.mkv", "Show.{ep:_<4}.mkv", 12, 0},
		{"Show.__0012__.mkv", "Show.{ep:_^4}.mkv", 12, 0},
		{"Show.12.mkv", "Show.{ep:.4}.mkv", 12, 0},
		{"Team.Show.12.mkv", "{a:l}.Show.{ep:w}.mkv", 12, 0},
		{"日本組.Show.12.mkv", "{a:w}.Show.{ep}.mkv", 12, 0},
		{"2026.Show.12.mkv", "{year:d}.Show.{ep}.mkv", 12, 0},
		{"0xFF.Show.12.mkv", "{code:x}.Show.{ep}.mkv", 12, 0},
		{"1,234.Show.12.mkv", "{count:n}.Show.{ep}.mkv", 12, 0},
		{"0b101.Show.12.mkv", "{code:b}.Show.{ep}.mkv", 12, 0},
		{"0o77.Show.12.mkv", "{code:o}.Show.{ep}.mkv", 12, 0},
		{"25.5%.Show.12.mkv", "{ratio:%}.Show.{ep}.mkv", 12, 0},
		{"-1.5.Show.12.mkv", "{value:f}.Show.{ep}.mkv", 12, 0},
		{"1.5e+3.Show.12.mkv", "{value:e}.Show.{ep}.mkv", 12, 0},
		{"100.Show.12.mkv", "{value:g}.Show.{ep}.mkv", 12, 0},
		{"(Group).12.mkv", "({metadata.team}).{ep}.mkv", 12, 0},
	} {
		t.Run(tc.format+"/"+tc.name, func(t *testing.T) {
			start, end, err := FormattedEpisodes(tc.name, tc.format, 0)
			if err != nil || start != tc.start || tc.end == 0 && end != nil || tc.end != 0 && (end == nil || *end != tc.end) {
				t.Fatal(start, end, err)
			}
		})
	}
}

func TestEpisodeTemplateRejectsInconsistentBindingsAndUnsupportedConversions(t *testing.T) {
	for _, format := range []string{"{a}.Show.{ep}.{a:w}.mkv", "Show.{ep:d}.mkv", "Show.{ep:F}.mkv", "Show.{ep:ti}.mkv", "{a:__import__}.Show.{ep}.mkv", "Show.{ep:999999999}.mkv", "Show.{ep:5.4}.mkv", "{{ep}}.mkv", "{a:}.Show.{ep}.mkv"} {
		if _, err := CompileEpisodeFormat(format); err == nil {
			t.Fatal("accepted unsupported format", format)
		}
	}
	for _, tc := range []struct{ name, format string }{
		{"A.Show.12.B.mkv", "{a}.Show.{ep}.{a}.mkv"},
		{"Show.12.13.mkv", "Show.{ep}.{ep}.mkv"},
		{"Show.12suffix.mkv", "Show.{ep}.mkv"},
		{"year.Show.12.mkv", "{year:d}.Show.{ep}.mkv"},
		{"(literal).12.mkv", "(?P<evil>).{ep}.mkv"},
	} {
		if _, _, err := FormattedEpisodes(tc.name, tc.format, 0); !errors.Is(err, ErrEpisodeTemplateNoMatch) {
			t.Fatal(tc, err)
		}
	}
}

func TestEpisodeTemplateRepeatedMatchBudgetCancellationAndConcurrency(t *testing.T) {
	compiled, err := CompileEpisodeFormat("{a}{b}{c}{a}.{ep}.mkv")
	if err != nil || compiled.repeated == nil {
		t.Fatal(err)
	}
	compiled.repeated.MatchTimeout = time.Nanosecond
	started := time.Now()
	_, _, err = compiled.Episodes(strings.Repeat("a", 5000)+"z.1.mkv", 0)
	if err == nil || errors.Is(err, ErrEpisodeTemplateNoMatch) || time.Since(started) > 3*time.Second {
		t.Fatal("unbounded or reclassified matcher failure", err, time.Since(started))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = compiled.EpisodesContext(ctx, "a.1.mkv", 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	compiled, err = CompileEpisodeFormat("{a}.Show.{ep}.{a}.mkv")
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			start, end, err := compiled.Episodes("Team.Show.12.team.mkv", -10)
			if err != nil || start != 2 || end != nil {
				t.Error(start, end, err)
			}
		}()
	}
	group.Wait()
}
