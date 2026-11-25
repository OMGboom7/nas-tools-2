package externalindexer

import (
	"context"
	"encoding/json"
	"io"
	"strings"
)

// PluginCommand shares discovery and search with the transitional plugin host.
// Its stdin and stdout can contain credentials and private download URLs.
func PluginCommand(ctx context.Context, action string, in io.Reader, out io.Writer) error {
	var q struct {
		Config          Config
		Domain, Keyword string
		Page            int
	}
	input := &io.LimitedReader{R: in, N: 32 << 10}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrConfig
	}
	encoder := json.NewEncoder(out)
	switch action {
	case "discover":
		indexers, err := Discover(ctx, q.Config, nil)
		if err != nil {
			return err
		}
		return encoder.Encode(indexers)
	case "search":
		base, err := baseURL(q.Config)
		if err != nil {
			return err
		}
		// Old snapshots identify an indexer by its discovery URL. Accept only
		// an endpoint constructed from the current settings and a valid ID.
		path := strings.TrimSuffix(q.Domain, "/results/torznab/")
		pos := strings.LastIndexByte(path, '/')
		remoteID := path[pos+1:]
		if !validRemoteID(q.Config.Kind, remoteID) || indexerEndpoint(base, q.Config.Kind, remoteID).String() != q.Domain {
			return ErrConfig
		}
		indexer := Indexer{Kind: q.Config.Kind, RemoteID: remoteID}
		items, err := searchRows(ctx, q.Config, indexer, q.Keyword, &q.Page, nil)
		if err != nil {
			return err
		}
		rows := make([]map[string]any, 0, len(items))
		for _, item := range items {
			var imdb any
			if q.Config.Kind == "Jackett" {
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
