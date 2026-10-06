package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/oliveagle/jsonpath"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/rssparserconfig"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
)

type rssPreviewAPI struct {
	tasks     *rsstaskconfig.Store
	parsers   *rssparserconfig.Store
	config    *config.Store
	transport http.RoundTripper
}

type rssFieldSpec struct {
	Path       string `json:"path"`
	Value      any    `json:"value"`
	Namespaces string `json:"namespaces"`
}

type rssFormat struct {
	List string                  `json:"list"`
	Item map[string]rssFieldSpec `json:"item"`
}

type rssPreviewArticle struct {
	Title        string `json:"title"`
	Link         string `json:"link"`
	Enclosure    string `json:"enclosure"`
	Size         string `json:"size"`
	Description  string `json:"description"`
	Date         string `json:"date"`
	FinishFlag   bool   `json:"finish_flag"`
	Year         string `json:"year"`
	Type         string `json:"type,omitempty"`
	AddressIndex int    `json:"address_index"`
	rawSize      string
}

func (api rssPreviewAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	if api.tasks == nil || api.parsers == nil || api.config == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native RSS preview is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 16<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS preview request")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(request.Form.Get("id")), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS task id")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 25*time.Second)
	defer cancel()
	task, err := api.tasks.Get(ctx, id)
	if errors.Is(err, rsstaskconfig.ErrNotFound) {
		writeJSON(response, http.StatusOK, map[string]any{"code": 1, "msg": "未获取到报文"})
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS task is unavailable")
		return
	}
	configuration, err := api.config.Snapshot()
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS configuration is unavailable")
		return
	}
	addresses := rssStringList(task.Address)
	parserIDs := rssValueList(task.Parser)
	uses := task.Uses
	if uses == "S" {
		uses = "R"
	}
	articles := make([]rssPreviewArticle, 0)
	seen := map[rssPreviewArticle]bool{}
	for index, address := range addresses {
		if index >= len(parserIDs) || strings.TrimSpace(address) == "" {
			continue
		}
		parserID, err := strconv.ParseInt(strings.TrimSpace(text(parserIDs[index])), 10, 64)
		if err != nil || parserID <= 0 {
			continue
		}
		parser, err := api.parsers.Get(ctx, parserID)
		if err != nil {
			continue
		}
		var format rssFormat
		if json.Unmarshal([]byte(parser.Format), &format) != nil || format.List == "" || len(format.Item) == 0 {
			continue
		}
		feed, err := api.fetch(ctx, address, parser.Params, task.Note, configuration)
		if err != nil {
			continue
		}
		items, err := parseRSSPreview(feed, parser.Type, format, index+1)
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.Title == "" {
				continue
			}
			processed, err := api.tasks.IsProcessed(ctx, uses, item.Title, item.Year, item.Enclosure)
			if err != nil {
				writeAPIError(response, http.StatusBadGateway, 502, "RSS article state is unavailable")
				return
			}
			item.FinishFlag = processed
			if !seen[item] {
				articles = append(articles, item)
				seen[item] = true
			}
		}
	}
	if len(articles) == 0 {
		writeJSON(response, http.StatusOK, map[string]any{"code": 1, "msg": "未获取到报文"})
		return
	}
	sort.SliceStable(articles, func(left, right int) bool { return articles[left].Date > articles[right].Date })
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "data": articles, "count": len(articles), "uses": uses, "address_count": len(addresses)})
}

func (api rssPreviewAPI) fetch(ctx context.Context, rawURL, params, note string, configuration map[string]any) ([]byte, error) {
	app := objectValue(configuration["app"])
	if params != "" {
		params = strings.ReplaceAll(params, "{TMDBKEY}", text(app["rmt_tmdbkey"]))
		if strings.ContainsAny(params, "{}") {
			return nil, errors.New("invalid RSS parameters")
		}
		separator := "?"
		if strings.Contains(rawURL, "?") {
			separator = "&"
		}
		rawURL += separator + params
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("invalid RSS URL")
	}
	transport := api.transport
	var options map[string]any
	_ = json.Unmarshal([]byte(note), &options)
	if options["proxy"] == true || options["proxy"] == "Y" || options["proxy"] == "1" {
		transport, err = siteProxyTransport(transport, objectValue(app["proxies"]), parsed.Scheme)
		if err != nil {
			return nil, err
		}
	}
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	result, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RSS request returned %d", result.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("RSS response is too large or unreadable")
	}
	return data, nil
}

func parseRSSPreview(data []byte, kind string, format rssFormat, addressIndex int) ([]rssPreviewArticle, error) {
	items := make([]map[string]string, 0)
	switch strings.ToUpper(kind) {
	case "XML":
		root, err := xmlquery.Parse(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		nodes, err := xmlquery.QueryAll(root, format.List)
		if err != nil {
			return nil, err
		}
		for _, node := range nodes {
			fields := map[string]string{}
			for key, spec := range format.Item {
				if spec.Path != "" {
					var match *xmlquery.Node
					if spec.Namespaces != "" {
						selector, err := xpath.CompileWithNS("//ns:"+spec.Path, map[string]string{"ns": spec.Namespaces})
						if err != nil {
							return nil, err
						}
						match = xmlquery.QuerySelector(node, selector)
					} else {
						var err error
						match, err = xmlquery.Query(node, spec.Path)
						if err != nil {
							return nil, err
						}
					}
					if match != nil {
						fields[key] = match.InnerText()
					}
				} else if spec.Value != nil {
					fields[key] = text(spec.Value)
				}
			}
			items = append(items, fields)
		}
	case "JSON":
		var root any
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, err
		}
		list, err := jsonpath.JsonPathLookup(root, rssJSONPath(format.List))
		if err != nil {
			return nil, err
		}
		array, ok := list.([]any)
		if !ok {
			return nil, errors.New("RSS JSON list is not an array")
		}
		for _, value := range array {
			fields := map[string]string{}
			for key, spec := range format.Item {
				if spec.Path != "" {
					found, err := jsonpath.JsonPathLookup(value, rssJSONPath(spec.Path))
					if err == nil && found != nil {
						if matches, ok := found.([]any); ok {
							if len(matches) == 0 {
								continue
							}
							found = matches[0]
						}
						fields[key] = text(found)
					}
				} else if spec.Value != nil {
					fields[key] = text(spec.Value)
				}
			}
			items = append(items, fields)
		}
	default:
		return nil, errors.New("unsupported RSS parser type")
	}
	result := make([]rssPreviewArticle, 0, len(items))
	for _, fields := range items {
		year := fields["year"]
		if len(year) > 4 {
			year = year[:4]
		}
		result = append(result, rssPreviewArticle{
			Title: fields["title"], Link: fields["link"], Enclosure: fields["enclosure"],
			Size: rssPreviewSize(fields["size"]), Description: fields["description"],
			Date: rssPreviewDate(fields["date"]), Year: year, AddressIndex: addressIndex,
			Type: fields["type"], rawSize: fields["size"],
		})
	}
	return result, nil
}

func rssJSONPath(path string) string {
	if path == "" || path[0] == '$' || path[0] == '@' {
		return path
	}
	if path[0] == '[' || path[0] == '.' {
		return "$" + path
	}
	return "$." + path
}

func rssPreviewDate(raw string) string {
	if raw == "" {
		return ""
	}
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02", "Mon, 02 Jan 2006 15:04:05 MST"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.Format("2006-01-02 15:04:05")
		}
	}
	return raw
}

func rssPreviewSize(raw string) string {
	if raw == "" {
		return ""
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return raw
	}
	if value < 1024 {
		return strconv.FormatFloat(value, 'f', -1, 64) + "B"
	}
	for _, unit := range []string{"K", "M", "G", "T"} {
		value /= 1024
		if value < 1024 {
			return strconv.FormatFloat(float64(int(value*100+0.5))/100, 'f', -1, 64) + unit
		}
	}
	return raw
}
