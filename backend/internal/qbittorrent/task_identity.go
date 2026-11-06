package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// TaskAddedAt is an incarnation fact, not an existence or completion shortcut.
func (c *Client) TaskAddedAt(ctx context.Context, hash string) (int64, error) {
	if !ValidHash(hash) {
		return 0, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return 0, err
	}
	body, err := c.request(ctx, http.MethodGet, "torrents/info?"+url.Values{"hashes": {hash}, "filter": {"all"}}.Encode(), nil)
	if err != nil {
		return 0, err
	}
	var rows []struct {
		Hash  string `json:"hash"`
		Added int64  `json:"added_on"`
	}
	if json.Unmarshal(body, &rows) != nil || len(rows) != 1 || !strings.EqualFold(rows[0].Hash, hash) || rows[0].Added <= 0 {
		return 0, ErrResponse
	}
	return rows[0].Added, nil
}
