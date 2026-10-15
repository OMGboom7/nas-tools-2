package transmission

import (
	"context"
	"encoding/json"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
)

func (c *Client) CompletedIDs(ctx context.Context) ([]string, error) {
	raw, modern, err := c.exchange(ctx, []byte(`{"method":"torrent-get","arguments":{"fields":["hashString","percentDone","leftUntilDone","metadataPercentComplete","status","error"]}}`), []byte(`{"jsonrpc":"2.0","id":1,"method":"torrent_get","params":{"fields":["hash_string","percent_done","left_until_done","metadata_percent_complete","status","error"]}}`))
	if err != nil {
		return nil, err
	}
	var env struct {
		JSONRPC   string          `json:"jsonrpc"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil, ErrResponse
	}
	args := env.Arguments
	if modern {
		if env.JSONRPC != "2.0" || hasJSONError(env.Error) {
			return nil, ErrResponse
		}
		args = env.Result
	} else {
		var result string
		if json.Unmarshal(env.Result, &result) != nil || result != "success" {
			return nil, ErrResponse
		}
	}
	var payload struct {
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if json.Unmarshal(args, &payload) != nil || payload.Torrents == nil || len(payload.Torrents) > 1000 {
		return nil, ErrResponse
	}
	key := func(a, b string) string {
		if modern {
			return b
		}
		return a
	}
	ids := []string{}
	for _, row := range payload.Torrents {
		get := func(k string, v any) bool {
			raw, ok := row[k]
			return ok && string(raw) != "null" && json.Unmarshal(raw, v) == nil
		}
		var hash string
		var progress, meta float64
		var left int64
		var status, code int
		if !get(key("hashString", "hash_string"), &hash) || !ValidHash(hash) || !get(key("percentDone", "percent_done"), &progress) || !get(key("metadataPercentComplete", "metadata_percent_complete"), &meta) || !get(key("leftUntilDone", "left_until_done"), &left) || !get("status", &status) || !get("error", &code) || !completionProgress(progress) || !completionProgress(meta) || left < 0 || status < 0 || status > 6 || code < 0 {
			return nil, ErrResponse
		}
		if progress == 1 && meta == 1 && left == 0 && code == 0 && (status == 0 || status == 5 || status == 6) {
			ids = append(ids, hash)
		}
	}
	ids, err = downloadcompletion.NormalizeIDs("transmission", ids)
	if err != nil {
		return nil, ErrResponse
	}
	return ids, nil
}
