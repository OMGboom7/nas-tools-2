package transmission

import (
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
)

// Completion reads one exact torrent including wanted files and completion
// counters. Both Transmission RPC dialects use the same normalized evidence.
// Protocol: https://github.com/transmission/transmission/blob/main/docs/rpc-spec.md
func (client *Client) Completion(ctx context.Context, hash string) (downloadcompletion.Task, error) {
	if !ValidHash(hash) {
		return downloadcompletion.Task{}, ErrConfiguration
	}
	legacyFields := []string{"hashString", "name", "downloadDir", "percentDone", "leftUntilDone", "status", "error", "metadataPercentComplete", "labels", "files", "fileStats"}
	modernFields := []string{"hash_string", "name", "download_dir", "percent_done", "left_until_done", "status", "error", "metadata_percent_complete", "labels", "files", "file_stats"}
	legacy, _ := json.Marshal(map[string]any{"method": "torrent-get", "arguments": map[string]any{"ids": []string{hash}, "fields": legacyFields}})
	modern, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "torrent_get", "params": map[string]any{"ids": []string{hash}, "fields": modernFields}, "id": 1})
	raw, isModern, err := client.exchange(ctx, legacy, modern)
	if err != nil {
		return downloadcompletion.Task{}, err
	}
	var envelope struct {
		JSONRPC   string          `json:"jsonrpc"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return downloadcompletion.Task{}, ErrResponse
	}
	args := envelope.Arguments
	if isModern {
		if envelope.JSONRPC != "2.0" || hasJSONError(envelope.Error) {
			return downloadcompletion.Task{}, ErrResponse
		}
		args = envelope.Result
	} else {
		var result string
		if json.Unmarshal(envelope.Result, &result) != nil || result != "success" {
			return downloadcompletion.Task{}, ErrResponse
		}
	}
	var payload struct {
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if json.Unmarshal(args, &payload) != nil || len(payload.Torrents) != 1 {
		return downloadcompletion.Task{}, ErrResponse
	}
	m := payload.Torrents[0]
	key := func(old, new string) string {
		if isModern {
			return new
		}
		return old
	}
	decode := func(field string, value any) bool {
		raw, ok := m[field]
		return ok && string(raw) != "null" && json.Unmarshal(raw, value) == nil
	}
	var id, name, dir string
	var progress, metadata float64
	var left int64
	var status, code int
	if !decode(key("hashString", "hash_string"), &id) || !strings.EqualFold(id, hash) || !decode("name", &name) || !decode(key("downloadDir", "download_dir"), &dir) || !decode(key("percentDone", "percent_done"), &progress) || !decode(key("metadataPercentComplete", "metadata_percent_complete"), &metadata) || !decode(key("leftUntilDone", "left_until_done"), &left) || !decode("status", &status) || !decode("error", &code) || left < 0 || status < 0 || status > 6 || code < 0 || !completionProgress(progress) || !completionProgress(metadata) {
		return downloadcompletion.Task{}, ErrResponse
	}
	task := downloadcompletion.Task{ID: id, Name: name, Directory: dir, State: []string{"stopped", "check-wait", "checking", "download-wait", "downloading", "seed-wait", "seeding"}[status], Complete: progress == 1 && metadata == 1 && left == 0 && code == 0 && (status == 0 || status == 5 || status == 6)}
	if _, exists := m["labels"]; exists {
		if !decode("labels", &task.Labels) || task.Labels == nil {
			return downloadcompletion.Task{}, ErrResponse
		}
		task.LabelsKnown = true
	}
	var files []map[string]json.RawMessage
	var stats []map[string]json.RawMessage
	if !decode("files", &files) || !decode(key("fileStats", "file_stats"), &stats) || len(files) == 0 || len(files) > 1000 || len(stats) != len(files) {
		return downloadcompletion.Task{}, ErrResponse
	}
	read := func(m map[string]json.RawMessage, k string, value any) bool {
		raw, ok := m[k]
		return ok && string(raw) != "null" && json.Unmarshal(raw, value) == nil
	}
	for i, f := range files {
		var name string
		var size, done, statDone int64
		var wanted bool
		if !read(f, "name", &name) || !read(f, "length", &size) || !read(f, key("bytesCompleted", "bytes_completed"), &done) || !read(stats[i], key("bytesCompleted", "bytes_completed"), &statDone) || !read(stats[i], "wanted", &wanted) || done < 0 || done > size || statDone != done {
			return downloadcompletion.Task{}, ErrResponse
		}
		task.Files = append(task.Files, downloadcompletion.File{Path: name, Size: size, Selected: wanted, Complete: done == size})
	}
	result, err := downloadcompletion.Normalize(task)
	if err != nil {
		return downloadcompletion.Task{}, ErrResponse
	}
	return result, nil
}
func completionProgress(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}
