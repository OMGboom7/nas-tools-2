package qbittorrent

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

func (c *Client) MarkOrganized(ctx context.Context, hash string, _ []string) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	body, err := c.request(ctx, http.MethodPost, "torrents/addTags", url.Values{"hashes": {hash}, "tags": {"已整理"}})
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(string(body)); s != "" && s != "Ok." {
		return ErrResponse
	}
	return nil
}

// RemoveOrganizationTask never deletes local data, including unselected files.
func (c *Client) RemoveOrganizationTask(ctx context.Context, hash string) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	body, err := c.request(ctx, http.MethodPost, "torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {"false"}})
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(string(body)); s != "" && s != "Ok." {
		return ErrResponse
	}
	return nil
}
