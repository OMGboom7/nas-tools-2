package httpserver

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Old SQLite rows contain Python's repr of a flat dictionary. Convert only
// quoted literals and the three scalar keywords, then use the JSON decoder.
func brushRuleFields(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	if json.Valid([]byte(raw)) {
		var fields map[string]any
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&fields); err != nil || fields == nil {
			return nil, errBrushDetail
		}
		return fields, nil
	}
	normalized := strings.Builder{}
	for i := 0; i < len(raw); {
		c := raw[i]
		if c == '\'' || c == '"' {
			quote := c
			i++
			var value strings.Builder
			for i < len(raw) && raw[i] != quote {
				r, _, rest, err := strconv.UnquoteChar(raw[i:], quote)
				if err != nil {
					return nil, errBrushDetail
				}
				value.WriteRune(r)
				i = len(raw) - len(rest)
			}
			if i == len(raw) {
				return nil, errBrushDetail
			}
			i++
			encoded, _ := json.Marshal(value.String())
			normalized.Write(encoded)
			continue
		}
		start := i
		for i < len(raw) && ((raw[i] >= 'A' && raw[i] <= 'Z') || (raw[i] >= 'a' && raw[i] <= 'z')) {
			i++
		}
		if i == start {
			normalized.WriteByte(c)
			i++
			continue
		}
		word := raw[start:i]
		switch word {
		case "None":
			word = "null"
		case "True":
			word = "true"
		case "False":
			word = "false"
		}
		normalized.WriteString(word)
	}
	var fields map[string]any
	d := json.NewDecoder(strings.NewReader(normalized.String()))
	d.UseNumber()
	if !json.Valid([]byte(normalized.String())) || d.Decode(&fields) != nil || fields == nil {
		return nil, errBrushDetail
	}
	return fields, nil
}
