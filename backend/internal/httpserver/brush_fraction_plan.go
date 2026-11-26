package httpserver

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

var errBrushFraction = errors.New("brush file selection unavailable")

type brushFractionRule struct {
	Ranges string `json:"frac_before_range"`
	Ratios string `json:"frac_before_percent"`
	After  string `json:"frac_after_range"`
}

func brushGiB(raw string) (int64, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 33)
	if err != nil || n > math.MaxInt64>>30 {
		return 0, errBrushFraction
	}
	return int64(n) << 30, nil
}

// A threshold applies only above its boundary. The after-range is in GiB;
// available storage and the remaining task quota always cap the final result.
func (r brushFractionRule) limit(size, remaining, available int64) (int64, error) {
	if size < 0 || remaining < 0 || available <= 0 || r == (brushFractionRule{}) {
		return 0, errBrushFraction
	}
	limit := size
	if r.Ranges != "" || r.Ratios != "" {
		bounds, ratios := strings.Split(r.Ranges, ","), strings.Split(r.Ratios, ",")
		if len(bounds) != len(ratios) {
			return 0, errBrushFraction
		}
		last := int64(-1)
		for i, b := range bounds {
			n, e := brushGiB(b)
			ratio, err := strconv.ParseFloat(strings.TrimSpace(ratios[i]), 64)
			if e != nil || err != nil || n <= last || math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
				return 0, errBrushFraction
			}
			if size > n {
				limit = size
				if ratio < 1 {
					limit = int64(float64(size) * ratio)
				}
			}
			last = n
		}
	}
	if r.After != "" {
		l, h, found := strings.Cut(r.After, ",")
		low, e := brushGiB(l)
		high, err := brushGiB(h)
		if !found || e != nil || err != nil || high < low {
			return 0, errBrushFraction
		}
		limit = min(max(limit, low), high)
	}
	limit = min(limit, remaining, available)
	if limit <= 0 {
		return 0, errBrushFraction
	}
	return limit, nil
}

type brushFilePlan struct {
	Size       int64
	Files      []torrentfiles.File
	Priorities map[int]int
}

// Use original file-index order. A large skipped file does not consume the
// budget; smaller files after it can still fit. Classes use qB's 0/1/6/7.
func planBrushFiles(files []torrentfiles.File, limit int64) (brushFilePlan, error) {
	files, err := torrentfiles.Normalize(files)
	p := brushFilePlan{Files: files, Priorities: map[int]int{}}
	if err != nil || limit <= 0 {
		return p, errBrushFraction
	}
	for i, f := range files {
		level := 0
		if !f.Padding && f.Size <= limit-p.Size {
			p.Size += f.Size
			switch {
			case p.Size <= (limit-1)/3:
				level = 7
			case p.Size < limit-limit/3:
				level = 6
			default:
				level = 1
			}
		}
		p.Files[i].Selected = level != 0
		p.Priorities[f.Index] = level
	}
	if p.Size == 0 {
		return p, errBrushFraction
	}
	return p, nil
}
