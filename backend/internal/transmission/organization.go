package transmission

import (
	"context"
	"encoding/json"
	"strings"
)

func (c *Client) organizationMutation(ctx context.Context, hash, oldMethod, newMethod string, legacyArgs, modernArgs map[string]any) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	legacyArgs["ids"] = []string{hash}
	modernArgs["ids"] = []string{hash}
	old, _ := json.Marshal(map[string]any{"method": oldMethod, "arguments": legacyArgs})
	new, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": newMethod, "params": modernArgs})
	raw, modern, err := c.exchange(ctx, old, new)
	if err != nil {
		return err
	}
	if modern {
		var reply struct {
			JSONRPC string          `json:"jsonrpc"`
			Result  map[string]any  `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &reply) != nil || reply.JSONRPC != "2.0" || reply.Result == nil || hasJSONError(reply.Error) {
			return ErrResponse
		}
	} else {
		var reply struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(raw, &reply) != nil || reply.Result != "success" {
			return ErrResponse
		}
	}
	return nil
}

// Transmission has only replace-labels, not atomic add-labels. Callers require
// a fresh label-list review and independent exclusive-label-writer consent.
func (c *Client) MarkOrganized(ctx context.Context, hash string, labels []string) error {
	if len(labels) == 0 || len(labels) > 65 {
		return ErrConfiguration
	}
	seen := map[string]bool{}
	for _, v := range labels {
		if v == "" || len(v) > 256 || strings.TrimSpace(v) != v || strings.ContainsAny(v, "\x00\r\n") || seen[v] {
			return ErrConfiguration
		}
		seen[v] = true
	}
	if !seen["已整理"] {
		return ErrConfiguration
	}
	return c.organizationMutation(ctx, hash, "torrent-set", "torrent_set", map[string]any{"labels": labels}, map[string]any{"labels": labels})
}
func (c *Client) RemoveOrganizationTask(ctx context.Context, hash string) error {
	return c.organizationMutation(ctx, hash, "torrent-remove", "torrent_remove", map[string]any{"delete-local-data": false}, map[string]any{"delete_local_data": false})
}
