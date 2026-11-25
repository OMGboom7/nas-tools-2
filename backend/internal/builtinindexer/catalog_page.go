package builtinindexer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"golang.org/x/net/html"
)

// CatalogPageRequest is used by the local scheduler. A rendered document can
// be parsed separately without supplying the browser's cookies or credentials.
type CatalogPageRequest struct {
	Definition                indexercatalog.Definition
	Keyword, Kind, Cookie, UA string
	Terms                     []string
	Page, Limit               int
	Proxy                     *string
	HTML                      *string
}

func catalogKeyword(q CatalogPageRequest) (string, error) {
	if len(q.Terms) == 0 {
		return q.Keyword, nil
	}
	if q.Keyword != "" || len(q.Terms) > 128 {
		return "", ErrConfig
	}
	var search struct {
		Batch struct {
			Delimiter    string
			SpaceReplace string `json:"space_replace"`
		}
	}
	if json.Unmarshal(q.Definition.Search, &search) != nil {
		return "", ErrConfig
	}
	delimiter, space := search.Batch.Delimiter, search.Batch.SpaceReplace
	if delimiter == "" {
		delimiter = " "
	}
	if len(delimiter) > 64 || len(space) > 64 {
		return "", ErrConfig
	}
	if space == "" {
		space = " "
	}
	terms := make([]string, len(q.Terms))
	for i, term := range q.Terms {
		if len(term) > 1024 {
			return "", ErrConfig
		}
		terms[i] = strings.ReplaceAll(term, " ", space)
	}
	return strings.Join(terms, delimiter), nil
}

// CatalogPlan applies the same batch and paging rules for HTTP and browser reads.
func CatalogPlan(q CatalogPageRequest) (Plan, error) {
	keyword, err := catalogKeyword(q)
	if err != nil {
		return Plan{}, err
	}
	return buildPage(q.Definition, keyword, q.Page, q.Kind, keyword == "", len(q.Terms) > 0)
}

func CatalogPage(ctx context.Context, q CatalogPageRequest, transport http.RoundTripper) ([]any, error) {
	d := q.Definition
	base, err := url.Parse(d.Domain)
	if err != nil || base.User != nil || base.Hostname() == "" || (base.Scheme != "https" && base.Scheme != "http") || base.RawQuery != "" || base.Fragment != "" || d.ID == "" || q.Limit < 1 || q.Limit > 2000 {
		return nil, ErrConfig
	}
	var body []byte
	if q.HTML != nil {
		if q.Cookie != "" || q.Proxy != nil || len(*q.HTML) == 0 {
			return nil, ErrConfig
		}
		body = []byte(*q.HTML)
	} else {
		plan, err := CatalogPlan(q)
		if err != nil {
			return nil, err
		}
		if q.Proxy != nil {
			transport, err = siteconfig.ProxyTransport(transport, *q.Proxy)
			if err != nil {
				return nil, ErrConfig
			}
			defer transport.(*http.Transport).CloseIdleConnections()
		}
		body, err = Fetch(ctx, plan, q.Cookie, q.UA, transport)
		if err != nil {
			return nil, err
		}
	}
	table, err := selectResultTable(ctx, d, body)
	if err != nil {
		return nil, err
	}
	// Legacy definitions resolve links against their configured directory.
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	ctx = context.WithValue(ctx, dateClockKey{}, dateClock(ctx))
	items := []any{}
	for _, row := range table.rows {
		item, err := table.schedulerRow(ctx, d.ID, base, row)
		if err != nil {
			return nil, err
		}
		if item != nil {
			items = append(items, item)
			if len(items) == q.Limit {
				break
			}
		}
	}
	return items, nil
}

var schedulerFields = []string{"title", "description", "details", "download", "size", "seeders", "leechers", "grabs", "imdbid", "date_added", "date_elapsed", "downloadvolumefactor", "uploadvolumefactor"}

func (table resultTable) schedulerRow(ctx context.Context, id string, base *url.URL, row *html.Node) (map[string]any, error) {
	rules := table.rules.Fields
	roots := map[string]*html.Node{"title_optional": table.groups[row]}
	fields, err := resolveFields(ctx, row, rules, roots, schedulerFields...)
	if err != nil {
		return nil, err
	}
	if fields["title"] == "" {
		if fields["download"] != "" {
			return nil, ErrResponse
		}
		return nil, nil
	}
	item := map[string]any{"indexer": id, "title": fields["title"]}
	for field, key := range map[string]string{"description": "description", "imdbid": "imdbid", "date_added": "pubdate", "date_elapsed": "date_elapsed"} {
		if _, ok := rules[field]; ok {
			item[key] = fields[field]
		}
	}
	for field, key := range map[string]string{"download": "enclosure", "details": "page_url"} {
		if raw := fields[field]; raw != "" {
			value, err := resultURL(base, raw, field == "download")
			if err != nil {
				return nil, err
			}
			item[key] = value
		}
	}
	for field, key := range map[string]string{"seeders": "seeders", "leechers": "peers", "grabs": "grabs"} {
		if _, ok := rules[field]; ok {
			value, _, _ := strings.Cut(fields[field], "/")
			if value == "" {
				item[key] = 0
			} else {
				item[key] = strings.TrimSpace(value)
			}
		}
	}
	if raw := fields["size"]; raw != "" {
		size, err := parseByteCount(raw, true)
		if err != nil {
			return nil, err
		}
		item["size"] = size
	}
	for _, key := range []string{"downloadvolumefactor", "uploadvolumefactor"} {
		if rule, ok := rules[key]; ok {
			var factor *float64
			if len(rule.Text) > 0 {
				factor, err = resultFactor(fields[key])
			} else {
				factor, err = NumericFieldValue(ctx, row, rule)
			}
			if err != nil {
				return nil, err
			}
			if factor == nil {
				item[key] = 1
			} else {
				item[key] = *factor
			}
		}
	}
	if rule, ok := rules["labels"]; ok {
		labels, err := Values(ctx, row, rule)
		if err != nil {
			return nil, err
		}
		if len(labels) > 0 {
			item["labels"] = strings.Join(labels, "|")
		}
	}
	return item, nil
}

func CatalogPageCommand(ctx context.Context, in io.Reader, out io.Writer) error {
	// A 4 MiB rendered page may expand during JSON escaping.
	input := &io.LimitedReader{R: in, N: 25 << 20}
	var request CatalogPageRequest
	decoder := json.NewDecoder(input)
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrConfig
	}
	rows, err := CatalogPage(ctx, request, nil)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(rows)
}
