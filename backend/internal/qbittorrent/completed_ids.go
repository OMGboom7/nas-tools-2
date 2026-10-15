package qbittorrent

import (
	"context"
	"encoding/json"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Candidate enumeration is not completion proof; callers re-read Completion.
func (c *Client) CompletedIDs(ctx context.Context) ([]string, error) {
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	body, err := c.request(ctx, http.MethodGet, "torrents/info?filter=completed&limit=1001", nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Hash string `json:"hash"`
	}
	if json.Unmarshal(body, &rows) != nil || rows == nil || len(rows) > 1000 {
		return nil, ErrResponse
	}
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.Hash)
	}
	ids, err = downloadcompletion.NormalizeIDs("qbittorrent", ids)
	if err != nil {
		return nil, ErrResponse
	}
	return ids, nil
}

// Select the exact API from a positive application-version read. Never retry
// another mutation endpoint after an ambiguous response.
func (c *Client) StopForOrganization(ctx context.Context, hash string) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	raw, err := c.request(ctx, http.MethodGet, "app/version", nil)
	if err != nil {
		return err
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(raw)), "v")
	major, _, ok := strings.Cut(version, ".")
	n, err := strconv.Atoi(major)
	if !ok || err != nil || len(version) > 64 || n != 4 && n != 5 {
		return ErrResponse
	}
	endpoint := "torrents/pause"
	if n == 5 {
		endpoint = "torrents/stop"
	}
	raw, err = c.request(ctx, http.MethodPost, endpoint, url.Values{"hashes": {hash}})
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(string(raw)); s != "" && s != "Ok." {
		return ErrResponse
	}
	return nil
}
