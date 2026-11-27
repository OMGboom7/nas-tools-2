package transmission

import (
	"context"
	"encoding/json"
	"strings"
)

func (c *Client) TransferRates(ctx context.Context) (int64, int64, error) {
	data, modern, err := c.transferRPC(ctx, "session-stats", "session_stats", map[string]any{}, map[string]any{})
	if err != nil {
		return 0, 0, err
	}
	up, down := "uploadSpeed", "downloadSpeed"
	if modern {
		up, down = "upload_speed", "download_speed"
	}
	var u, d int64
	if !transferField(data, up, &u) || !transferField(data, down, &d) || u < 0 || d < 0 {
		return 0, 0, ErrResponse
	}
	return u, d, nil
}

func (c *Client) BrushLabels(ctx context.Context) (all, downloading map[string][]string, err error) {
	old := map[string]any{"fields": []string{"hashString", "status", "labels"}}
	mod := map[string]any{"fields": []string{"hash_string", "status", "labels"}}
	data, modern, err := c.transferRPC(ctx, "torrent-get", "torrent_get", old, mod)
	if err != nil {
		return nil, nil, err
	}
	key := "hashString"
	if modern {
		key = "hash_string"
	}
	var rows []map[string]json.RawMessage
	if !transferField(data, "torrents", &rows) || rows == nil {
		return nil, nil, ErrResponse
	}
	all = map[string][]string{}
	downloading = map[string][]string{}
	for _, row := range rows {
		var id string
		var status int
		var labels []string
		if !transferField(row, key, &id) || !ValidHash(id) || !transferField(row, "status", &status) || status < 0 || status > 6 || !transferField(row, "labels", &labels) || labels == nil {
			return nil, nil, ErrResponse
		}
		id = strings.ToLower(id)
		if _, ok := all[id]; ok {
			return nil, nil, ErrResponse
		}
		all[id] = labels
		if status == 3 || status == 4 {
			downloading[id] = labels
		}
	}
	return all, downloading, nil
}
