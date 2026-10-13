package mediameta

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Only template syntax is interpreted. The caller quotes literals and assigns
// internal capture names; user text can never inject a regexp or expression.
func episodeFieldPattern(spec string, episode bool) (string, string, string, error) {
	if spec == "" {
		return `.+?`, "", "", nil
	}
	fill, align := " ", byte(0)
	specifiedFill := false
	runes := []rune(spec)
	if strings.ContainsRune("<>=^", runes[0]) {
		align, spec = byte(runes[0]), string(runes[1:])
	} else if len(runes) > 1 && strings.ContainsRune("<>=^", runes[1]) {
		fill, align, spec = string(runes[0]), byte(runes[1]), string(runes[2:])
		specifiedFill = true
	}
	if len(spec) > 0 && strings.ContainsAny(spec[:1], "+- ") {
		spec = spec[1:]
	}
	if strings.HasPrefix(spec, "0") {
		spec = spec[1:]
	}
	number := func() (int, error) {
		i := 0
		for i < len(spec) && spec[i] >= '0' && spec[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, nil
		}
		n, err := strconv.Atoi(spec[:i])
		spec = spec[i:]
		if err != nil || n > 512 {
			return 0, errors.New("episode template width or precision exceeds 512")
		}
		return n, nil
	}
	width, err := number()
	if err != nil {
		return "", "", "", err
	}
	precision := 0
	if strings.HasPrefix(spec, ".") {
		spec = spec[1:]
		precision, err = number()
		if err != nil || precision == 0 {
			return "", "", "", errors.New("invalid episode template precision")
		}
	}
	kind := spec
	numeric := strings.Contains("nbox%fegd", kind) && len(kind) == 1
	if episode && (numeric || kind == "F") {
		return "", "", "", errors.New("episode capture must remain text, not a numeric conversion")
	}
	core := ""
	switch kind {
	case "":
		low := 1
		if width > 0 {
			low = width
		}
		if precision > 0 {
			if precision < low {
				return "", "", "", errors.New("episode template precision is less than width")
			}
			core = fmt.Sprintf(`.{%d,%d}?`, low, precision)
		} else if width > 0 {
			core = fmt.Sprintf(`.{%d,}?`, width)
		} else {
			core = `.+?`
		}
	case "w":
		core = `[\p{L}\p{N}_]+`
	case "W":
		core = `[^\p{L}\p{N}_]+`
	case "D":
		core = `[^\p{Nd}]+`
	case "s":
		core = `\s+`
	case "S":
		core = `\S+`
	case "l":
		core = `[a-zA-Z]+`
	case "d":
		quantity := "+"
		if width > 0 {
			quantity = fmt.Sprintf(`{1,%d}`, width)
		}
		core = `(?:[0-9]` + quantity + `|0x[0-9a-f]` + quantity + `|0b[01]` + quantity + `|0o[0-7]` + quantity + `)`
	case "n":
		core = `[0-9]{1,3}(?:[,.][0-9]{3})*`
	case "b":
		core = `(?:0b)?[01]+`
	case "o":
		core = `(?:0o)?[0-7]+`
	case "x":
		core = `(?:0x)?[0-9a-f]+`
	case "%":
		core = `[0-9]+(?:\.[0-9]+)?%`
	case "f", "F":
		core = `[0-9]*\.[0-9]+`
	case "e":
		core = `(?:[0-9]*\.[0-9]+e[-+]?[0-9]+|nan|inf)`
	case "g":
		core = `(?:[0-9]+(?:\.[0-9]+)?(?:e[-+]?[0-9]+)?|nan|inf)`
	default:
		return "", "", "", errors.New("unsupported episode template field type")
	}
	if numeric {
		if align == '=' {
			if !specifiedFill {
				fill = "0"
			}
			core = regexp.QuoteMeta(fill) + "*" + core
		}
		core = `[-+ ]?` + core
	}
	before, after := "", ""
	if width > 0 {
		if align == 0 {
			align = '>'
		}
		padding := regexp.QuoteMeta(fill) + "*"
		switch align {
		case '<':
			after = padding
		case '>':
			before = padding
		case '^':
			before, after = padding, padding
		}
	}
	return core, before, after, nil
}
