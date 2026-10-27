package transmission

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

func (c *Client) Selection(ctx context.Context, hash string) (torrentfiles.Snapshot, error) {
	s := torrentfiles.Snapshot{}
	if !ValidHash(hash) {
		return s, ErrConfiguration
	}
	legacy := []string{"hashString", "downloadDir", "labels", "status", "metadataPercentComplete", "files", "fileStats"}
	modern := []string{"hash_string", "download_dir", "labels", "status", "metadata_percent_complete", "files", "file_stats"}
	lb, _ := json.Marshal(map[string]any{"method": "torrent-get", "arguments": map[string]any{"ids": []string{hash}, "fields": legacy}})
	mb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "torrent_get", "params": map[string]any{"ids": []string{hash}, "fields": modern}, "id": 1})
	raw, isModern, err := c.exchange(ctx, lb, mb)
	if err != nil {
		return s, err
	}
	var env struct {
		JSONRPC   string          `json:"jsonrpc"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return s, ErrResponse
	}
	args := env.Arguments
	fields := legacy
	if isModern {
		if env.JSONRPC != "2.0" || hasJSONError(env.Error) {
			return s, ErrResponse
		}
		args = env.Result
		fields = modern
	} else if string(env.Result) != `"success"` {
		return s, ErrResponse
	}
	var rows struct {
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if json.Unmarshal(args, &rows) != nil || len(rows.Torrents) != 1 {
		return s, ErrResponse
	}
	m := rows.Torrents[0]
	decode := func(key string, out any) bool {
		v, ok := m[key]
		return ok && string(v) != "null" && json.Unmarshal(v, out) == nil
	}
	var status int
	var metadata float64
	var files []map[string]json.RawMessage
	var stats []map[string]json.RawMessage
	outputs := []any{&s.Hash, &s.Directory, &s.Labels, &status, &metadata, &files, &stats}
	for i, key := range fields {
		if !decode(key, outputs[i]) {
			return s, ErrResponse
		}
	}
	if !strings.EqualFold(s.Hash, hash) || s.Directory == "" || s.Labels == nil || status < 0 || status > 6 || metadata != 1 || len(files) != len(stats) {
		return s, ErrResponse
	}
	s.Hash = strings.ToLower(hash)
	s.State = []string{"stopped", "check-wait", "checking", "download-wait", "downloading", "seed-wait", "seeding"}[status]
	s.Paused = status == 0
	for i, f := range files {
		var name string
		var size int64
		var wanted bool
		read := func(m map[string]json.RawMessage, k string, v any) bool {
			raw, ok := m[k]
			return ok && string(raw) != "null" && json.Unmarshal(raw, v) == nil
		}
		if !read(f, "name", &name) || !read(f, "length", &size) || !read(stats[i], "wanted", &wanted) {
			return s, ErrResponse
		}
		s.Files = append(s.Files, torrentfiles.File{Index: i, Path: name, Size: size, Selected: wanted})
	}
	s.Files, err = torrentfiles.Normalize(s.Files)
	if err != nil {
		return s, ErrResponse
	}
	return s, nil
}
func (c *Client) SelectFiles(ctx context.Context, hash string, wanted, unwanted []int) error {
	if !ValidHash(hash) || len(wanted) == 0 || len(wanted)+len(unwanted) > torrentfiles.MaxFiles {
		return ErrConfiguration
	}
	seen := map[int]bool{}
	for _, list := range [][]int{wanted, unwanted} {
		for _, id := range list {
			if id < 0 || id > 1000000 || seen[id] {
				return ErrConfiguration
			}
			seen[id] = true
		}
	}
	old := map[string]any{"ids": []string{hash}, "files-wanted": wanted}
	new := map[string]any{"ids": []string{hash}, "files_wanted": wanted}
	// An empty array means ALL files in Transmission; omit empty unwanted.
	if len(unwanted) > 0 {
		old["files-unwanted"] = unwanted
		new["files_unwanted"] = unwanted
	}
	lb, _ := json.Marshal(map[string]any{"method": "torrent-set", "arguments": old})
	mb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "torrent_set", "params": new, "id": 1})
	raw, modern, err := c.exchange(ctx, lb, mb)
	if err != nil {
		return err
	}
	if modern {
		var r struct {
			JSONRPC string          `json:"jsonrpc"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(raw, &r) != nil || r.JSONRPC != "2.0" || hasJSONError(r.Error) || len(r.Result) == 0 || string(r.Result) == "null" {
			return ErrResponse
		}
	} else {
		var r struct {
			Result string `json:"result"`
		}
		if json.Unmarshal(raw, &r) != nil || r.Result != "success" {
			return ErrResponse
		}
	}
	return nil
}
