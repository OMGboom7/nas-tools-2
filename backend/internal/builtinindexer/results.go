package builtinindexer

import (
	"context"
	"encoding/json"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
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

type resultTable struct {
	doc    Document
	rules  torrentRules
	rows   []*html.Node
	groups map[*html.Node]*html.Node
}

func selectResultTable(ctx context.Context, definition indexercatalog.Definition, body []byte) (resultTable, error) {
	var rules torrentRules
	if json.Unmarshal(definition.Torrents, &rules) != nil || len(rules.Fields) == 0 {
		return resultTable{}, ErrConfig
	}
	if definition.Parser == "HaiDanSpider" {
		normalizeHaiDanID(rules.Fields)
	}
	doc, err := ParseDocument(ctx, body)
	if err != nil {
		return resultTable{}, err
	}
	blocked, err := trackerLoginPage(ctx, doc)
	if err != nil {
		return resultTable{}, err
	}
	if blocked {
		return resultTable{}, ErrResponse
	}
	rows, err := Select(ctx, doc.Root, rules.List.Selector)
	if err != nil {
		return resultTable{}, err
	}
	groups := map[*html.Node]*html.Node{}
	if definition.Parser == "HaiDanSpider" {
		torrents := []*html.Node{}
		for _, group := range rows {
			children, err := Select(ctx, group, "div.torrent_wrap")
			if err != nil {
				return resultTable{}, err
			}
			for _, child := range children {
				if groups[child] != nil {
					return resultTable{}, ErrResponse
				}
				groups[child] = group
				torrents = append(torrents, child)
			}
		}
		if len(rows) != 0 && len(torrents) == 0 {
			return resultTable{}, ErrResponse
		}
		rows = torrents
	}
	return resultTable{doc, rules, rows, groups}, nil
}

func ParseResults(ctx context.Context, plan Plan, body []byte, options ResultOptions) ([]externalindexer.Resource, error) {
	if plan.URL == nil || plan.Definition.ID == "" || options.Limit < 1 || options.Limit > 2000 || !validDateClock(options.Now) {
		return nil, ErrConfig
	}
	base, err := url.Parse(plan.Definition.Domain)
	if err != nil || base.User != nil || base.Hostname() == "" || (base.Scheme != "http" && base.Scheme != "https") || !sameOrigin(base, plan.URL) {
		return nil, ErrConfig
	}
	table, err := selectResultTable(ctx, plan.Definition, body)
	if err != nil {
		return nil, err
	}
	resources := []externalindexer.Resource{}
	if len(table.rows) == 0 {
		if options.EmptySelector == "" {
			return nil, ErrResponse
		}
		markers, err := Select(ctx, table.doc.Root, options.EmptySelector)
		if err != nil {
			return nil, err
		}
		if len(markers) == 0 {
			return nil, ErrResponse
		}
		return resources, nil
	}
	for _, row := range table.rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		roots := map[string]*html.Node{}
		if group := table.groups[row]; group != nil {
			roots["title_optional"] = group
		}
		fields, err := resolveFields(context.WithValue(ctx, dateClockKey{}, options.Now), row, table.rules.Fields, roots)
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
		minimumSeedTime, err := resultSeedTime(fields["minimumseedtime"])
		if err != nil {
			return nil, err
		}
		minimumRatio, err := resultFactor(fields["minimumratio"])
		if err != nil {
			return nil, err
		}
		imdb := fields["imdbid"]
		if imdb != "" && !imdbPattern.MatchString(imdb) {
			return nil, ErrResponse
		}
		resource := externalindexer.Resource{IndexerID: plan.Definition.ID, Indexer: plan.Definition.Name, Title: title, Description: fields["description"], PageURL: page, DownloadURL: download, IMDbID: imdb, Size: size, Seeders: seeders, Peers: peers, DownloadFactor: downloadFactor, UploadFactor: uploadFactor}
		resource.MinimumSeedTime, resource.MinimumRatio = minimumSeedTime, minimumRatio
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

var sizePattern = regexp.MustCompile(`(?i)^((?:[0-9]+|[0-9]{1,3}(?:,[0-9]{3})+)(?:\.[0-9]+)?)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB|PB|PiB)?$`)
var imdbPattern = regexp.MustCompile(`^tt[0-9]+$`)

// SizeBytes is the shared bounded tracker/RSS size decoder. Empty or malformed
// values are errors; callers must retain unknown size rather than fabricate one.
func SizeBytes(raw string) (int64, error) { return resultSize(raw) }

func resultSize(raw string) (int64, error) {
	return parseByteCount(raw, false)
}

func parseByteCount(raw string, round bool) (int64, error) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, "\n", ""))
	if len(raw) > 128 {
		return 0, ErrResponse
	}
	match := sizePattern.FindStringSubmatch(raw)
	if match == nil {
		return 0, ErrResponse
	}
	number, ok := new(big.Rat).SetString(strings.ReplaceAll(match[1], ",", ""))
	if !ok {
		return 0, ErrResponse
	}
	power := 0
	switch strings.ToUpper(match[2]) {
	case "KB", "KIB":
		power = 1
	case "MB", "MIB":
		power = 2
	case "GB", "GIB":
		power = 3
	case "TB", "TIB":
		power = 4
	case "PB", "PIB":
		power = 5
	}
	numerator := new(big.Int).Lsh(number.Num(), uint(10*power))
	var amount, remainder big.Int
	amount.QuoRem(numerator, number.Denom(), &remainder)
	if round {
		half := remainder.Lsh(&remainder, 1).Cmp(number.Denom())
		if half > 0 || (half == 0 && amount.Bit(0) != 0) {
			amount.Add(&amount, big.NewInt(1))
		}
	}
	if !amount.IsInt64() {
		return 0, ErrResponse
	}
	return amount.Int64(), nil
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

func resultSeedTime(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	// Seconds must be a non-negative integer, not a count such as "3/5".
	value := strings.TrimSpace(raw)
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return nil, ErrResponse
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds > 1_000_000_000 {
		return nil, ErrResponse
	}
	return &seconds, nil
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
