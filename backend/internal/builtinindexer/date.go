package builtinindexer

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type dateClockKey struct{}

func dateClock(ctx context.Context) time.Time {
	if now, ok := ctx.Value(dateClockKey{}).(time.Time); ok {
		return now
	}
	return time.Now()
}

func validDateClock(now time.Time) bool {
	return !now.IsZero() && now.Year() >= 1 && now.Year() <= 9999
}

func ResolveFieldsAt(ctx context.Context, root *html.Node, rules map[string]FieldSpec, now time.Time) (map[string]string, error) {
	if !validDateClock(now) {
		return nil, ErrConfig
	}
	return ResolveFields(context.WithValue(ctx, dateClockKey{}, now), root, rules)
}

func FilterValueAt(ctx context.Context, value string, filters []Filter, now time.Time) (string, error) {
	if !validDateClock(now) {
		return "", ErrConfig
	}
	return FilterValue(context.WithValue(ctx, dateClockKey{}, now), value, filters)
}

var englishInterval = regexp.MustCompile(`([0-9]+)\s*(weeks?|days?|hours?|hrs?|minutes?|mins?|seconds?|secs?)`)
var chineseInterval = regexp.MustCompile(`([0-9]+)\s*(小时|小時|分钟|分鐘|星期|天|日|時|时|分|秒|周|週)`)

func relativeDate(raw string, now time.Time, english bool) (string, error) {
	if !validDateClock(now) || len(raw) > 1024 {
		return "", ErrResponse
	}
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "now" || value == "just now" || value == "刚刚" || value == "剛剛" {
		return now.Format("2006-01-02 15:04:05"), nil
	}
	pattern := chineseInterval
	sign := 1
	units := map[string]int64{"星期": 604800, "周": 604800, "週": 604800, "天": 86400, "日": 86400, "小时": 3600, "小時": 3600, "時": 3600, "时": 3600, "分钟": 60, "分鐘": 60, "分": 60, "秒": 1}
	if english {
		pattern = englishInterval
		sign = -1
		units = map[string]int64{"week": 604800, "day": 86400, "hour": 3600, "hr": 3600, "minute": 60, "min": 60, "second": 1, "sec": 1}
		if strings.HasSuffix(value, " ago") {
			value = strings.TrimSpace(strings.TrimSuffix(value, " ago"))
		}
	} else if strings.HasSuffix(value, "前") {
		sign = -1
		value = strings.TrimSpace(strings.TrimSuffix(value, "前"))
	} else if strings.HasSuffix(value, "后") || strings.HasSuffix(value, "後") {
		value = strings.TrimSpace(strings.TrimRight(value, "后後"))
	}
	matches := pattern.FindAllStringSubmatchIndex(value, -1)
	if len(matches) == 0 || len(matches) > 8 {
		return "", ErrResponse
	}
	seen := map[int64]bool{}
	seconds := int64(0)
	previous := 0
	for _, match := range matches {
		gap := strings.TrimSpace(value[previous:match[0]])
		if previous == 0 && gap != "" {
			return "", ErrResponse
		}
		if gap != "" && gap != "," && !(english && gap == "and") {
			return "", ErrResponse
		}
		count, err := strconv.ParseInt(value[match[2]:match[3]], 10, 64)
		if err != nil {
			return "", ErrResponse
		}
		unit := value[match[4]:match[5]]
		if english {
			unit = strings.TrimSuffix(unit, "s")
		}
		multiplier, ok := units[unit]
		if !ok || seen[multiplier] || count > 3153600000/multiplier {
			return "", ErrResponse
		}
		seen[multiplier] = true
		seconds += count * multiplier
		if seconds > 3153600000 {
			return "", ErrResponse
		}
		previous = match[1]
	}
	if strings.TrimSpace(value[previous:]) != "" {
		return "", ErrResponse
	}
	result := now.Add(time.Duration(sign) * time.Duration(seconds) * time.Second)
	if !validDateClock(result) {
		return "", ErrResponse
	}
	return result.Format("2006-01-02 15:04:05"), nil
}
