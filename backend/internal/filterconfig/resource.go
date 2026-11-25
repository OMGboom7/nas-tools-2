package filterconfig

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/dlclark/regexp2"
)

var qualityPatterns = map[string]string{"BLURAY": `Blu-?Ray|BD|BDRIP`, "REMUX": "REMUX", "DOLBY": `DOLBY|DOVI|\s+DV$|\s+DV\s+`, "WEB": `WEB-?DL|WEBRIP`, "HDTV": "U?HDTV", "UHD": "UHD", "HDR": "HDR", "3D": "3D"}
var resolutionPatterns = map[string]string{"8k": "8K", "4k": "4K|2160P|X2160", "1080p": "1080[PIX]|X1080", "720p": "720P"}

func ResourcePatterns(quality, resolution string) (string, string, error) {
	q, p := qualityPatterns[quality], resolutionPatterns[resolution]
	if quality != "" && q == "" || resolution != "" && p == "" {
		return "", "", fmt.Errorf("%w: unsupported quality or resolution", ErrInvalidRule)
	}
	return q, p, nil
}

type resourceFilter struct {
	Group                                                           int64
	Metadata                                                        TorrentMetadata
	Original, Edition, Pixels, ReleaseGroup, Separator              string
	Quality, Resolution, Team, Promotion, Include, Exclude, Keyword string
	Upload, Download                                                *float64
}
type resourceDecision struct {
	MatchResult
	Team, Reason string
}

func (q resourceFilter) evaluate(ctx context.Context, groups []GroupInfo) (resourceDecision, error) {
	result := resourceDecision{}
	quality, res, err := ResourcePatterns(q.Quality, q.Resolution)
	if err != nil {
		return result, err
	}
	title := q.Metadata.Title
	if q.Metadata.Subtitle != "" {
		title += " " + q.Metadata.Subtitle
	}
	check := func(pattern, value string, invert bool) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if len(pattern) > 16<<10 || len(value) > 64<<10 {
			return false, ErrInvalidRule
		}
		re, err := regexcompat.Compile(pattern, regexp2.IgnoreCase)
		if err != nil {
			return false, err
		}
		hit, err := re.MatchString(value)
		return hit != invert, err
	}
	type condition struct {
		pattern, value, reason string
		exclude                bool
	}
	testConditions := func(conditions ...condition) error {
		for _, c := range conditions {
			if c.pattern == "" {
				continue
			}
			matched, err := check(c.pattern, c.value, c.exclude)
			if err != nil {
				return err
			}
			if !matched {
				result.Reason = q.Original + " 不符合" + c.reason + " 要求"
				break
			}
		}
		return nil
	}
	err = testConditions(condition{quality, q.Edition, "质量 " + q.Quality, false}, condition{res, q.Pixels, "分辨率 " + q.Resolution, false})
	if err != nil || result.Reason != "" {
		return result, err
	}
	if q.Team != "" {
		if q.ReleaseGroup == "" {
			result.Team, err = mediameta.MatchReleaseGroup(ctx, q.Metadata.Title, q.Team, q.Separator)
			if result.Team == "" {
				result.Reason = q.Original + " 不符合制作组/字幕组 " + q.Team + " 要求"
			}
		} else {
			err = testConditions(condition{q.Team, q.ReleaseGroup, "制作组/字幕组 " + q.Team, false})
		}
		if err != nil || result.Reason != "" {
			return result, err
		}
	}
	if q.Promotion != "" {
		factors := strings.Fields(q.Promotion)
		if len(factors) != 2 {
			return result, ErrInvalidRule
		}
		for i, v := range []*float64{q.Upload, q.Download} {
			if factors[i] == "*" {
				continue
			}
			want, e := strconv.ParseFloat(factors[i], 64)
			if e != nil || !finiteFactor(want) {
				return result, ErrInvalidRule
			}
			if v == nil || *v != want {
				result.Reason = q.Original + " 不符合促销要求"
				return result, nil
			}
		}
	}
	err = testConditions(condition{q.Include, title, "包含 " + q.Include, false}, condition{q.Exclude, title, "排除 " + q.Exclude, true}, condition{q.Keyword, title, " " + q.Keyword, false})
	if err != nil || result.Reason != "" {
		return result, err
	}
	result.MatchResult, err = matchGroupsContext(ctx, groups, q.Group, q.Metadata)
	return result, err
}

type releaseScope struct {
	Seasons, Episodes, WantedSeasons, WantedEpisodes []int
	Year, WantedYear                                 string
}

func (s releaseScope) matches() bool {
	contains := func(wanted, actual []int) bool {
		if len(wanted) == 0 {
			return true
		}
		set := make(map[int]bool, len(wanted))
		for _, n := range wanted {
			set[n] = true
		}
		for _, n := range actual {
			if !set[n] {
				return false
			}
		}
		return true
	}
	return (len(s.WantedSeasons) == 0 || len(s.Seasons) > 0) && contains(s.WantedSeasons, s.Seasons) && contains(s.WantedEpisodes, s.Episodes) && (s.WantedYear == "" || s.WantedYear == s.Year)
}
