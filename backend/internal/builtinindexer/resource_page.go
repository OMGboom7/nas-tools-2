package builtinindexer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

// ResourcePageRequest carries the selected site's settings from the local
// scheduler or HTTP handler. Callers supply the configured site credentials.
type ResourcePageRequest struct {
	ID, Domain, Name, Cookie, APIKey, UA string
	Keyword                              *string
	Page, Limit                          int
	Proxy                                *string
}

// ResourcePage keeps the original client field names and site-specific values.
func ResourcePage(ctx context.Context, parser string, q ResourcePageRequest, transport http.RoundTripper) ([]any, error) {
	definition := indexercatalog.Definition{ID: q.ID, Name: q.Name, Domain: q.Domain, Parser: parser}
	var err error
	if q.Proxy != nil {
		transport, err = siteconfig.ProxyTransport(transport, *q.Proxy)
		if err != nil {
			return nil, ErrConfig
		}
	}
	rows := []any{}
	switch parser {
	case "MTeamSpider":
		items, err := mteamPage(ctx, definition, q.APIKey, q.UA, q.Keyword, q.Page, transport)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			rows = append(rows, map[string]any{
				"indexer": item.IndexerID, "title": item.Title, "description": item.Description,
				"enclosure": item.DownloadURL, "size": item.size, "seeders": item.seeders, "peers": item.peers,
				"downloadvolumefactor": item.DownloadFactor, "uploadvolumefactor": item.UploadFactor,
				"page_url": item.PageURL, "imdbid": item.IMDbID,
			})
		}
	case "TNodeSpider":
		keyword := ""
		if q.Keyword != nil {
			keyword = *q.Keyword
		}
		items, err := tnodeRows(ctx, definition, q.Cookie, q.UA, keyword, q.Page, q.Limit, transport)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			rows = append(rows, item)
		}
	default:
		return nil, ErrConfig
	}
	return rows, nil
}

// ResourcePageCommand reads private site settings from the local caller's stdin.
func ResourcePageCommand(ctx context.Context, command string, in io.Reader, out io.Writer) error {
	var q ResourcePageRequest
	input := &io.LimitedReader{R: in, N: 256 << 10}
	decoder := json.NewDecoder(input)
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrConfig
	}
	var parser string
	switch command {
	case "tnode-resources":
		parser = "TNodeSpider"
	case "mteam-resources":
		parser = "MTeamSpider"
	default:
		return ErrConfig
	}
	rows, err := ResourcePage(ctx, parser, q, nil)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(rows)
}
