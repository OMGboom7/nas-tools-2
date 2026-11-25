package transmission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

// transferRPC preserves the original CSRF-only dialect negotiation. Only a
// definitive HTTP 409 may resend a mutation with the returned session token;
// connection errors, malformed replies and RPC errors never change dialect.
func (c *Client) transferRPC(ctx context.Context, oldMethod, newMethod string, oldArgs, newArgs map[string]any) (map[string]json.RawMessage, bool, error) {
	old, err := json.Marshal(map[string]any{"method": oldMethod, "arguments": oldArgs, "tag": 1})
	if err != nil {
		return nil, false, ErrConfiguration
	}
	new, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": newMethod, "params": newArgs})
	if err != nil {
		return nil, false, ErrConfiguration
	}
	raw, modern, err := c.exchange(ctx, old, new)
	if err != nil {
		return nil, modern, err
	}
	var env struct {
		JSONRPC   string          `json:"jsonrpc"`
		ID        *int            `json:"id"`
		Tag       *int            `json:"tag"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || hasJSONError(env.Error) {
		return nil, modern, ErrResponse
	}
	body := env.Arguments
	if modern {
		if env.JSONRPC != "2.0" || env.ID == nil || *env.ID != 1 {
			return nil, modern, ErrResponse
		}
		body = env.Result
	} else {
		var status string
		if json.Unmarshal(env.Result, &status) != nil || status != "success" || env.Tag == nil || *env.Tag != 1 {
			return nil, modern, ErrResponse
		}
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(body, &result) != nil || result == nil {
		return nil, modern, ErrResponse
	}
	return result, modern, nil
}

type TransferProtocol struct {
	Version string `json:"version"`
	RPC     int    `json:"rpc"`
	Modern  bool   `json:"modern"`
}

func (c *Client) TransferCapabilities(ctx context.Context) (TransferProtocol, error) {
	p := TransferProtocol{}
	row, modern, err := c.transferRPC(ctx, "session-get", "session_get", map[string]any{"fields": []string{"version", "rpc-version"}}, map[string]any{"fields": []string{"version", "rpc_version"}})
	if err != nil {
		return p, err
	}
	key := "rpc-version"
	if modern {
		key = "rpc_version"
	}
	if !transferField(row, "version", &p.Version) || len(p.Version) > 128 || p.Version == "" || !transferField(row, key, &p.RPC) || p.RPC < 16 || p.RPC > 100 {
		return p, torrenttransfer.ErrProtocol
	}
	p.Modern = modern
	return p, nil
}
func transferField(row map[string]json.RawMessage, key string, out any) bool {
	raw, ok := row[key]
	return ok && string(raw) != "null" && json.Unmarshal(raw, out) == nil
}

// Transmission's supported RPC identity is SHA1. Hybrid torrents retain that
// original v1 identity; pure v2 is not sent to a SHA1-only target.
func (c *Client) AddTransferPaused(ctx context.Context, content []byte, directory string) (torrenttransfer.AddReceipt, error) {
	return c.addTransferPaused(ctx, content, directory, nil)
}

func (c *Client) AddReviewedTransfer(ctx context.Context, content []byte, directory string, intent torrenttransfer.AddIntent) (torrenttransfer.AddReceipt, error) {
	if err := intent.Validate(); err != nil {
		return torrenttransfer.AddReceipt{}, err
	}
	return c.addTransferPaused(ctx, content, directory, &intent)
}

func (c *Client) addTransferPaused(ctx context.Context, content []byte, directory string, intent *torrenttransfer.AddIntent) (torrenttransfer.AddReceipt, error) {
	r := torrenttransfer.AddReceipt{}
	m, err := torrentmeta.PrepareTransfer(content, nil)
	if err != nil {
		return r, ErrConfiguration
	}
	if !torrenttransfer.Hex(m.V1Hash, 40) {
		return r, torrenttransfer.ErrProtocol
	}
	directory, err = torrenttransfer.NormalizeRemote(directory)
	if err != nil || strings.TrimSpace(directory) != directory {
		return r, ErrConfiguration
	}
	if _, err := c.TransferCapabilities(ctx); err != nil {
		return r, err
	}
	encoded := base64.StdEncoding.EncodeToString(m.Content)
	old := map[string]any{"metainfo": encoded, "download-dir": directory, "paused": true, "labels": []string{"已整理", "转移做种"}}
	new := map[string]any{"metainfo": encoded, "download_dir": directory, "paused": true, "labels": []string{"已整理", "转移做种"}}
	send := func() (torrenttransfer.AddReceipt, error) {
		return c.sendTransferAdd(ctx, m.V1Hash, old, new)
	}
	if intent != nil {
		return intent.Send(ctx, c.Address(), m.V1Hash, directory, m.Content, send)
	}
	return send()
}

func (c *Client) sendTransferAdd(ctx context.Context, expectedHash string, old, new map[string]any) (torrenttransfer.AddReceipt, error) {
	r := torrenttransfer.AddReceipt{}
	row, modern, err := c.transferRPC(ctx, "torrent-add", "torrent_add", old, new)
	if err != nil {
		return r, err
	}
	added, duplicate, hashKey := "torrent-added", "torrent-duplicate", "hashString"
	if modern {
		added, duplicate, hashKey = "torrent_added", "torrent_duplicate", "hash_string"
	}
	addRaw, hasAdded := row[added]
	dupRaw, hasDuplicate := row[duplicate]
	if hasAdded == hasDuplicate {
		return r, ErrResponse
	}
	raw := addRaw
	if hasDuplicate {
		raw = dupRaw
	}
	var task map[string]json.RawMessage
	if json.Unmarshal(raw, &task) != nil || !transferField(task, hashKey, &r.WireID) || !strings.EqualFold(r.WireID, expectedHash) {
		return torrenttransfer.AddReceipt{}, ErrResponse
	}
	r.WireID = strings.ToLower(r.WireID)
	r.Duplicate, r.CreatedKnown = hasDuplicate, true
	return r, nil
}

func (c *Client) TransferTask(ctx context.Context, id string) (torrenttransfer.Task, error) {
	s := torrenttransfer.Task{Kind: "transmission", Labels: []string{}, Done: []int{}}
	if !torrenttransfer.Hex(id, 40) {
		return s, ErrConfiguration
	}
	legacy := []string{"hashString", "name", "downloadDir", "addedDate", "status", "percentDone", "metadataPercentComplete", "leftUntilDone", "error", "labels", "files", "fileStats", "torrentFile", "trackers"}
	modernFields := []string{"hash_string", "name", "download_dir", "added_date", "status", "percent_done", "metadata_percent_complete", "left_until_done", "error", "labels", "files", "file_stats", "torrent_file", "trackers"}
	args, modern, err := c.transferRPC(ctx, "torrent-get", "torrent_get", map[string]any{"ids": []string{id}, "fields": legacy}, map[string]any{"ids": []string{id}, "fields": modernFields})
	if err != nil {
		return s, err
	}
	var rows []map[string]json.RawMessage
	if !transferField(args, "torrents", &rows) || rows == nil || len(rows) > 1 {
		return s, ErrResponse
	}
	if len(rows) == 0 {
		return s, torrenttransfer.ErrTaskAbsent
	}
	row := rows[0]
	fields := legacy
	if modern {
		fields = modernFields
	}
	var status, code int
	var progress, metadata float64
	var left int64
	var files, stats []map[string]json.RawMessage
	var trackers []struct {
		Announce string `json:"announce"`
	}
	var metainfoPath string
	outputs := []any{&s.WireID, &s.Name, &s.Directory, &s.AddedAt, &status, &progress, &metadata, &left, &code, &s.Labels, &files, &stats, &metainfoPath, &trackers}
	for i, key := range fields {
		if !transferField(row, key, outputs[i]) {
			return s, ErrResponse
		}
	}
	for _, tracker := range trackers {
		if tracker.Announce != "" {
			s.HasTracker = true
			break
		}
	}
	if metainfoPath != "" {
		s.MetainfoFile = path.Base(strings.ReplaceAll(metainfoPath, "\\", "/"))
	}
	if !strings.EqualFold(s.WireID, id) || status < 0 || status > 6 || code < 0 || left < 0 || !completionProgress(progress) || !completionProgress(metadata) || metadata != 1 || len(files) == 0 || len(files) > torrentfiles.MaxFiles || len(stats) != len(files) {
		return s, ErrResponse
	}
	s.WireID, s.V1Hash = id, id
	s.State = []string{"stopped", "check-wait", "checking", "download-wait", "downloading", "seed-wait", "seeding"}[status]
	s.Paused, s.Checking, s.Error, s.SourceSeeding = status == 0, status == 1 || status == 2, code != 0, status == 5 || status == 6
	s.Complete = progress == 1 && left == 0 && code == 0 && (status == 0 || s.SourceSeeding)
	s.Labels, err = torrenttransfer.Labels(s.Labels)
	if err != nil {
		return s, ErrResponse
	}
	doneKey := "bytesCompleted"
	if modern {
		doneKey = "bytes_completed"
	}
	for i, file := range files {
		var name string
		var size, done, statDone int64
		var wanted bool
		if !transferField(file, "name", &name) || !transferField(file, "length", &size) || !transferField(file, doneKey, &done) || !transferField(stats[i], doneKey, &statDone) || !transferField(stats[i], "wanted", &wanted) || size < 0 || done < 0 || done > size || done != statDone {
			return s, ErrResponse
		}
		s.Files = append(s.Files, torrentfiles.File{Index: i, Path: name, Size: size, Selected: wanted})
		if done == size {
			s.Done = append(s.Done, i)
		}
	}
	if !s.Valid() {
		return s, ErrResponse
	}
	return s, nil
}

func (c *Client) transferAction(ctx context.Context, expected torrenttransfer.Task, m torrentmeta.TransferMetadata, start bool) error {
	if expected.Kind != "transmission" || !expected.Matches(m) || !expected.Paused || expected.Checking || expected.Error || start && !expected.FullComplete(m) {
		return ErrConfiguration
	}
	current, err := c.TransferTask(ctx, expected.WireID)
	if err != nil {
		return err
	}
	if !expected.SameInstance(current) || !current.Paused || current.Checking || current.Error || start && (!current.HasTracker || !current.FullComplete(m)) {
		return ErrResponse
	}
	if start && m.Private {
		profile, err := c.Profile(ctx, expected.WireID)
		if err != nil {
			return err
		}
		dir, _ := torrenttransfer.NormalizeRemote(profile.Directory)
		want, _ := torrenttransfer.NormalizeRemote(current.Directory)
		if profile.Status != 0 || profile.RatioMode != 2 || profile.IdleMode != 2 || dir != want {
			return ErrResponse
		}
	}
	old, new := "torrent-verify", "torrent_verify"
	if start {
		old, new = "torrent-start", "torrent_start"
	}
	_, _, err = c.transferRPC(ctx, old, new, map[string]any{"ids": []string{expected.WireID}}, map[string]any{"ids": []string{expected.WireID}})
	return err
}
func (c *Client) RecheckTransfer(ctx context.Context, expected torrenttransfer.Task, m torrentmeta.TransferMetadata) error {
	return c.transferAction(ctx, expected, m, false)
}
func (c *Client) StartTransfer(ctx context.Context, expected torrenttransfer.Task, m torrentmeta.TransferMetadata) error {
	return c.transferAction(ctx, expected, m, true)
}

func (c *Client) TransferWireID(ctx context.Context, m torrentmeta.TransferMetadata) (string, error) {
	if !torrenttransfer.Hex(m.V1Hash, 40) {
		return "", torrenttransfer.ErrProtocol
	}
	if _, err := c.TransferCapabilities(ctx); err != nil {
		return "", err
	}
	return m.V1Hash, nil
}

var _ torrenttransfer.ReceiverClient = (*Client)(nil)
