package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Ask qBittorrent to apply its own downloading filter across client versions.
func (c *Client) BrushLabels(ctx context.Context, downloading bool) (map[string][]string, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	filter := "all"
	if downloading {
		filter = "downloading"
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/info?filter="+filter, nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Hash string
		Tags *string
	}
	if json.Unmarshal(raw, &rows) != nil || rows == nil {
		return nil, ErrResponse
	}
	result := map[string][]string{}
	for _, r := range rows {
		id := strings.ToLower(r.Hash)
		if _, ok := result[id]; ok || !ValidHash(id) || r.Tags == nil {
			return nil, ErrResponse
		}
		labels := strings.Split(*r.Tags, ",")
		for i := range labels {
			labels[i] = strings.TrimSpace(labels[i])
		}
		result[id] = labels
	}
	return result, nil
}
