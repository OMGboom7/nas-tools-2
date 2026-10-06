package httpserver

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// APScheduler's five-field crontab uses AND for day and weekday, and Monday=0.
// Do not silently replace those rules with Unix cron's OR/Sunday=0 semantics.
type rssSchedule struct {
	interval time.Duration
	fields   [5]map[int]bool
	lastDay  bool
	location *time.Location
}

var errRSSSchedule = errors.New("invalid or unsupported RSS schedule")

func parseRSSSchedule(raw string, location *time.Location) (rssSchedule, error) {
	s := rssSchedule{location: location}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 512 || location == nil {
		return s, errRSSSchedule
	}
	if rssDigits(raw) {
		minutes, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || minutes == 0 || minutes > uint64((1<<63-1)/int64(time.Minute)) {
			return s, errRSSSchedule
		}
		s.interval = time.Duration(minutes) * time.Minute
		return s, nil
	}
	parts := strings.Fields(strings.ToLower(raw))
	if len(parts) != 5 {
		return s, errRSSSchedule
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, part := range parts {
		names := map[string]int{}
		if i == 3 {
			for n, name := range strings.Fields("jan feb mar apr may jun jul aug sep oct nov dec") {
				names[name] = n + 1
			}
		}
		if i == 4 {
			for n, name := range strings.Fields("mon tue wed thu fri sat sun") {
				names[name] = n
			}
		}
		values := map[int]bool{}
		for _, term := range strings.Split(part, ",") {
			if i == 2 && term == "last" {
				s.lastDay = true
				continue
			}
			steps := strings.Split(term, "/")
			if len(steps) > 2 || steps[0] == "" {
				return s, errRSSSchedule
			}
			step := 1
			if len(steps) == 2 {
				if !rssDigits(steps[1]) {
					return s, errRSSSchedule
				}
				var err error
				step, err = strconv.Atoi(steps[1])
				if err != nil || step < 1 || step > bounds[i][1]-bounds[i][0]+1 {
					return s, errRSSSchedule
				}
			}
			lo, hi := bounds[i][0], bounds[i][1]
			if steps[0] != "*" {
				rangeParts := strings.Split(steps[0], "-")
				if len(rangeParts) > 2 {
					return s, errRSSSchedule
				}
				value := func(raw string) (int, error) {
					if n, ok := names[raw]; ok {
						return n, nil
					}
					if !rssDigits(raw) {
						return 0, errRSSSchedule
					}
					return strconv.Atoi(raw)
				}
				var err error
				lo, err = value(rangeParts[0])
				if err != nil {
					return s, errRSSSchedule
				}
				hi = lo
				if len(rangeParts) == 2 {
					hi, err = value(rangeParts[1])
					if err != nil {
						return s, errRSSSchedule
					}
				} else if len(steps) == 2 {
					hi = bounds[i][1]
				}
			}
			if lo < bounds[i][0] || hi > bounds[i][1] || hi < lo {
				return s, errRSSSchedule
			}
			for n := lo; n <= hi; n += step {
				values[n] = true
			}
		}
		s.fields[i] = values
	}
	return s, nil
}

func rssDigits(raw string) bool {
	if raw == "" {
		return false
	}
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func (s rssSchedule) next(after time.Time) time.Time {
	if s.interval > 0 {
		return after.Add(s.interval)
	}
	local := after.In(s.location)
	day := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, s.location)
	// Calendar-based scanning avoids millions of minute iterations for annual
	// schedules. Forty years covers leap-day + weekday combinations, including
	// the leap-year exception at non-400-divisible century boundaries.
	for i := 0; i < 40*366 && day.Year() <= 9999; i++ {
		year, month, date := day.Date()
		last := day.AddDate(0, 0, 1).Month() != month
		if s.fields[3][int(month)] && (s.fields[2][date] || s.lastDay && last) && s.fields[4][(int(day.Weekday())+6)%7] {
			for hour := 0; hour < 24; hour++ {
				if !s.fields[1][hour] {
					continue
				}
				for minute := 0; minute < 60; minute++ {
					if !s.fields[0][minute] {
						continue
					}
					candidate := time.Date(year, month, date, hour, minute, 0, 0, s.location)
					// A nonexistent local time (DST gap) must not run at a different hour.
					if candidate.Hour() == hour && candidate.Minute() == minute && candidate.Day() == date && candidate.After(after) {
						return candidate
					}
				}
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}
}
