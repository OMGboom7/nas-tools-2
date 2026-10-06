package builtinindexer

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

var templateFieldReference = regexp.MustCompile(`fields\[\s*['"]([A-Za-z_][A-Za-z_0-9]*)['"]\s*\]`)

// Resolve dependencies independently of map iteration order. No rule can read
// partially evaluated fields; cycles and unsupported definitions fail closed.
func ResolveFields(ctx context.Context, root *html.Node, rules map[string]FieldSpec) (map[string]string, error) {
	return resolveFields(ctx, root, rules, nil)
}

func resolveFields(ctx context.Context, root *html.Node, rules map[string]FieldSpec, roots map[string]*html.Node) (map[string]string, error) {
	// Use one instant for the entire resource, not a new clock read per field.
	ctx = context.WithValue(ctx, dateClockKey{}, dateClock(ctx))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, ErrResponse
	}
	if len(rules) > 256 {
		return nil, ErrConfig
	}
	values := map[string]string{}
	state := map[string]int{}
	var resolve func(string, int) error
	resolve = func(name string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > 32 || state[name] == 1 {
			return ErrConfig
		}
		if state[name] == 2 {
			return nil
		}
		rule, exists := rules[name]
		if !exists {
			return nil
		}
		state[name] = 1
		var value string
		var err error
		fieldRoot := root
		if override := roots[name]; override != nil {
			fieldRoot = override
		}
		switch {
		case len(rule.Text) != 0:
			if strings.TrimSpace(string(rule.Text)) == "null" {
				return ErrConfig
			}
			if rule.Selector != "" || rule.Selectors != "" || len(rule.Case) != 0 {
				return ErrConfig
			}
			var raw string
			if json.Unmarshal(rule.Text, &raw) == nil {
				template, err := CompileFieldTemplate(raw)
				if err != nil {
					return err
				}
				for _, reference := range templateFieldReference.FindAllStringSubmatch(raw, -1) {
					if err := resolve(reference[1], depth+1); err != nil {
						return err
					}
				}
				value, err = template.Render(ctx, values)
				if err != nil {
					return err
				}
			} else {
				var number float64
				if json.Unmarshal(rule.Text, &number) != nil || strings.TrimSpace(string(rule.Text)) == "null" || !validNumericValue(number) {
					return ErrConfig
				}
				value = strconv.FormatFloat(number, 'f', -1, 64)
			}
			value, err = FilterValue(ctx, value, rule.Filters)
		case len(rule.Case) != 0:
			var number *float64
			number, err = NumericFieldValue(ctx, fieldRoot, rule)
			if number != nil {
				value = strconv.FormatFloat(*number, 'f', -1, 64)
			}
		default:
			value, err = FieldValue(ctx, fieldRoot, rule)
		}
		if err != nil {
			return err
		}
		values[name] = value
		state[name] = 2
		return nil
	}
	for name := range rules {
		if err := resolve(name, 0); err != nil {
			return nil, err
		}
	}
	return values, nil
}
