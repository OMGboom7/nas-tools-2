package externalindexer

import (
	"context"
	"encoding/json"
	"io"
	"strings"
)

type PluginQuery struct {
	Domain, Keyword string
	Page            int
}

// PluginCommand shares discovery and search with the transitional plugin host.
// Its stdin and stdout can contain credentials and private download URLs.
func PluginCommand(ctx context.Context, action string, in io.Reader, out io.Writer) error {
	var q struct {
		Config Config
		PluginQuery
	}
	input := &io.LimitedReader{R: in, N: 32 << 10}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrConfig
	}
	return RunPluginQuery(ctx, action, q.Config, q.PluginQuery, out)
}

func RunPluginQuery(ctx context.Context, action string, cfg Config, q PluginQuery, out io.Writer) error {
	encoder := json.NewEncoder(out)
	switch action {
	case "discover":
		indexers, err := Discover(ctx, cfg, nil)
		if err != nil {
			return err
		}
		return encoder.Encode(indexers)
	case "search":
		// Old snapshots identify an indexer by its discovery URL. Accept only
		// an endpoint constructed from the current settings and a valid ID.
		path := strings.TrimSuffix(q.Domain, "/results/torznab/")
		pos := strings.LastIndexByte(path, '/')
		remoteID := path[pos+1:]
		endpoint, err := DiscoveryURL(cfg, remoteID)
		if err != nil || endpoint != q.Domain {
			return ErrConfig
		}
		indexer := Indexer{Kind: cfg.Kind, RemoteID: remoteID}
		items, err := searchRows(ctx, cfg, indexer, q.Keyword, &q.Page, nil)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, 0, len(items))
		for _, item := range items {
			var imdb any
			if cfg.Kind == "Jackett" {
				imdb = item.IMDbID
			}
			rows = append(rows, map[string]any{
				"indexer_id": item.sourceID, "indexer": item.sourceName, "title": item.Title,
				"enclosure": item.DownloadURL, "description": item.Description, "size": item.size,
				"seeders": item.seeders, "peers": item.peers, "freeleech": item.Freeleech,
				"downloadvolumefactor": item.download, "uploadvolumefactor": item.upload,
				"page_url": item.PageURL, "imdbid": imdb,
			})
		}
		return encoder.Encode(rows)
	default:
		return ErrConfig
	}
}
