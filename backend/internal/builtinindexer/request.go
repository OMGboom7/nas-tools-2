package builtinindexer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

var ErrConfig = errors.New("invalid builtin indexer search definition")
var ErrUnsupported = errors.New("builtin indexer requires a dedicated request implementation")
var ErrResponse = errors.New("builtin indexer search response is unavailable or invalid")

type Plan struct {
	URL        *url.URL `json:"-"`
	Definition indexercatalog.Definition
}
type searchConfig struct {
	Paths []struct {
		Path   string `json:"path"`
		Method string `json:"method"`
		Type   string `json:"type"`
	} `json:"paths"`
	Params map[string]json.RawMessage `json:"params"`
}
type categoryConfig struct {
	Field     string  `json:"field"`
	Delimiter *string `json:"delimiter"`
	Movie     []struct {
		ID json.RawMessage `json:"id"`
	} `json:"movie"`
	TV []struct {
		ID json.RawMessage `json:"id"`
	} `json:"tv"`
}

func substitute(template string, variables map[string]string) (string, error) {
	var output strings.Builder
	for pos := 0; pos < len(template); {
		if template[pos] == '{' {
			if pos+1 < len(template) && template[pos+1] == '{' {
				output.WriteByte('{')
				pos += 2
				continue
			}
			end := strings.IndexByte(template[pos+1:], '}')
			if end < 0 {
				return "", ErrConfig
			}
			end += pos + 1
			value, ok := variables[template[pos+1:end]]
			if !ok {
				return "", ErrConfig
			}
			output.WriteString(value)
			pos = end + 1
		} else if template[pos] == '}' {
			if pos+1 >= len(template) || template[pos+1] != '}' {
				return "", ErrConfig
			}
			output.WriteByte('}')
			pos += 2
		} else {
			output.WriteByte(template[pos])
			pos++
		}
	}
	return output.String(), nil
}

func scalar(raw json.RawMessage) (string, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return "", ErrConfig
	}
	switch value := value.(type) {
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	case bool:
		if value {
			return "True", nil
		}
		return "False", nil
	default:
		return "", ErrConfig
	}
}

func Build(definition indexercatalog.Definition, keyword string, page int, kind string) (Plan, error) {
	if definition.Parser != "" && definition.Parser != "HaiDanSpider" || definition.Encoding != "" && !strings.EqualFold(definition.Encoding, "UTF-8") {
		return Plan{}, ErrUnsupported
	}
	if page < 0 || page > 10000 || len(keyword) > 1024 || strings.TrimSpace(keyword) == "" || !utf8.ValidString(keyword) || strings.ContainsFunc(keyword, unicode.IsControl) {
		return Plan{}, ErrConfig
	}
	if kind != "" && kind != "movie" && kind != "tv" && kind != "anime" {
		return Plan{}, ErrConfig
	}
	base, err := url.Parse(definition.Domain)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return Plan{}, ErrConfig
	}
	var configuration searchConfig
	if len(definition.Search) > 64<<10 || json.Unmarshal(definition.Search, &configuration) != nil || len(configuration.Paths) == 0 || len(configuration.Paths) > 32 {
		return Plan{}, ErrConfig
	}
	path := -1
	if len(configuration.Paths) == 1 {
		path = 0
	} else {
		wanted := kind
		if wanted == "" {
			wanted = "all"
		}
		for index, item := range configuration.Paths {
			if item.Type == wanted {
				path = index
				break
			}
		}
	}
	if path < 0 {
		return Plan{}, ErrUnsupported
	}
	selected := configuration.Paths[path]
	if selected.Method != "" && !strings.EqualFold(selected.Method, "get") {
		return Plan{}, ErrUnsupported
	}
	if len(selected.Path) > 4096 || strings.HasPrefix(selected.Path, "//") {
		return Plan{}, ErrConfig
	}
	values := url.Values{}
	pathVariables := map[string]string{"keyword": url.QueryEscape(keyword), "page": strconv.Itoa(page)}
	if len(configuration.Params) > 0 {
		values.Set("search_mode", "0")
		values.Set("page", strconv.Itoa(page))
		values.Set("notnewword", "1")
		for key, raw := range configuration.Params {
			if key == "" || len(key) > 128 || strings.ContainsFunc(key, unicode.IsControl) {
				return Plan{}, ErrConfig
			}
			value, err := scalar(raw)
			if err != nil {
				return Plan{}, err
			}
			value, err = substitute(value, map[string]string{"keyword": keyword})
			if err != nil {
				return Plan{}, err
			}
			values.Set(key, value)
		}
		var categories categoryConfig
		if len(definition.Category) > 0 && string(definition.Category) != "null" {
			if json.Unmarshal(definition.Category, &categories) != nil {
				return Plan{}, ErrConfig
			}
			entries := categories.Movie
			if kind == "tv" || kind == "anime" {
				entries = categories.TV
			} else if kind == "" {
				entries = append(append([]struct {
					ID json.RawMessage `json:"id"`
				}{}, categories.Movie...), categories.TV...)
			}
			if len(entries) > 256 {
				return Plan{}, ErrConfig
			}
			delimiter := " "
			if categories.Delimiter != nil {
				delimiter = *categories.Delimiter
			}
			for _, entry := range entries {
				id, err := scalar(entry.ID)
				if err != nil || id == "" || len(id) > 128 {
					return Plan{}, ErrConfig
				}
				if categories.Field == "" {
					values.Set("cat"+id, "1")
				} else {
					values.Set(categories.Field, values.Get(categories.Field)+delimiter+id)
				}
			}
		}
	}
	relative, err := substitute(selected.Path, pathVariables)
	if err != nil {
		return Plan{}, err
	}
	// Keep reverse-proxy prefixes without allowing a rule/keyword to change
	// origin and receive the selected site's authentication cookie.
	address, err := url.Parse(strings.TrimRight(base.String(), "/") + "/" + strings.TrimLeft(relative, "/"))
	if err != nil || address.Host != base.Host || address.Scheme != base.Scheme || address.User != nil || address.Fragment != "" {
		return Plan{}, ErrConfig
	}
	if len(values) > 0 {
		query := address.Query()
		for key, value := range values {
			query[key] = value
		}
		address.RawQuery = query.Encode()
	}
	return Plan{URL: address, Definition: definition}, nil
}

// Fetch does not treat a login HTML page as empty search results. The parser
// must subsequently establish a valid results list before returning success.
func Fetch(ctx context.Context, plan Plan, cookie, userAgent string, transport http.RoundTripper) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if plan.URL == nil || plan.URL.User != nil || plan.URL.Hostname() == "" || (plan.URL.Scheme != "http" && plan.URL.Scheme != "https") {
		return nil, ErrConfig
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, plan.URL.String(), nil)
	if err != nil {
		return nil, ErrConfig
	}
	request.Header.Set("Cookie", cookie)
	request.Header.Set("User-Agent", userAgent)
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrResponse
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, ErrResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 || len(body) == 0 || !utf8.Valid(body) {
		return nil, ErrResponse
	}
	return body, nil
}
