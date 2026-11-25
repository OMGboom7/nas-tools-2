package autosub

import (
	"context"
	"strings"
	"testing"
)

func TestSubtitleTimingAndTranslationFallback(t *testing.T) {
	input := []byte("\ufeff1\r\n00:00:00,010 --> 00:00:00,900\r\n<i>First</i>\r\n\r\n2\r\n00:00:01,000 --> 00:00:02,500\r\nsentence.\r\n\r\n3\r\n00:00:02,600 --> 00:00:03,000\r\nSecond!\r\n\r\n4\r\n00:00:03,500 --> 00:00:04,000\r\n[music]")
	calls := 0
	data, err := Bilingual(t.Context(), input, "en", func(_ context.Context, line string) (string, error) {
		calls++
		if strings.Contains(line, "\n") {
			return "wrong line count", nil
		}
		return "译文\n第二行", nil
	})
	if err != nil || calls != 3 {
		t.Fatal(err, calls)
	}
	c, e := Parse(data)
	if e != nil || len(c) != 3 || c[0].Start != 10 || c[0].End != 2500 || c[0].Text != "译文 第二行\nFirst sentence." || c[2].Text != "[music]" {
		t.Fatal(c, e)
	}
}
func TestStreamChoiceSkipsForcedAndBitmapAndKeepsZero(t *testing.T) {
	audio := Stream{Index: 0, Type: "audio"}
	audio.Disposition.Default = 1
	ignored := Stream{Index: 1, Type: "audio"}
	s := Stream{Index: 4, Type: "subtitle"}
	s.Tags.Language = "eng"
	forced := s
	forced.Disposition.Forced = 1
	bitmap := s
	bitmap.Codec = "hdmv_pgs_subtitle"
	tracks := []Stream{audio, ignored, forced, bitmap, s}
	if a, ok := Audio(tracks); !ok || a.Index != 0 {
		t.Fatal(a)
	}
	if sub, n := Subtitle(tracks, "en"); n != 1 || sub.Index != 4 {
		t.Fatal(sub, n)
	}
	for _, c := range []string{"chi", "zho", "zh"} {
		if Lang(c) != "zh" {
			t.Fatal(c)
		}
	}
}

func TestSingleTranslationPreservesWrappedReply(t *testing.T) {
	data, e := Bilingual(t.Context(), []byte("1\n00:00:00,000 --> 00:00:01,000\nUna frase."), "es", func(context.Context, string) (string, error) { return "第一行\n第二行", nil })
	if e != nil || !strings.Contains(string(data), "第一行 第二行\nUna frase.") {
		t.Fatal(string(data), e)
	}
}
