package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

// Selection is independent of download completion. Missing indexes, metadata,
// labels or priorities cannot authorize a write or a success receipt.
func (c *Client) Selection(ctx context.Context, hash string) (torrentfiles.Snapshot, error) {
	s := torrentfiles.Snapshot{}
	if !ValidHash(hash) {
		return s, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return s, err
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/info?"+url.Values{"hashes": {hash}, "filter": {"all"}}.Encode(), nil)
	if err != nil {
		return s, err
	}
	var rows []struct {
		Hash      string  `json:"hash"`
		Directory *string `json:"save_path"`
		Tags      *string `json:"tags"`
		State     string  `json:"state"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || !strings.EqualFold(rows[0].Hash, hash) || rows[0].Directory == nil || *rows[0].Directory == "" || rows[0].Tags == nil {
		return s, ErrResponse
	}
	r := rows[0]
	s.Hash = strings.ToLower(hash)
	s.Directory = *r.Directory
	s.State = r.State
	s.Labels = []string{}
	for _, label := range strings.Split(*r.Tags, ",") {
		if label = strings.TrimSpace(label); label != "" {
			s.Labels = append(s.Labels, label)
		}
	}
	state := ProfileState{State: r.State}
	s.Paused = state.Paused()
	if !s.Paused && !state.Running() {
		return s, ErrResponse
	}
	raw, err = c.request(ctx, http.MethodGet, "torrents/files?"+url.Values{"hash": {hash}}.Encode(), nil)
	if err != nil {
		return s, err
	}
	var files []struct {
		Index    *int   `json:"index"`
		Name     string `json:"name"`
		Size     *int64 `json:"size"`
		Priority *int   `json:"priority"`
	}
	if json.Unmarshal(raw, &files) != nil {
		return s, ErrResponse
	}
	for _, f := range files {
		if f.Index == nil || f.Size == nil || f.Priority == nil || *f.Priority != 0 && *f.Priority != 1 && *f.Priority != 6 && *f.Priority != 7 {
			return s, ErrResponse
		}
		s.Files = append(s.Files, torrentfiles.File{Index: *f.Index, Path: f.Name, Size: *f.Size, Selected: *f.Priority != 0})
	}
	s.Files, err = torrentfiles.Normalize(s.Files)
	if err != nil {
		return s, ErrResponse
	}
	return s, nil
}
func (c *Client) SetFilePriority(ctx context.Context, hash string, ids []int, wanted bool) error {
	if !ValidHash(hash) || len(ids) == 0 || len(ids) > torrentfiles.MaxFiles {
		return ErrConfiguration
	}
	seen := map[int]bool{}
	values := []string{}
	for _, id := range ids {
		if id < 0 || id > 1000000 || seen[id] {
			return ErrConfiguration
		}
		seen[id] = true
		values = append(values, strconv.Itoa(id))
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	priority := "0"
	if wanted {
		priority = "1"
	}
	raw, err := c.request(ctx, http.MethodPost, "torrents/filePrio", url.Values{"hash": {hash}, "id": {strings.Join(values, "|")}, "priority": {priority}})
	if err == nil && strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "Ok." {
		return ErrResponse
	}
	return err
}

// Selected adds require known stopped semantics on both API generations.
func (c *Client) prepareSelectedForm(ctx context.Context, form url.Values) error {
	raw, err := c.request(ctx, http.MethodGet, "app/version", nil)
	if err != nil {
		return err
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(raw)), "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return ErrResponse
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major != 4 && major != 5 {
		return ErrResponse
	}
	for _, part := range parts[1:] {
		if _, err := strconv.Atoi(part); err != nil {
			return ErrResponse
		}
	}
	form.Set("autoTMM", "false")
	form.Set("contentLayout", "Original")
	form.Del("paused")
	form.Del("stopped")
	if major == 5 {
		form.Set("stopped", "true")
	} else {
		form.Set("paused", "true")
	}
	return nil
}
func (c *Client) StartSelected(ctx context.Context, hash string) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	raw, err := c.request(ctx, http.MethodGet, "app/version", nil)
	if err != nil {
		return err
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(raw)), "v")
	endpoint := "torrents/resume"
	if strings.HasPrefix(v, "5.") {
		endpoint = "torrents/start"
	} else if !strings.HasPrefix(v, "4.") {
		return ErrResponse
	}
	raw, err = c.request(ctx, http.MethodPost, endpoint, url.Values{"hashes": {hash}})
	if err == nil && strings.TrimSpace(string(raw)) != "" && strings.TrimSpace(string(raw)) != "Ok." {
		return ErrResponse
	}
	return err
}
