package mediameta

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var episodeFormatField = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,31}$`)
var formattedEpisode = regexp.MustCompile(`(?i)^(?:EP)?([0-9]{1,4})(?:-(?:EP)?([0-9]{1,4}))?$`)

// FormattedEpisodes treats the template as literal text and named placeholders,
// never as a user-supplied regexp or executable expression. Matching is against
// the whole filename, including its extension, with exactly one {ep} field.
var ErrEpisodeTemplateNoMatch = errors.New("episode template does not match selected filename")

type FilenameEpisodeFormat struct{ pattern *regexp.Regexp }

func FormattedEpisodes(filename, format string, offset int) (int, *int, error) {
	compiled, err := CompileEpisodeFormat(format)
	if err != nil {
		return 0, nil, err
	}
	return compiled.Episodes(filename, offset)
}

func CompileEpisodeFormat(format string) (*FilenameEpisodeFormat, error) {
	if len(format) == 0 || len(format) > 512 || !utf8.ValidString(format) {
		return nil, errors.New("invalid episode filename template")
	}
	var pattern strings.Builder
	pattern.WriteString(`\A`)
	fields := map[string]bool{}
	previousField := false
	for i := 0; i < len(format); {
		switch format[i] {
		case '{':
			end := strings.IndexByte(format[i+1:], '}')
			if end < 0 {
				return nil, errors.New("unclosed episode template field")
			}
			end += i + 1
			name := format[i+1 : end]
			if !episodeFormatField.MatchString(name) || fields[name] || len(fields) >= 16 || previousField {
				return nil, errors.New("invalid, repeated or adjacent episode template fields")
			}
			fields[name] = true
			if name == "ep" {
				pattern.WriteString(`(?P<episode>(?i:EP)?[0-9]{1,4}(?:-(?i:EP)?[0-9]{1,4})?)`)
			} else {
				pattern.WriteString(`(?s:.+?)`)
			}
			previousField = true
			i = end + 1
		case '}':
			return nil, errors.New("unmatched episode template brace")
		default:
			end := i + 1
			for end < len(format) && format[end] != '{' && format[end] != '}' {
				end++
			}
			pattern.WriteString(regexp.QuoteMeta(format[i:end]))
			previousField = false
			i = end
		}
	}
	if !fields["ep"] {
		return nil, errors.New("episode template requires {ep}")
	}
	pattern.WriteString(`\z`)
	re, err := regexp.Compile(pattern.String())
	if err != nil {
		return nil, errors.New("invalid episode filename template")
	}
	return &FilenameEpisodeFormat{pattern: re}, nil
}

func (f *FilenameEpisodeFormat) Episodes(filename string, offset int) (int, *int, error) {
	if f == nil || f.pattern == nil || len(filename) > 64<<10 || !utf8.ValidString(filename) {
		return 0, nil, errors.New("invalid episode filename")
	}
	match := f.pattern.FindStringSubmatch(filename)
	if match == nil {
		return 0, nil, ErrEpisodeTemplateNoMatch
	}
	numbers := formattedEpisode.FindStringSubmatch(match[f.pattern.SubexpIndex("episode")])
	start, _ := strconv.Atoi(numbers[1])
	var end *int
	if numbers[2] != "" {
		n, _ := strconv.Atoi(numbers[2])
		end = &n
	}
	return OffsetEpisodes(start, end, offset)
}

func OffsetEpisodes(start int, end *int, offset int) (int, *int, error) {
	if start < 1 || start > 9999 || offset < -99999 || offset > 99999 || end != nil && (*end < start || *end > 9999) {
		return 0, nil, errors.New("invalid episode range or offset")
	}
	start += offset
	if start < 1 || start > 9999 {
		return 0, nil, errors.New("offset episode is outside supported range")
	}
	if end != nil {
		n := *end + offset
		if n < start || n > 9999 {
			return 0, nil, errors.New("offset episode range is outside supported range")
		}
		end = &n
	}
	return start, end, nil
}
