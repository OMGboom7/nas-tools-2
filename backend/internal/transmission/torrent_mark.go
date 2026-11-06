package transmission

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
)

func (c *Client) MarkEvidence(ctx context.Context, hash string) (torrentmark.Evidence, error) {
	var e torrentmark.Evidence
	if !ValidHash(hash) {
		return e, ErrConfiguration
	}
	oldFields := []string{"hashString", "status", "percentDone", "leftUntilDone", "metadataPercentComplete", "error", "addedDate", "labels", "trackers", "isPrivate"}
	newFields := []string{"hash_string", "status", "percent_done", "left_until_done", "metadata_percent_complete", "error", "added_date", "labels", "trackers", "is_private"}
	old, _ := json.Marshal(map[string]any{"method": "torrent-get", "arguments": map[string]any{"ids": []string{hash}, "fields": oldFields}})
	new, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "torrent_get", "params": map[string]any{"ids": []string{hash}, "fields": newFields}})
	raw, modern, err := c.exchange(ctx, old, new)
	if err != nil {
		return e, err
	}
	var env struct {
		JSONRPC   string          `json:"jsonrpc"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return e, ErrResponse
	}
	body := env.Arguments
	if modern {
		if env.JSONRPC != "2.0" || hasJSONError(env.Error) {
			return e, ErrResponse
		}
		body = env.Result
	} else {
		var s string
		if json.Unmarshal(env.Result, &s) != nil || s != "success" {
			return e, ErrResponse
		}
	}
	var reply struct {
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if json.Unmarshal(body, &reply) != nil || len(reply.Torrents) != 1 {
		return e, ErrResponse
	}
	row := reply.Torrents[0]
	key := func(old, new string) string {
		if modern {
			return new
		}
		return old
	}
	get := func(name string, target any) bool {
		b, ok := row[name]
		return ok && string(b) != "null" && json.Unmarshal(b, target) == nil
	}
	var status, code int
	var progress, meta float64
	var left int64
	var labels []string
	var trackers []struct {
		Announce string `json:"announce"`
	}
	if !get(key("hashString", "hash_string"), &e.Hash) || !strings.EqualFold(e.Hash, hash) ||
		!get(key("addedDate", "added_date"), &e.Added) || e.Added <= 0 ||
		!get(key("percentDone", "percent_done"), &progress) || progress != 1 ||
		!get(key("leftUntilDone", "left_until_done"), &left) || left != 0 ||
		!get(key("metadataPercentComplete", "metadata_percent_complete"), &meta) || meta != 1 ||
		!get("error", &code) || code != 0 || !get("status", &status) || (status != 5 && status != 6) ||
		!get("labels", &labels) || !get("trackers", &trackers) || trackers == nil || len(trackers) > 128 {
		return e, ErrResponse
	}
	if field, ok := row[key("isPrivate", "is_private")]; ok && json.Unmarshal(field, &e.Private) != nil {
		return e, ErrResponse
	}
	e.Labels, err = torrentmark.NormalizeLabels(labels)
	if err != nil {
		return e, ErrResponse
	}
	for _, tracker := range trackers {
		e.Trackers = append(e.Trackers, tracker.Announce)
	}
	e.Hash, e.Complete = strings.ToLower(e.Hash), true
	return e, nil
}

func (c *Client) SetMark(ctx context.Context, before torrentmark.Evidence, want string) error {
	if want != torrentmark.PT && want != torrentmark.BT || torrentmark.Classify(before) != want {
		return ErrConfiguration
	}
	labels := torrentmark.LabelsAfter(before.Labels, want)
	if _, err := torrentmark.NormalizeLabels(labels); err != nil {
		return ErrConfiguration
	}
	return c.organizationMutation(ctx, before.Hash, "torrent-set", "torrent_set", map[string]any{"labels": labels}, map[string]any{"labels": labels})
}
