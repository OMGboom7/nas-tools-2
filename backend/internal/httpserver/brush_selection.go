package httpserver

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
)

type brushSelection struct {
	Rules     map[string]any
	Title     string
	Size      int64
	Published *time.Time
}

// Size limits historically include equality for one-sided tests but exclude
// the endpoints of a range. Seeder and age ranges include both endpoints.
func brushMeasure(raw, kind string, value float64) (bool, error) {
	if raw == "" || raw == "#" {
		return true, nil
	}
	if kind == "peercount" && !strings.Contains(raw, "#") {
		raw = "lt#" + raw
	}
	op, bounds, ok := strings.Cut(raw, "#")
	if !ok || bounds == "" {
		return false, errors.New("invalid brush limit")
	}
	lo, hi, hasMax := strings.Cut(bounds, ",")
	parse := func(s string) (float64, error) {
		if kind == "peercount" {
			n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 63)
			return float64(n), err
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return 0, errors.New("invalid brush threshold")
		}
		return n, nil
	}
	minimum, err := parse(lo)
	if err != nil {
		return false, err
	}
	switch op {
	case "gt", "lt":
		if hasMax {
			return false, errors.New("unexpected brush upper limit")
		}
		if op == "gt" {
			return value > minimum || kind == "size" && value == minimum, nil
		}
		return value < minimum || kind == "size" && value == minimum, nil
	case "bw":
		maximum := math.Inf(1)
		if hasMax {
			maximum, err = parse(hi)
		} else if kind != "peercount" {
			return false, errors.New("missing brush upper limit")
		}
		if err != nil || maximum < minimum {
			return false, errors.New("invalid brush range")
		}
		if kind == "size" {
			return minimum < value && value < maximum, nil
		}
		return minimum <= value && value <= maximum, nil
	}
	return false, errors.New("unknown brush operator")
}

func (s brushSelection) match(a brushAttributes, now time.Time, admit func() bool) string {
	match, err := brushMeasure(text(s.Rules["size"]), "size", float64(s.Size)/(1<<30))
	if err != nil || !match || s.Size < 0 {
		return "skip"
	}
	for _, key := range []string{"include", "exclude"} {
		if pattern := text(s.Rules[key]); pattern != "" {
			re, err := regexcompat.Compile(pattern, 0)
			if err != nil {
				return "skip"
			}
			match, err := re.MatchString(s.Title)
			if err != nil || match == (key == "exclude") {
				return "skip"
			}
		}
	}
	if len(s.Rules) != 0 && !admit() {
		return "retry"
	}
	switch text(s.Rules["free"]) {
	case "":
	case "FREE", "2XFREE":
		if a.Free == nil {
			return "retry"
		}
		if !*a.Free || text(s.Rules["free"]) == "2XFREE" && !a.DoubleFree {
			return "skip"
		}
	default:
		return "skip"
	}
	if hr := s.Rules["hr"]; hr != nil && hr != "" && hr != false {
		if a.HitAndRun == nil {
			return "retry"
		}
		if *a.HitAndRun {
			return "skip"
		}
	}
	if raw := text(s.Rules["peercount"]); raw != "" && raw != "#" {
		if a.Seeders == nil {
			return "retry"
		}
		if match, err := brushMeasure(raw, "peercount", float64(*a.Seeders)); err != nil || !match {
			return "skip"
		}
	}
	if raw := text(s.Rules["pubdate"]); raw != "" && raw != "#" {
		if s.Published == nil {
			return "retry"
		}
		if match, err := brushMeasure(raw, "pubdate", math.Trunc(now.Sub(*s.Published).Seconds())/3600); err != nil || !match {
			return "skip"
		}
	}
	return "match"
}
