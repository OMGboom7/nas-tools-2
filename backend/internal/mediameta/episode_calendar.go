package mediameta

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type episodeCalendarFormat struct {
	lexical  string
	validate *regexp.Regexp
}
type episodeCalendarBinding struct{ group, kind string }

// Date/time fields constrain filename matching, not TMDB identity or episode
// numbers. Generated lexical patterns are paired with semantic validation.
var episodeCalendarFormats = buildEpisodeCalendarFormats()

func buildEpisodeCalendarFormats() map[string]episodeCalendarFormat {
	months := []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	short := []string{}
	all := []string{}
	for _, month := range months {
		short = append(short, month[:3])
		all = append(all, month, month[:3])
	}
	named := func(name, pattern string) string { return "(?P<" + name + ">" + pattern + ")" }
	year := named("year", `[0-9]{4}`)
	day := named("day", `[0-9]{1,2}`)
	month := named("month", `[0-9]{1,2}|`+strings.Join(all, "|"))
	monthShort := named("month", strings.Join(short, "|"))
	weekday := `(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)`
	clock := named("hour", `[0-9]{1,2}`) + ":" + named("minute", `[0-9]{1,2}`) + "(?::" + named("second", `[0-9]{1,2}`) + "(?:\\." + named("fraction", `[0-9]+`) + ")?)?"
	meridian := `(?:\s+` + named("am", `AM|PM`) + `)?`
	zone := named("zone", `[+-][0-9]{1,2}:?[0-9]{2}`)
	patterns := map[string]string{
		"ti": year + "-" + named("month", `[0-9]{2}`) + "-" + named("day", `[0-9]{2}`) + `(?:(?:T|\s+)` + clock + `)?(?:` + named("zone", `Z|\s*[+-][0-9]{2}:?[0-9]{2}`) + `)?`,
		"tg": day + `[-/]` + month + `[-/]` + year + `(?:\s+` + clock + `)?` + meridian + `(?:\s+` + zone + `)?`,
		"ta": month + `[-/]` + day + `[-/]` + year + `(?:\s+` + clock + `)?` + meridian + `(?:\s+` + zone + `)?`,
		"te": `(?:` + weekday + `,\s+)?` + day + `\s+` + monthShort + `\s+` + year + `\s+` + clock + `\s+` + zone,
		"th": day + `[-/]` + monthShort + `[-/]` + year + ":" + clock + `\s+` + zone,
		"tc": weekday + `\s+` + monthShort + `\s+` + day + `\s+` + clock + `\s+` + year,
		"tt": `(?:` + clock + `)?` + meridian + `(?:\s+` + zone + `)?`,
		"ts": monthShort + `\s+` + named("day", `[0-9]+`) + `\s+(?:` + named("hour", `[0-9]{1,2}`) + ":" + named("minute", `[0-9]{1,2}`) + ":" + named("second", `[0-9]{1,2}`) + `)?`,
	}
	result := map[string]episodeCalendarFormat{}
	groupName := regexp.MustCompile(`\(\?P<[a-z]+>`)
	for kind, pattern := range patterns {
		pattern = strings.ReplaceAll(pattern, "[0-9]", `[\p{Nd}]`)
		result[kind] = episodeCalendarFormat{lexical: groupName.ReplaceAllString(pattern, "(?:"), validate: regexp.MustCompile(`(?i)\A` + pattern + `\z`)}
	}
	return result
}

func episodeCalendarKind(spec string) string {
	if len(spec) < 2 {
		return ""
	}
	kind := spec[len(spec)-2:]
	if _, exists := episodeCalendarFormats[kind]; exists {
		return kind
	}
	return ""
}

func validateEpisodeCalendar(kind, value string, referenceYear int) error {
	format, exists := episodeCalendarFormats[kind]
	if !exists {
		return errors.New("unsupported episode calendar format")
	}
	match := format.validate.FindStringSubmatch(value)
	invalid := errors.New("invalid date or time in episode template field")
	if match == nil {
		return invalid
	}
	field := func(name string) string {
		i := format.validate.SubexpIndex(name)
		if i < 0 {
			return ""
		}
		return match[i]
	}
	number := func(name string) (int, bool) {
		value := field(name)
		if value == "" {
			return 0, true
		}
		normalized, valid := calendarDecimalText(value)
		if !valid {
			return 0, false
		}
		n, err := strconv.Atoi(normalized)
		return n, err == nil
	}
	hour, ok := number("hour")
	if !ok {
		return invalid
	}
	minute, ok := number("minute")
	if !ok {
		return invalid
	}
	second, ok := number("second")
	if !ok {
		return invalid
	}
	if strings.EqualFold(field("am"), "AM") && hour == 12 {
		hour = 0
	}
	if strings.EqualFold(field("am"), "PM") && hour != 12 {
		hour += 12
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return invalid
	}
	if fraction := field("fraction"); fraction != "" {
		fraction, valid := calendarDecimalText(fraction)
		if !valid {
			return invalid
		}
		f, err := strconv.ParseFloat("0."+fraction, 64)
		if err != nil || int(f*1000000) > 999999 {
			return invalid
		}
	}
	if zone := strings.TrimSpace(field("zone")); zone != "" && !strings.EqualFold(zone, "Z") {
		digits := strings.ReplaceAll(zone[1:], ":", "")
		var valid bool
		digits, valid = calendarDecimalText(digits)
		if !valid {
			return invalid
		}
		if len(digits) < 3 || len(digits) > 4 {
			return invalid
		}
		h, e1 := strconv.Atoi(digits[:len(digits)-2])
		m, e2 := strconv.Atoi(digits[len(digits)-2:])
		if e1 != nil || e2 != nil || h > 23 || m > 59 {
			return invalid
		}
	}
	if kind == "tt" {
		return nil
	}
	year, ok := number("year")
	if !ok {
		return invalid
	}
	if kind == "ts" {
		year = referenceYear
	}
	if year < 1 || year > 9999 {
		return invalid
	}
	monthText := field("month")
	normalizedMonth, numericMonth := calendarDecimalText(monthText)
	month, err := strconv.Atoi(normalizedMonth)
	if err != nil || !numericMonth {
		month = 0
		for m := time.January; m <= time.December; m++ {
			if strings.EqualFold(monthText, m.String()) || strings.EqualFold(monthText, m.String()[:3]) {
				month = int(m)
				break
			}
		}
	}
	day, ok := number("day")
	if !ok || month < 1 || month > 12 || day < 1 || day > 31 {
		return invalid
	}
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if date.Year() != year || int(date.Month()) != month || date.Day() != day {
		return invalid
	}
	return nil
}

func calendarDecimalText(value string) (string, bool) {
	var normalized strings.Builder
	for _, digit := range value {
		if !unicode.IsDigit(digit) {
			return "", false
		}
		normalized.WriteByte(byte('0' + episodeDecimalDigits(string(digit))))
	}
	return normalized.String(), value != ""
}
