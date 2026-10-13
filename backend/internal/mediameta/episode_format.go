package mediameta

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/dlclark/regexp2"
)

var episodeFormatField = regexp.MustCompile(`^(?:[\p{L}\p{N}_]{1,32}(?:\.[\p{L}\p{N}_]+|\[[^\[\]{}:]+\])*)?$`)
var formattedEpisode = regexp.MustCompile(`(?i)^(?:EP)?([\p{Nd}]{1,4})(?:-(?:EP)?([\p{Nd}]{1,4}))?$`)

// FormattedEpisodes treats the template as literal text and named placeholders,
// never as a user-supplied regexp or executable expression. Matching is against
// the whole filename, including its extension, with a text {ep} field and equal-value named repetitions.
var ErrEpisodeTemplateNoMatch = errors.New("episode template does not match selected filename")

type FilenameEpisodeFormat struct {
	pattern  *regexp.Regexp
	repeated *regexp2.Regexp
}

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
	type binding struct{ group, spec string }
	fields := map[string]binding{}
	occurrences := 0
	repeats := false
	for i := 0; i < len(format); {
		if i+1 < len(format) && ((format[i] == '{' && format[i+1] == '{') || (format[i] == '}' && format[i+1] == '}')) {
			pattern.WriteString(regexp.QuoteMeta(format[i : i+1]))
			i += 2
			continue
		}
		switch format[i] {
		case '{':
			end := strings.IndexByte(format[i+1:], '}')
			if end < 0 {
				return nil, errors.New("unclosed episode template field")
			}
			end += i + 1
			field := format[i+1 : end]
			name, spec, typed := strings.Cut(field, ":")
			if !episodeFormatField.MatchString(name) || typed && spec == "" || occurrences >= 16 {
				return nil, errors.New("invalid episode template field or field limit")
			}
			occurrences++
			// Anonymous/positional fields are independent, as in the old parser.
			named := name != "" && unicode.IsLetter([]rune(name)[0])
			if prior, exists := fields[name]; exists && named {
				if prior.spec != spec {
					return nil, errors.New("repeated episode template field has inconsistent format")
				}
				pattern.WriteString("(?P=" + prior.group + ")")
				repeats = true
			} else {
				core, before, after, err := episodeFieldPattern(spec, name == "ep")
				if err != nil {
					return nil, err
				}
				group := fmt.Sprintf("field%d", occurrences)
				if name == "ep" {
					group = "episode"
				}
				pattern.WriteString(before + "(?P<" + group + ">" + core + ")" + after)
				if named {
					fields[name] = binding{group, spec}
				}
			}
			i = end + 1
		case '}':
			return nil, errors.New("unmatched episode template brace")
		default:
			end := i + 1
			for end < len(format) && format[end] != '{' && format[end] != '}' {
				end++
			}
			pattern.WriteString(regexp.QuoteMeta(format[i:end]))
			i = end
		}
	}
	if _, exists := fields["ep"]; !exists {
		return nil, errors.New("episode template requires a text {ep} field")
	}
	pattern.WriteString(`\z`)
	result := &FilenameEpisodeFormat{}
	var err error
	if repeats {
		result.repeated, err = regexcompat.Compile(pattern.String(), regexp2.IgnoreCase|regexp2.Singleline)
	} else {
		result.pattern, err = regexp.Compile("(?is)" + pattern.String())
	}
	if err != nil {
		return nil, errors.New("invalid episode filename template")
	}
	return result, nil
}

func (f *FilenameEpisodeFormat) Episodes(filename string, offset int) (int, *int, error) {
	return f.EpisodesContext(context.Background(), filename, offset)
}

func (f *FilenameEpisodeFormat) EpisodesContext(ctx context.Context, filename string, offset int) (int, *int, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if f == nil || f.pattern == nil && f.repeated == nil || len(filename) > 64<<10 || !utf8.ValidString(filename) {
		return 0, nil, errors.New("invalid episode filename")
	}
	value := ""
	if f.repeated != nil {
		match, err := f.repeated.FindStringMatch(filename)
		if err != nil {
			return 0, nil, errors.New("episode template matching exceeded its budget")
		}
		if err = ctx.Err(); err != nil {
			return 0, nil, err
		}
		if match == nil {
			return 0, nil, ErrEpisodeTemplateNoMatch
		}
		value = match.GroupByName("episode").String()
	} else {
		match := f.pattern.FindStringSubmatch(filename)
		if match == nil {
			return 0, nil, ErrEpisodeTemplateNoMatch
		}
		value = match[f.pattern.SubexpIndex("episode")]
	}
	numbers := formattedEpisode.FindStringSubmatch(value)
	if numbers == nil {
		return 0, nil, ErrEpisodeTemplateNoMatch
	}
	start := episodeDecimalDigits(numbers[1])
	var end *int
	if numbers[2] != "" {
		n := episodeDecimalDigits(numbers[2])
		if n != start {
			end = &n
		}
	}
	return OffsetEpisodes(start, end, offset)
}

// Legacy untyped episode captures accept Unicode decimal digits. Each Nd
// block is a sequence of ten digits; no locale or floating-point conversion.
func episodeDecimalDigits(value string) int {
	number := 0
	for _, digit := range value {
		found := false
		for _, block := range unicode.Digit.R16 {
			if uint32(digit) >= uint32(block.Lo) && uint32(digit) <= uint32(block.Hi) && (uint32(digit)-uint32(block.Lo))%uint32(block.Stride) == 0 {
				number = number*10 + int((uint32(digit)-uint32(block.Lo))/uint32(block.Stride)%10)
				found = true
				break
			}
		}
		if found {
			continue
		}
		for _, block := range unicode.Digit.R32 {
			if uint32(digit) >= block.Lo && uint32(digit) <= block.Hi && (uint32(digit)-block.Lo)%block.Stride == 0 {
				number = number*10 + int((uint32(digit)-block.Lo)/block.Stride%10)
				found = true
				break
			}
		}
		if !found {
			return 0
		}
	}
	return number
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
