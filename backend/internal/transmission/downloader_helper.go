package transmission

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloaderhelper"
	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
)

func (c *Client) HelperTasks(ctx context.Context, id string) ([]downloaderhelper.Task, error) {
	old := []string{"hashString", "name", "downloadDir", "addedDate", "status", "error", "errorString", "percentDone", "leftUntilDone", "metadataPercentComplete", "labels", "trackers"}
	mod := []string{"hash_string", "name", "download_dir", "added_date", "status", "error", "error_string", "percent_done", "left_until_done", "metadata_percent_complete", "labels", "trackers"}
	a, b := map[string]any{"fields": old}, map[string]any{"fields": mod}
	if id != "" {
		if !ValidHash(id) {
			return nil, ErrConfiguration
		}
		a["ids"], b["ids"] = []string{id}, []string{id}
	}
	res, modern, err := c.transferRPC(ctx, "torrent-get", "torrent_get", a, b)
	if err != nil {
		return nil, err
	}
	var rows []map[string]json.RawMessage
	if !transferField(res, "torrents", &rows) || rows == nil || id != "" && len(rows) > 1 {
		return nil, ErrResponse
	}
	keys := old
	if modern {
		keys = mod
	}
	out := make([]downloaderhelper.Task, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		var t downloaderhelper.Task
		var status, code int
		var msg string
		var progress, metadata float64
		var left int64
		var trackers []struct{ Announce string }
		for i, dst := range []any{&t.Hash, &t.Name, &t.Directory, &t.Added, &status, &code, &msg, &progress, &left, &metadata, &t.Labels, &trackers} {
			if !transferField(row, keys[i], dst) {
				return nil, ErrResponse
			}
		}
		t.Hash = strings.ToLower(t.Hash)
		if !ValidHash(t.Hash) || seen[t.Hash] || id != "" && id != t.Hash || t.Name == "" || t.Directory == "" || t.Added <= 0 || status < 0 || status > 6 || code < 0 || !completionProgress(progress) || !completionProgress(metadata) {
			return nil, ErrResponse
		}
		seen[t.Hash] = true
		t.Labels, err = torrentmark.NormalizeLabels(t.Labels)
		if err != nil {
			return nil, ErrResponse
		}
		if len(trackers) > 0 {
			t.Site = downloaderhelper.Keyword(trackers[0].Announce)
		}
		full := metadata == 1 && progress == 1 && left == 0 && code == 0
		t.CanSeed = full && status == 0
		t.Seeding = full && (status == 5 || status == 6)
		t.Missing = code == 3 && strings.Contains(msg, "No data found")
		out = append(out, t)
	}
	return out, nil
}
func (c *Client) SetHelperLabels(ctx context.Context, t downloaderhelper.Task, want, drop string) error {
	labels, err := torrentmark.NormalizeLabels(downloaderhelper.LabelsAfter(t.Labels, want, drop))
	if err != nil {
		return err
	}
	return c.organizationMutation(ctx, t.Hash, "torrent-set", "torrent_set", map[string]any{"labels": labels}, map[string]any{"labels": labels})
}
