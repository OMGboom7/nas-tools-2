package builtinindexer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

type numericCase struct {
	selector cascadia.Selector
	value    float64
	fallback bool
}

// Decode with a token stream rather than a map. Tracker rules are ordered;
// the first matching selector wins, including an explicitly placed fallback.
func compileNumericCases(raw json.RawMessage) ([]numericCase, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, ErrConfig
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrConfig
	}
	cases := []numericCase{}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, ErrConfig
		}
		rawSelector, ok := key.(string)
		if !ok || seen[rawSelector] || len(cases) >= 128 {
			return nil, ErrConfig
		}
		seen[rawSelector] = true
		var rawValue json.RawMessage
		if decoder.Decode(&rawValue) != nil || bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
			return nil, ErrConfig
		}
		var value float64
		if json.Unmarshal(rawValue, &value) != nil || !validNumericValue(value) {
			return nil, ErrConfig
		}
		entry := numericCase{value: value, fallback: rawSelector == "*"}
		if !entry.fallback {
			entry.selector, err = compileSelector(rawSelector)
			if err != nil {
				return nil, err
			}
		}
		cases = append(cases, entry)
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(cases) == 0 {
		return nil, ErrConfig
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, ErrConfig
	}
	return cases, nil
}

func validNumericValue(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1e9
}

func NumericCaseValue(ctx context.Context, root *html.Node, raw json.RawMessage) (*float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cases, err := compileNumericCases(raw)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, ErrResponse
	}
	for _, entry := range cases {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matched := entry.fallback || cascadia.Query(root, entry.selector) != nil
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if matched {
			value := entry.value
			return &value, nil
		}
	}
	return nil, nil
}

var numericFieldPattern = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?|\.[0-9]+`)

// Unknown metadata is nil, not a fabricated factor of one. Fractional factors
// remain fractional (the old int conversion discarded the discount).
func NumericFieldValue(ctx context.Context, root *html.Node, field FieldSpec) (*float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(field.Case) != 0 {
		if field.Selector != "" || field.Selectors != "" || len(field.Filters) != 0 {
			return nil, ErrConfig
		}
		return NumericCaseValue(ctx, root, field.Case)
	}
	if field.Selector == "" && field.Selectors == "" {
		return nil, nil
	}
	text, err := FieldValue(ctx, root, field)
	if err != nil || text == "" {
		return nil, err
	}
	span := numericFieldPattern.FindStringIndex(text)
	if span == nil {
		return nil, ErrResponse
	}
	if span[0] > 0 && strings.HasSuffix(strings.TrimSpace(text[:span[0]]), "-") {
		return nil, ErrResponse
	}
	if span[1] < len(text) && strings.ContainsRune(".eE", rune(text[span[1]])) {
		return nil, ErrResponse
	}
	value, err := strconv.ParseFloat(text[span[0]:span[1]], 64)
	if err != nil || !validNumericValue(value) {
		return nil, ErrResponse
	}
	return &value, nil
}
