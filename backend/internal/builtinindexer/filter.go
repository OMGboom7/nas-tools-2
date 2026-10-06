package builtinindexer

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
)

type Filter struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type fieldFilter struct {
	apply func(string) (string, error)
}

func stringArgument(raw json.RawMessage) (string, error) {
	if strings.TrimSpace(string(raw)) == "null" {
		return "", ErrConfig
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", ErrConfig
	}
	return value, nil
}

func pairArguments(raw json.RawMessage) ([]json.RawMessage, error) {
	var value []json.RawMessage
	if json.Unmarshal(raw, &value) != nil || len(value) != 2 {
		return nil, ErrConfig
	}
	return value, nil
}

// Compile all operations even for an empty extracted field: unsupported rules
// must never silently become an apparently successful empty search.
func compileFilters(filters []Filter) ([]fieldFilter, error) {
	return compileFiltersAt(filters, time.Now())
}

func compileFiltersAt(filters []Filter, now time.Time) ([]fieldFilter, error) {
	if len(filters) > 32 {
		return nil, ErrConfig
	}
	compiled := make([]fieldFilter, 0, len(filters))
	for _, filter := range filters {
		if len(filter.Args) > 8192 {
			return nil, ErrConfig
		}
		var apply func(string) (string, error)
		switch filter.Name {
		case "strip":
			if len(filter.Args) != 0 && string(filter.Args) != "null" {
				return nil, ErrConfig
			}
			apply = func(value string) (string, error) { return strings.TrimSpace(value), nil }
		case "appendleft", "querystring":
			arg, err := stringArgument(filter.Args)
			if err != nil {
				return nil, err
			}
			if filter.Name == "appendleft" {
				apply = func(value string) (string, error) {
					if len(arg)+len(value) > 4<<20 {
						return "", ErrResponse
					}
					return arg + value, nil
				}
			} else {
				if arg == "" {
					return nil, ErrConfig
				}
				apply = func(value string) (string, error) {
					query := strings.TrimPrefix(value, "?")
					if pos := strings.IndexByte(query, '?'); pos >= 0 {
						query = query[pos+1:]
					}
					if pos := strings.IndexByte(query, '#'); pos >= 0 {
						query = query[:pos]
					}
					values, err := url.ParseQuery(query)
					if err != nil {
						return "", ErrResponse
					}
					return values.Get(arg), nil
				}
			}
		case "lstrip":
			var args []string
			if json.Unmarshal(filter.Args, &args) != nil || len(args) != 1 {
				return nil, ErrConfig
			}
			apply = func(value string) (string, error) { return strings.TrimLeft(value, args[0]), nil }
		case "replace", "split", "re_search":
			args, err := pairArguments(filter.Args)
			if err != nil {
				return nil, err
			}
			first, err := stringArgument(args[0])
			if err != nil {
				return nil, err
			}
			if filter.Name == "replace" {
				last, err := stringArgument(args[1])
				if err != nil {
					return nil, err
				}
				apply = func(value string) (string, error) {
					growth := len(last) - len(first)
					if growth > 0 && strings.Count(value, first) > ((4<<20)-len(value))/growth {
						return "", ErrResponse
					}
					return strings.ReplaceAll(value, first, last), nil
				}
			} else {
				var index int
				if json.Unmarshal(args[1], &index) != nil {
					return nil, ErrConfig
				}
				if filter.Name == "split" {
					if first == "" {
						return nil, ErrConfig
					}
					apply = func(value string) (string, error) {
						parts := strings.Split(value, first)
						pos := index
						if pos < 0 {
							pos += len(parts)
						}
						if pos < 0 || pos >= len(parts) {
							return "", ErrResponse
						}
						return parts[pos], nil
					}
				} else {
					re, err := regexcompat.Compile(first, 0)
					if err != nil {
						return nil, ErrConfig
					}
					valid := false
					for _, number := range re.GetGroupNumbers() {
						if number == index {
							valid = true
						}
					}
					if !valid {
						return nil, ErrConfig
					}
					apply = func(value string) (string, error) {
						match, err := re.FindStringMatch(value)
						if err != nil {
							return "", ErrResponse
						}
						if match == nil {
							return "", nil
						}
						return match.GroupByNumber(index).String(), nil
					}
				}
			}
		case "dateparse":
			format, err := stringArgument(filter.Args)
			if err != nil {
				return nil, err
			}
			layout, err := dateLayout(format)
			if err != nil {
				return nil, err
			}
			apply = func(value string) (string, error) {
				if strings.EqualFold(strings.TrimSpace(value), "now") {
					if !validDateClock(now) {
						return "", ErrResponse
					}
					return now.Format("2006-01-02 15:04:05"), nil
				}
				parsed, err := time.Parse(layout, value)
				if err != nil {
					return "", ErrResponse
				}
				// Tracker dates are naive wall-clock values. Do not invent a UTC offset.
				return parsed.Format("2006-01-02 15:04:05"), nil
			}
		case "date_elapsed_parse", "date_en_elapsed_parse":
			if len(filter.Args) != 0 && strings.TrimSpace(string(filter.Args)) != "null" {
				return nil, ErrConfig
			}
			english := filter.Name == "date_en_elapsed_parse"
			apply = func(value string) (string, error) { return relativeDate(value, now, english) }
		default:
			return nil, ErrUnsupported
		}
		compiled = append(compiled, fieldFilter{apply: apply})
	}
	return compiled, nil
}

func dateLayout(format string) (string, error) {
	if format == "" || len(format) > 512 {
		return "", ErrConfig
	}
	codes := map[byte]string{'Y': "2006", 'm': "01", 'd': "02", 'H': "15", 'M': "04", 'S': "05", '%': "%"}
	var output strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			output.WriteByte(format[i])
			continue
		}
		i++
		if i >= len(format) {
			return "", ErrConfig
		}
		value, ok := codes[format[i]]
		if !ok {
			return "", ErrUnsupported
		}
		output.WriteString(value)
	}
	return output.String(), nil
}

func applyFilters(ctx context.Context, value string, filters []fieldFilter) (string, error) {
	for _, filter := range filters {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if value == "" {
			break
		}
		var err error
		value, err = filter.apply(value)
		if err != nil {
			return "", err
		}
		if len(value) > 4<<20 {
			return "", ErrResponse
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func FilterValue(ctx context.Context, value string, filters []Filter) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(value) > 4<<20 {
		return "", ErrResponse
	}
	compiled, err := compileFiltersAt(filters, dateClock(ctx))
	if err != nil {
		return "", err
	}
	return applyFilters(ctx, value, compiled)
}
