package transmission

import (
	"context"
	"encoding/json"
	"strings"
)

func (c *Client) TaskAddedAt(ctx context.Context, hash string) (int64, error) {
	if !ValidHash(hash) {
		return 0, ErrConfiguration
	}
	old, _ := json.Marshal(map[string]any{"method": "torrent-get", "arguments": map[string]any{"ids": []string{hash}, "fields": []string{"hashString", "addedDate"}}})
	new, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "torrent_get", "params": map[string]any{"ids": []string{hash}, "fields": []string{"hash_string", "added_date"}}})
	raw, modern, err := c.exchange(ctx, old, new)
	if err != nil {
		return 0, err
	}
	var env struct {
		JSONRPC   string          `json:"jsonrpc"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return 0, ErrResponse
	}
	body := env.Arguments
	if modern {
		if env.JSONRPC != "2.0" || hasJSONError(env.Error) {
			return 0, ErrResponse
		}
		body = env.Result
	} else {
		var result string
		if json.Unmarshal(env.Result, &result) != nil || result != "success" {
			return 0, ErrResponse
		}
	}
	var payload struct {
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Torrents) != 1 {
		return 0, ErrResponse
	}
	hashKey, dateKey := "hashString", "addedDate"
	if modern {
		hashKey, dateKey = "hash_string", "added_date"
	}
	var actual string
	var added int64
	if json.Unmarshal(payload.Torrents[0][hashKey], &actual) != nil || !strings.EqualFold(actual, hash) || json.Unmarshal(payload.Torrents[0][dateKey], &added) != nil || added <= 0 {
		return 0, ErrResponse
	}
	return added, nil
}
