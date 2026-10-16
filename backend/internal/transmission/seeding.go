package transmission

import (
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

func (c *Client) SeedingEvidence(ctx context.Context, hash string) (seeding.Evidence, error) {
	e := seeding.Evidence{}
	if !ValidHash(hash) {
		return e, ErrConfiguration
	}
	oldFields := []string{"hashString", "status", "percentDone", "leftUntilDone", "metadataPercentComplete", "error", "secondsSeeding", "uploadRatio"}
	newFields := []string{"hash_string", "status", "percent_done", "left_until_done", "metadata_percent_complete", "error", "seconds_seeding", "upload_ratio"}
	old, _ := json.Marshal(map[string]any{"method": "torrent-get", "arguments": map[string]any{"ids": []string{hash}, "fields": oldFields}})
	modern, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "torrent_get", "params": map[string]any{"ids": []string{hash}, "fields": newFields}})
	raw, isModern, err := c.exchange(ctx, old, modern)
	if err != nil {
		return e, err
	}
	var envelope struct {
		JSONRPC   string          `json:"jsonrpc"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return e, ErrResponse
	}
	args := envelope.Arguments
	if isModern {
		if envelope.JSONRPC != "2.0" || hasJSONError(envelope.Error) {
			return e, ErrResponse
		}
		args = envelope.Result
	} else {
		var result string
		if json.Unmarshal(envelope.Result, &result) != nil || result != "success" {
			return e, ErrResponse
		}
	}
	var payload struct {
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if json.Unmarshal(args, &payload) != nil || len(payload.Torrents) != 1 {
		return e, ErrResponse
	}
	m := payload.Torrents[0]
	key := func(old, new string) string {
		if isModern {
			return new
		}
		return old
	}
	var found string
	var status *int
	if json.Unmarshal(m[key("hashString", "hash_string")], &found) != nil || !strings.EqualFold(found, hash) || json.Unmarshal(m["status"], &status) != nil || status == nil || *status < 0 || *status > 6 {
		return e, ErrResponse
	}
	e.Hash = strings.ToLower(found)
	e.State = map[int]string{0: "stopped", 1: "check-wait", 2: "checking", 3: "download-wait", 4: "downloading", 5: "seed-wait", 6: "seeding"}[*status]
	read := func(name string, target any) bool {
		raw, ok := m[name]
		return !ok || json.Unmarshal(raw, target) == nil
	}
	var progress, metadata *float64
	var left, seconds *int64
	var ratio *float64
	var taskError *int
	if !read(key("percentDone", "percent_done"), &progress) || !read(key("metadataPercentComplete", "metadata_percent_complete"), &metadata) || !read(key("leftUntilDone", "left_until_done"), &left) || !read("error", &taskError) || !read(key("secondsSeeding", "seconds_seeding"), &seconds) || !read(key("uploadRatio", "upload_ratio"), &ratio) {
		return e, ErrResponse
	}
	for _, n := range []*float64{progress, metadata} {
		if n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0) || *n < 0 || *n > 1) {
			return e, ErrResponse
		}
	}
	if left != nil && *left < 0 {
		return e, ErrResponse
	}
	if progress != nil && metadata != nil && left != nil && taskError != nil {
		complete := *progress == 1 && *metadata == 1 && *left == 0 && *taskError == 0 && (*status == 0 || *status == 5 || *status == 6)
		e.Complete = &complete
	}
	if seconds != nil && *seconds >= 0 {
		e.Seconds = seconds
	}
	if ratio != nil && !math.IsNaN(*ratio) && !math.IsInf(*ratio, 0) && *ratio >= 0 {
		e.Ratio = ratio
	}
	return e, nil
}
