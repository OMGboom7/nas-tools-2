// Package autosub selects media streams and produces timed subtitle output.
package autosub

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

var ErrSubtitle = errors.New("字幕格式或时间范围无效")

const MaxSubtitle = 16 << 20

type Cue struct {
	Start, End int64
	Text       string
}

var timing = regexp.MustCompile(`^(\d{1,6}):([0-5]\d):([0-5]\d)[,.](\d{3})\s+-->\s+(\d{1,6}):([0-5]\d):([0-5]\d)[,.](\d{3})(?:[ \t].*)?$`)

func milliseconds(v []string) int64 {
	var n [4]int64
	for i := range n {
		n[i], _ = strconv.ParseInt(v[i], 10, 64)
	}
	return ((n[0]*60+n[1])*60+n[2])*1000 + n[3]
}
func Parse(data []byte) ([]Cue, error) {
	if len(data) > MaxSubtitle || !utf8.Valid(data) {
		return nil, ErrSubtitle
	}
	text := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(strings.TrimPrefix(string(data), "\uFEFF"))
	var out []Cue
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); {
		if strings.TrimSpace(lines[i]) == "" {
			i++
			continue
		}
		if _, e := strconv.ParseUint(strings.TrimSpace(lines[i]), 10, 32); e == nil {
			i++
		}
		if i == len(lines) {
			return nil, ErrSubtitle
		}
		t := timing.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if t == nil {
			return nil, ErrSubtitle
		}
		start, end := milliseconds(t[1:5]), milliseconds(t[5:9])
		if end < start {
			return nil, ErrSubtitle
		}
		i++
		first := i
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			i++
		}
		if i > first {
			out = append(out, Cue{start, end, strings.Join(lines[first:i], "\n")})
		}
	}
	if len(out) == 0 {
		return nil, ErrSubtitle
	}
	return out, nil
}
func stamp(ms int64) string {
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
func Encode(cues []Cue) []byte {
	var out strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&out, "%d\n%s --> %s\n%s\n\n", i+1, stamp(c.Start), stamp(c.End), c.Text)
	}
	return []byte(out.String())
}
func noisy(s string) bool {
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}, {"{", "}"}, {"【", "】"}, {"♪", "♪"}, {"♫", "♫"}} {
		if strings.HasPrefix(s, pair[0]) && strings.HasSuffix(s, pair[1]) {
			return true
		}
	}
	return false
}
func plain(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		switch z.Next() {
		case html.TextToken:
			b.Write(z.Text())
		case html.ErrorToken:
			return strings.Join(strings.Fields(b.String()), " ")
		}
	}
}
func MergeEnglish(in []Cue) []Cue {
	var out []Cue
	ended := true
	for _, c := range in {
		c.Text = plain(c.Text)
		if c.Text == "" {
			continue
		}
		noise := noisy(c.Text)
		if ended || noise {
			out = append(out, c)
		} else {
			prev := &out[len(out)-1]
			prev.Text += " " + c.Text
			prev.End = max(prev.End, c.End)
		}
		tail := strings.TrimRight(c.Text, "\"”")
		ended = noise || utf8.RuneCountInString(out[len(out)-1].Text) > 350
		if tail != "" {
			last, _ := utf8.DecodeLastRuneInString(tail)
			ended = ended || strings.ContainsRune(".!?。！？", last)
		}
	}
	return out
}

type TranslateFunc func(context.Context, string) (string, error)

// Bilingual preserves cue times and non-speech cues. Do not publish a partial
// translation when one batch fails; callers can reuse completed batch replies.
func Bilingual(ctx context.Context, data []byte, lang string, call TranslateFunc) ([]byte, error) {
	cues, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if lang == "en" {
		cues = MergeEnglish(cues)
	}
	if len(cues) == 0 {
		return nil, ErrSubtitle
	}
	var batch []int
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		lines := make([]string, len(batch))
		for i, n := range batch {
			lines[i] = strings.Join(strings.Fields(cues[n].Text), " ")
		}
		reply, err := call(ctx, strings.Join(lines, "\n"))
		if err != nil {
			return err
		}
		translated := strings.Split(strings.TrimSpace(strings.ReplaceAll(reply, "\r\n", "\n")), "\n")
		if len(batch) == 1 {
			translated = []string{reply}
		} else if len(translated) != len(batch) {
			translated = make([]string, len(batch))
			for i, line := range lines {
				translated[i], err = call(ctx, line)
				if err != nil {
					return err
				}
			}
		}
		for i, n := range batch {
			v := strings.TrimSpace(strings.Join(strings.Fields(translated[i]), " "))
			if v == "" {
				return ErrSubtitle
			}
			cues[n].Text = v + "\n" + cues[n].Text
		}
		batch = nil
		size = 0
		return nil
	}
	for i, c := range cues {
		if noisy(strings.TrimSpace(c.Text)) {
			continue
		}
		batch = append(batch, i)
		size += utf8.RuneCountInString(c.Text)
		if size >= 1000 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	result := Encode(cues)
	if len(result) > MaxSubtitle {
		return nil, ErrSubtitle
	}
	return result, nil
}
