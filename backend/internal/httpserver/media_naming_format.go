package httpserver

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

var errLibraryFormat = errors.New("invalid or unsupported library naming format")

// Naming values such as season, year and TMDB ID are strings in the legacy
// format dictionary. Do not coerce them to numbers just because they are digits.
// String format rules: https://docs.python.org/3/library/string.html#formatspec
func renderLocalTemplate(template string, values map[string]string, depth int) (string, error) {
	if len(template) > 16<<10 || !utf8.ValidString(template) || depth > 1 {
		return "", errLibraryFormat
	}
	var result strings.Builder
	for len(template) > 0 {
		index := strings.IndexAny(template, "{}")
		if index < 0 {
			result.WriteString(template)
			break
		}
		result.WriteString(template[:index])
		template = template[index:]
		if len(template) > 1 && template[0] == template[1] {
			result.WriteByte(template[0])
			template = template[2:]
		} else {
			if template[0] != '{' {
				return "", errLibraryFormat
			}
			level, end := 1, -1
			for index := 1; index < len(template); index++ {
				switch template[index] {
				case '{':
					level++
				case '}':
					level--
				}
				if level == 0 {
					end = index
					break
				}
				if level > 2 {
					return "", errLibraryFormat
				}
			}
			if end < 0 {
				return "", errLibraryFormat
			}
			field, spec, _ := strings.Cut(template[1:end], ":")
			name, conversion, converted := strings.Cut(field, "!")
			if converted && conversion != "s" {
				return "", errLibraryFormat
			}
			value, exists := values[name]
			if !exists {
				return "", errLibraryFormat
			}
			if value == "" {
				value = "\t"
			}
			if strings.ContainsAny(spec, "{}") {
				var err error
				spec, err = renderLocalTemplate(spec, values, depth+1)
				if err != nil {
					return "", err
				}
			}
			value, err := formatLocalText(value, spec)
			if err != nil {
				return "", err
			}
			result.WriteString(value)
			template = template[end+1:]
		}
		if result.Len() > 4096 {
			return "", errLibraryFormat
		}
	}
	if result.Len() > 4096 {
		return "", errLibraryFormat
	}
	return result.String(), nil
}

func formatLocalText(value, spec string) (string, error) {
	if len(spec) > 256 || !utf8.ValidString(value) {
		return "", errLibraryFormat
	}
	chars := []rune(spec)
	fill, alignment := ' ', '<'
	fillSpecified := false
	if len(chars) >= 2 && strings.ContainsRune("<>^", chars[1]) {
		fill, alignment, chars = chars[0], chars[1], chars[2:]
		fillSpecified = true
	} else if len(chars) > 0 && strings.ContainsRune("<>^", chars[0]) {
		alignment, chars = chars[0], chars[1:]
	}
	// Python 3.10+ zero padding does not change string alignment.
	if len(chars) > 0 && chars[0] == '0' && !fillSpecified {
		fill = '0'
	}
	width, remaining, err := localFormatNumber(chars)
	if err != nil {
		return "", err
	}
	chars = remaining
	precision := -1
	if len(chars) > 0 && chars[0] == '.' {
		if len(chars) < 2 || chars[1] < '0' || chars[1] > '9' {
			return "", errLibraryFormat
		}
		precision, chars, err = localFormatNumber(chars[1:])
		if err != nil {
			return "", err
		}
	}
	if len(chars) > 1 || len(chars) == 1 && chars[0] != 's' {
		return "", errLibraryFormat
	}
	text := []rune(value)
	if precision >= 0 && precision < len(text) {
		text = text[:precision]
	}
	padding := width - len(text)
	if padding <= 0 {
		return string(text), nil
	}
	left := 0
	if alignment == '>' {
		left = padding
	} else if alignment == '^' {
		left = padding / 2
	}
	result := strings.Repeat(string(fill), left) + string(text) + strings.Repeat(string(fill), padding-left)
	if len(result) > 4096 {
		return "", errLibraryFormat
	}
	return result, nil
}

func localFormatNumber(chars []rune) (int, []rune, error) {
	index := 0
	for index < len(chars) && chars[index] >= '0' && chars[index] <= '9' {
		index++
	}
	if index == 0 {
		return 0, chars, nil
	}
	number, err := strconv.Atoi(string(chars[:index]))
	if err != nil || number > 4096 {
		return 0, nil, errLibraryFormat
	}
	return number, chars[index:], nil
}
