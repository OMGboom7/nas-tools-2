package filterconfig

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/dlclark/regexp2"
)

var ErrInvalidRule = errors.New("invalid filter rule")

// TorrentMetadata is supplied after name recognition and custom-word processing.
// A nil promotion factor means that the tracker did not provide that value.
type TorrentMetadata struct {
	Title, Subtitle string
	SizeBytes       float64
	Movie           bool
	Episodes        int
	UploadFactor    *float64
	DownloadFactor  *float64
}

type MatchResult struct {
	Matched   bool
	Order     int
	GroupName string
	RuleID    int64
}

// Match reloads groups and rules in one snapshot, so edits take effect without
// depending on the old Python process's cached filter configuration.
func (store *Store) Match(ctx context.Context, groupID int64, metadata TorrentMetadata) (MatchResult, error) {
	if groupID == -1 {
		return MatchResult{Matched: true, GroupName: "不过滤"}, nil
	}
	groups, err := store.List(ctx)
	if err != nil {
		return MatchResult{}, err
	}
	return MatchGroups(groups, groupID, metadata)
}

// MatchGroups uses zero for the default group and -1 to bypass filtering.
// Rules must be in priority/ID order, as returned by Store.List and Builtins.
func MatchGroups(groups []GroupInfo, groupID int64, metadata TorrentMetadata) (MatchResult, error) {
	if groupID == -1 {
		return MatchResult{Matched: true, GroupName: "不过滤"}, nil
	}
	if metadata.SizeBytes < 0 || math.IsNaN(metadata.SizeBytes) || math.IsInf(metadata.SizeBytes, 0) || metadata.Episodes < 0 {
		return MatchResult{}, fmt.Errorf("%w: invalid torrent metadata", ErrInvalidRule)
	}
	var selected *GroupInfo
	for index := range groups {
		if groupID == groups[index].Group.ID || groupID == 0 && groups[index].Group.Default {
			selected = &groups[index]
			break
		}
	}
	if selected == nil {
		if groupID == 0 {
			return MatchResult{Matched: true, GroupName: "未配置过滤规则"}, nil
		}
		return MatchResult{}, ErrNotFound
	}
	result := MatchResult{Matched: len(selected.Rules) == 0, GroupName: selected.Group.Name}
	text := metadata.Title
	if metadata.Subtitle != "" {
		text += " " + metadata.Subtitle
	}
	for _, rule := range selected.Rules {
		priority, err := strconv.Atoi(strings.TrimSpace(rule.Priority))
		if rule.Priority == "" {
			priority, err = 0, nil
		}
		if err != nil {
			return MatchResult{}, fmt.Errorf("%w: rule %d priority", ErrInvalidRule, rule.ID)
		}
		matched, err := matchRule(rule, text, metadata)
		if err != nil {
			return MatchResult{}, fmt.Errorf("%w: rule %d", err, rule.ID)
		}
		if matched {
			return MatchResult{Matched: true, Order: 100 - priority, GroupName: selected.Group.Name, RuleID: rule.ID}, nil
		}
	}
	return result, nil
}

func matchRule(rule Rule, text string, metadata TorrentMetadata) (bool, error) {
	for _, include := range strings.Split(rule.Include, "\n") {
		if include == "" {
			continue
		}
		matched, err := rulePatternMatches(include, text)
		if err != nil || !matched {
			return false, err
		}
	}
	// The legacy rule format excludes only when every nonempty exclusion line
	// matches. A single line containing alternation retains normal OR behavior.
	allExcluded, hasExclusion := true, false
	for _, exclude := range strings.Split(rule.Exclude, "\n") {
		if exclude == "" {
			continue
		}
		hasExclusion = true
		matched, err := rulePatternMatches(exclude, text)
		if err != nil {
			return false, err
		}
		allExcluded = allExcluded && matched
	}
	if hasExclusion && allExcluded {
		return false, nil
	}
	if rule.Size != "" && metadata.SizeBytes != 0 && (metadata.Movie || metadata.Episodes > 0) {
		minimum, maximum, err := ruleSizeBounds(rule.Size)
		if err != nil {
			return false, err
		}
		bytes := math.Trunc(metadata.SizeBytes)
		if !metadata.Movie {
			bytes /= float64(metadata.Episodes)
		}
		if bytes < minimum*(1<<30) || bytes > maximum*(1<<30) {
			return false, nil
		}
	}
	if rule.Free != "" && metadata.UploadFactor != nil && metadata.DownloadFactor != nil {
		factors := strings.Fields(rule.Free)
		if len(factors) != 2 {
			return false, fmt.Errorf("%w: promotion factors", ErrInvalidRule)
		}
		minimumUpload, uploadErr := strconv.ParseFloat(factors[0], 64)
		maximumDownload, downloadErr := strconv.ParseFloat(factors[1], 64)
		if uploadErr != nil || downloadErr != nil || !finiteFactor(minimumUpload) || !finiteFactor(maximumDownload) || !finiteFactor(*metadata.UploadFactor) || !finiteFactor(*metadata.DownloadFactor) {
			return false, fmt.Errorf("%w: promotion factors", ErrInvalidRule)
		}
		if minimumUpload > *metadata.UploadFactor || maximumDownload < *metadata.DownloadFactor {
			return false, nil
		}
	}
	return true, nil
}

func finiteFactor(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func rulePatternMatches(pattern, text string) (bool, error) {
	if len(pattern) > 16<<10 || len(text) > 64<<10 {
		return false, fmt.Errorf("%w: regular expression input too large", ErrInvalidRule)
	}
	compiled, err := regexcompat.Compile(strings.TrimSpace(pattern), regexp2.IgnoreCase)
	if err != nil {
		return false, fmt.Errorf("%w: unsupported or invalid regular expression", ErrInvalidRule)
	}
	matched, err := compiled.MatchString(text)
	if err != nil {
		return false, fmt.Errorf("%w: regular expression match timed out", ErrInvalidRule)
	}
	return matched, nil
}

func ruleSizeBounds(raw string) (float64, float64, error) {
	parts := strings.Split(raw, ",")
	if len(parts) > 2 {
		return 0, 0, fmt.Errorf("%w: size range", ErrInvalidRule)
	}
	minimum, maximum := 0.0, 0.0
	for index, part := range parts {
		part = strings.TrimSpace(part)
		value := 0.0
		if part != "" {
			integer, err := strconv.ParseUint(part, 10, 53)
			if err != nil {
				return 0, 0, fmt.Errorf("%w: size range", ErrInvalidRule)
			}
			value = float64(integer)
		}
		if len(parts) == 1 || index == 1 {
			maximum = value
		} else {
			minimum = value
		}
	}
	return minimum, maximum, nil
}
