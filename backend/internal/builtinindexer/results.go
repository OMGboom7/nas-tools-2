package builtinindexer

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"golang.org/x/net/html"
)

type ResultOptions struct {
	Now   time.Time
	Limit int
	// A site-specific empty-result marker is required to accept zero rows.
	// Merely finding no torrents does not prove the page was authenticated.
	EmptySelector string
}

type torrentRules struct {
	List struct {
		Selector string `json:"selector"`
	} `json:"list"`
	Fields map[string]FieldSpec `json:"fields"`
}

func ParseResults(ctx context.Context, plan Plan, body []byte, options ResultOptions) ([]externalindexer.Resource, error) {
	if plan.URL == nil || plan.Definition.ID == "" || options.Limit < 1 || options.Limit > 2000 || !validDateClock(options.Now) {
		return nil, ErrConfig
	}
	base, err := url.Parse(plan.Definition.Domain)
	if err != nil || base.User != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") || !sameOrigin(base, plan.URL) {
		return nil, ErrConfig
	}
	var rules torrentRules
	if json.Unmarshal(plan.Definition.Torrents, &rules) != nil || len(rules.Fields) == 0 {
		return nil, ErrConfig
	}
	if plan.Definition.Parser == "HaiDanSpider" {
		normalizeHaiDanID(rules.Fields)
	}
	doc, err := ParseDocument(ctx, body)
	if err != nil {
		return nil, err
	}
	blocked, err := trackerLoginPage(ctx, doc)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, ErrResponse
	}
	rows, err := Select(ctx, doc.Root, rules.List.Selector)
	if err != nil {
		return nil, err
	}
	groups := map[*html.Node]*html.Node{}
	if plan.Definition.Parser == "HaiDanSpider" {
		torrents := []*html.Node{}
		for _, group := range rows {
			children, err := Select(ctx, group, "div.torrent_wrap")
			if err != nil {
				return nil, err
			}
			for _, child := range children {
				if groups[child] != nil {
					return nil, ErrResponse
				}
				groups[child] = group
				torrents = append(torrents, child)
			}
		}
		if len(rows) != 0 && len(torrents) == 0 {
			return nil, ErrResponse
		}
		rows = torrents
	}
	resources := []externalindexer.Resource{}
	if len(rows) == 0 {
		if options.EmptySelector == "" {
			return nil, ErrResponse
		}
		markers, err := Select(ctx, doc.Root, options.EmptySelector)
		if err != nil {
			return nil, err
		}
		if len(markers) == 0 {
			return nil, ErrResponse
		}
		return resources, nil
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		roots := map[string]*html.Node{}
		if group := groups[row]; group != nil {
			roots["title_optional"] = group
		}
		fields, err := resolveFields(context.WithValue(ctx, dateClockKey{}, options.Now), row, rules.Fields, roots)
		if err != nil {
			return nil, err
		}
		title := strings.TrimSpace(fields["title"])
		// Header/separator rows must not conceal broken resource definitions.
		if title == "" && fields["download"] == "" {
			continue
		}
		if title == "" || len(title) > 4096 || len(fields["description"]) > 64<<10 {
			return nil, ErrResponse
		}
		download, err := resultURL(base, fields["download"], true)
		if err != nil {
			return nil, err
		}
		page := ""
		if fields["details"] != "" {
			page, err = resultURL(base, fields["details"], false)
			if err != nil {
				return nil, err
			}
			// Details links can carry credentials too. Retain only safe numeric IDs.
			u, _ := url.Parse(page)
			query := url.Values{}
			for _, key := range []string{"id", "torrentid", "group_id", "torrent_id"} {
				value := u.Query().Get(key)
				if _, err := strconv.ParseUint(value, 10, 64); err == nil {
					query.Set(key, value)
				}
			}
			u.RawQuery = query.Encode()
			page = u.String()
		}
		size, err := resultSize(fields["size"])
		if err != nil {
			return nil, err
		}
		seeders, err := resultCount(fields["seeders"])
		if err != nil {
			return nil, err
		}
		peers, err := resultCount(fields["leechers"])
		if err != nil {
			return nil, err
		}
		downloadFactor, err := resultFactor(fields["downloadvolumefactor"])
		if err != nil {
			return nil, err
		}
		uploadFactor, err := resultFactor(fields["uploadvolumefactor"])
		if err != nil {
			return nil, err
		}
		imdb := fields["imdbid"]
		if imdb != "" && !imdbPattern.MatchString(imdb) {
			return nil, ErrResponse
		}
		resource := externalindexer.Resource{IndexerID: plan.Definition.ID, Indexer: plan.Definition.Name, Title: title, Description: fields["description"], PageURL: page, DownloadURL: download, IMDbID: imdb, Size: size, Seeders: seeders, Peers: peers, DownloadFactor: downloadFactor, UploadFactor: uploadFactor}
		if downloadFactor != nil {
			free := *downloadFactor == 0
			resource.Freeleech = &free
		}
		resources = append(resources, resource)
		if len(resources) >= options.Limit {
			break
		}
	}
	if len(resources) == 0 {
		return nil, ErrResponse
	}
	return resources, nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func resultURL(base *url.URL, raw string, download bool) (string, error) {
	if raw == "" || len(raw) > 16<<10 || strings.ContainsAny(raw, "\r\n\t") {
		return "", ErrResponse
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return "", ErrResponse
	}
	if u.Scheme == "magnet" {
		if !download || !externalindexer.ValidDownloadURL(raw) {
			return "", ErrResponse
		}
		return raw, nil
	}
	u = base.ResolveReference(u)
	if !sameOrigin(base, u) || !externalindexer.ValidDownloadURL(u.String()) {
		return "", ErrResponse
	}
	return u.String(), nil
}

var sizePattern = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB)?$`)
var imdbPattern = regexp.MustCompile(`^tt[0-9]+$`)

func resultSize(raw string) (int64, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\n", ""))
	if raw == "" {
		return 0, ErrResponse
	}
	match := sizePattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, ErrResponse
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, ErrResponse
	}
	powers := map[string]int{"": 0, "B": 0, "KB": 1, "KIB": 1, "MB": 2, "MIB": 2, "GB": 3, "GIB": 3, "TB": 4, "TIB": 4}
	// Legacy tracker sizes use binary units even when labelled GB/MB.
	bytes := number * math.Pow(1024, float64(powers[strings.ToUpper(match[2])]))
	if math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes < 0 || bytes >= float64(math.MaxInt64) {
		return 0, ErrResponse
	}
	return int64(bytes), nil
}

func resultCount(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	value := strings.TrimSpace(strings.SplitN(raw, "/", 2)[0])
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return nil, ErrResponse
	}
	return &number, nil
}

func resultFactor(raw string) (*float64, error) {
	if raw == "" {
		return nil, nil
	}
	number, err := strconv.ParseFloat(raw, 64)
	if err != nil || !validNumericValue(number) {
		return nil, ErrResponse
	}
	return &number, nil
}
