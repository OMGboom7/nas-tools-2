package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloaderhelper"
	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
)

// HelperTasks includes stopped, incomplete and missing tasks. The ordinary
// downloads page filters its list to downloads still in progress.
func (c *Client) HelperTasks(ctx context.Context, id string) ([]downloaderhelper.Task, error) {
	if id != "" && !ValidHash(id) {
		return nil, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	query := url.Values{}
	if id != "" {
		query.Set("hashes", id)
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/info?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Hash, Name, State, Tracker string
		Magnet                     string `json:"magnet_uri"`
		Directory                  string `json:"save_path"`
		Added                      int64  `json:"added_on"`
		Tags                       *string
		Progress                   *float64
		Left                       *int64 `json:"amount_left"`
	}
	if json.Unmarshal(raw, &rows) != nil || rows == nil || id != "" && len(rows) > 1 {
		return nil, ErrResponse
	}
	out := make([]downloaderhelper.Task, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		h := strings.ToLower(r.Hash)
		if !ValidHash(h) || seen[h] || id != "" && id != h || r.Name == "" || r.State == "" || r.Directory == "" || r.Added <= 0 || r.Tags == nil || r.Progress == nil || *r.Progress < 0 || *r.Progress > 1 || r.Left == nil {
			return nil, ErrResponse
		}
		seen[h] = true
		labels := []string{}
		if *r.Tags != "" {
			for _, v := range strings.Split(*r.Tags, ",") {
				labels = append(labels, strings.TrimSpace(v))
			}
		}
		labels, err = torrentmark.NormalizeLabels(labels)
		if err != nil {
			return nil, ErrResponse
		}
		tracker := r.Tracker
		if tracker == "" {
			if u, e := url.Parse(r.Magnet); e == nil && u.Scheme == "magnet" {
				tracker = u.Query().Get("tr")
			}
		}
		full := *r.Progress == 1 && *r.Left == 0
		out = append(out, downloaderhelper.Task{Hash: h, Name: r.Name, Directory: r.Directory, Added: r.Added, Site: downloaderhelper.Keyword(tracker), Labels: labels, CanSeed: full && (r.State == "pausedUP" || r.State == "stoppedUP"), Seeding: full && slices.Contains([]string{"uploading", "queuedUP", "forcedUP", "stalledUP"}, r.State), Missing: r.State == "missingFiles"})
	}
	return out, nil
}

func (c *Client) SetHelperLabels(ctx context.Context, t downloaderhelper.Task, want, drop string) error {
	if !ValidHash(t.Hash) || want == "" {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	post := func(method, label string) error {
		reply, err := c.request(ctx, http.MethodPost, method, url.Values{"hashes": {t.Hash}, "tags": {label}})
		if err == nil && strings.TrimSpace(string(reply)) != "" && strings.TrimSpace(string(reply)) != "Ok." {
			return ErrResponse
		}
		return err
	}
	if !slices.Contains(t.Labels, want) {
		if err := post("torrents/addTags", want); err != nil {
			return err
		}
	}
	if drop != "" && slices.Contains(t.Labels, drop) {
		rows, err := c.HelperTasks(ctx, t.Hash)
		if err != nil || len(rows) != 1 {
			return ErrResponse
		}
		fresh := rows[0]
		expect := downloaderhelper.LabelsAfter(t.Labels, want, "")
		if fresh.Added != t.Added || fresh.Site != t.Site || fresh.Directory != t.Directory || !slices.Equal(fresh.Labels, expect) {
			return ErrResponse
		}
		return post("torrents/removeTags", drop)
	}
	return nil
}
