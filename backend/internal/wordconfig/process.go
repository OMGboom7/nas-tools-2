package wordconfig

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/dlclark/regexp2"
)

type ProcessResult struct {
	Title                      string
	Ignored, Replaced, Offsets []string
	Warnings                   []string
}

func (store *Store) Process(ctx context.Context, title string) (ProcessResult, error) {
	_, words, err := store.List(ctx)
	if err != nil {
		return ProcessResult{}, err
	}
	return ProcessWords(ctx, title, words)
}

func ProcessWords(ctx context.Context, title string, words []Word) (ProcessResult, error) {
	result := ProcessResult{Title: title, Ignored: []string{}, Replaced: []string{}, Offsets: []string{}, Warnings: []string{}}
	if len(title) > 64<<10 || len(words) > 10000 {
		return result, errors.New("custom-word input too large")
	}
	for _, word := range words {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if word.Enabled != 1 {
			continue
		}
		previous := result.Title
		ignoredCount, replacedCount, offsetCount := len(result.Ignored), len(result.Replaced), len(result.Offsets)
		var changed bool
		var err error
		switch word.Type {
		case 1, 2, 3:
			replacement := word.Replace
			if word.Type == 1 {
				replacement = ""
			}
			if word.Regex == 1 || word.Type == 3 {
				result.Title, changed, err = replaceRegex(ctx, previous, word.Replaced, replacement)
			} else if word.Replaced != "" && strings.Contains(previous, word.Replaced) {
				count := strings.Count(previous, word.Replaced)
				if len(replacement) > 64<<10 || len(previous)+count*(len(replacement)-len(word.Replaced)) > 64<<10 {
					err = errors.New("custom-word result too large")
				} else {
					result.Title = strings.ReplaceAll(previous, word.Replaced, replacement)
					changed = true
				}
			}
			if err == nil && changed && word.Type == 3 {
				var shifted bool
				result.Title, shifted, err = offsetEpisodes(ctx, result.Title, word.Front, word.Back, word.Offset)
				if shifted && err == nil {
					result.Offsets = append(result.Offsets, word.Front+" + "+word.Back+" >> "+word.Offset)
				} else {
					changed = false
				}
			}
			if err == nil && changed {
				if word.Type == 1 {
					result.Ignored = append(result.Ignored, word.Replaced)
				} else {
					result.Replaced = append(result.Replaced, word.Replaced+" ⇒ "+word.Replace)
				}
			}
		case 4:
			result.Title, changed, err = offsetEpisodes(ctx, previous, word.Front, word.Back, word.Offset)
			if err == nil && changed {
				result.Offsets = append(result.Offsets, word.Front+" + "+word.Back+" >> "+word.Offset)
			}
		}
		if len(result.Title) > 64<<10 {
			err = errors.New("custom-word result too large")
		}
		if err != nil {
			if cancellation := ctx.Err(); cancellation != nil {
				return result, cancellation
			}
			result.Title = previous
			result.Ignored = result.Ignored[:ignoredCount]
			result.Replaced = result.Replaced[:replacedCount]
			result.Offsets = result.Offsets[:offsetCount]
			result.Warnings = append(result.Warnings, fmt.Sprintf("识别词 %d 处理失败", word.ID))
		}
	}
	return result, nil
}

func replaceRegex(ctx context.Context, title, pattern, replacement string) (string, bool, error) {
	if len(replacement) > 16<<10 {
		return title, false, errors.New("replacement template too large")
	}
	return substitute(ctx, title, pattern, func(match *regexp2.Match) (string, error) {
		return expandReplacement(replacement, match)
	})
}

// Iterate directly because regexp2 v1's Replace can suppress errors from a
// later FindNextMatch. An error must preserve the original title atomically.
func substitute(ctx context.Context, title, pattern string, replacement func(*regexp2.Match) (string, error)) (string, bool, error) {
	if len(pattern) > 16<<10 || len(title) > 64<<10 {
		return title, false, errors.New("regular expression input too large")
	}
	compiled, err := regexcompat.Compile(pattern, 0)
	if err != nil {
		return title, false, err
	}
	match, err := compiled.FindStringMatch(title)
	if err != nil || match == nil {
		return title, false, err
	}
	runes := []rune(title)
	var output strings.Builder
	previous, count := 0, 0
	started := time.Now()
	for match != nil {
		if err := ctx.Err(); err != nil {
			return title, false, err
		}
		if count >= 1000 || time.Since(started) > time.Second {
			return title, false, errors.New("regular expression replacement limit exceeded")
		}
		value, err := replacement(match)
		if err != nil {
			return title, false, err
		}
		output.WriteString(string(runes[previous:match.Index]))
		output.WriteString(value)
		if output.Len() > 64<<10 {
			return title, false, errors.New("regular expression result too large")
		}
		previous = match.Index + match.Length
		count++
		match, err = compiled.FindNextMatch(match)
		if err != nil {
			return title, false, err
		}
	}
	output.WriteString(string(runes[previous:]))
	return output.String(), true, nil
}

func expandReplacement(template string, match *regexp2.Match) (string, error) {
	var output strings.Builder
	for index := 0; index < len(template); index++ {
		if template[index] != '\\' {
			output.WriteByte(template[index])
			continue
		}
		index++
		if index >= len(template) {
			return "", errors.New("trailing replacement escape")
		}
		var group *regexp2.Group
		if template[index] == 'g' && index+1 < len(template) && template[index+1] == '<' {
			end := strings.IndexByte(template[index+2:], '>')
			if end < 0 {
				return "", errors.New("invalid named replacement")
			}
			name := template[index+2 : index+2+end]
			if number, err := strconv.Atoi(name); err == nil {
				group = match.GroupByNumber(number)
			} else {
				group = match.GroupByName(name)
			}
			index += 2 + end
		} else if template[index] >= '1' && template[index] <= '9' {
			begin := index
			if index+1 < len(template) && template[index+1] >= '0' && template[index+1] <= '9' {
				index++
			}
			number, _ := strconv.Atoi(template[begin : index+1])
			group = match.GroupByNumber(number)
		} else {
			switch template[index] {
			case '\\':
				output.WriteByte('\\')
			case 'n':
				output.WriteByte('\n')
			case 'r':
				output.WriteByte('\r')
			case 't':
				output.WriteByte('\t')
			case 'f':
				output.WriteByte('\f')
			case 'v':
				output.WriteByte('\v')
			case 'a':
				output.WriteByte('\a')
			case 'b':
				output.WriteByte('\b')
			default:
				if unicode.IsLetter(rune(template[index])) {
					return "", errors.New("unknown replacement escape")
				}
				output.WriteByte('\\')
				output.WriteByte(template[index])
			}
			continue
		}
		if group == nil {
			return "", errors.New("replacement group does not exist")
		}
		output.WriteString(group.String())
	}
	return output.String(), nil
}

func offsetEpisodes(ctx context.Context, title, front, back, expression string) (string, bool, error) {
	for _, locator := range []string{front, back} {
		if locator == "" {
			continue
		}
		compiled, err := regexcompat.Compile(locator, 0)
		if err != nil {
			return title, false, err
		}
		matched, err := compiled.MatchString(title)
		if err != nil || !matched {
			return title, false, err
		}
	}
	pattern := "(?<=" + front + ".*?)[0-9一二三四五六七八九十]+(?=.*?" + back + ")"
	return substitute(ctx, title, pattern, func(match *regexp2.Match) (string, error) {
		raw := match.String()
		number, err := episodeNumber(raw)
		if err != nil {
			return "", err
		}
		shifted, err := evaluateOffset(expression, number)
		if err != nil {
			return "", err
		}
		if _, err := strconv.Atoi(raw); err == nil {
			prefix := raw[:len(raw)-len(strings.TrimLeft(raw, "0"))]
			return prefix + strconv.Itoa(shifted), nil
		}
		return chineseEpisode(shifted)
	})
}

func episodeNumber(raw string) (int, error) {
	replacer := strings.NewReplacer("一", "1", "二", "2", "三", "3", "四", "4", "五", "5", "六", "6", "七", "7", "八", "8", "九", "9")
	value := replacer.Replace(raw)
	if parts := strings.Split(value, "十"); len(parts) == 2 {
		tens, ones := 1, 0
		var err error
		if parts[0] != "" {
			tens, err = strconv.Atoi(parts[0])
			if err != nil {
				return 0, err
			}
		}
		if parts[1] != "" {
			ones, err = strconv.Atoi(parts[1])
			if err != nil {
				return 0, err
			}
		}
		return tens*10 + ones, nil
	}
	return strconv.Atoi(value)
}

func chineseEpisode(number int) (string, error) {
	if number < 0 {
		value, err := chineseEpisode(-number)
		return "负" + value, err
	}
	if number > 9999 {
		return "", errors.New("Chinese episode number out of range")
	}
	if number == 0 {
		return "零", nil
	}
	digits, units := []string{"零", "一", "二", "三", "四", "五", "六", "七", "八", "九"}, []string{"", "十", "百", "千"}
	result, zero := "", false
	for place, base := 3, 1000; place >= 0; place, base = place-1, base/10 {
		digit := number / base % 10
		if digit == 0 {
			zero = result != ""
			continue
		}
		if zero {
			result += "零"
			zero = false
		}
		if !(place == 1 && digit == 1 && result == "") {
			result += digits[digit]
		}
		result += units[place]
	}
	return result, nil
}
