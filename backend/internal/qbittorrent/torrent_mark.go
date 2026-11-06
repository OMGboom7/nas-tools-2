package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
)

func (c *Client) MarkEvidence(ctx context.Context, hash string) (torrentmark.Evidence, error) {
	var e torrentmark.Evidence
	if !ValidHash(hash) {
		return e, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return e, err
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/info?"+url.Values{"hashes": {hash}, "filter": {"completed"}}.Encode(), nil)
	if err != nil {
		return e, err
	}
	var rows []struct {
		Hash     string   `json:"hash"`
		Added    *int64   `json:"added_on"`
		State    string   `json:"state"`
		Tags     *string  `json:"tags"`
		Progress *float64 `json:"progress"`
		Left     *int64   `json:"amount_left"`
		Private  *bool    `json:"isPrivate"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || !strings.EqualFold(rows[0].Hash, hash) {
		return e, ErrResponse
	}
	row := rows[0]
	if row.Added == nil || *row.Added <= 0 || row.Tags == nil || row.Progress == nil || *row.Progress != 1 || row.Left == nil || *row.Left != 0 {
		return e, ErrResponse
	}
	switch row.State {
	case "uploading", "stalledUP", "queuedUP", "forcedUP", "pausedUP", "stoppedUP":
	default:
		return e, ErrResponse
	}
	labels := []string{}
	if *row.Tags != "" {
		for _, tag := range strings.Split(*row.Tags, ",") {
			labels = append(labels, strings.TrimSpace(tag))
		}
	}
	labels, err = torrentmark.NormalizeLabels(labels)
	if err != nil {
		return e, ErrResponse
	}
	body, err := c.request(ctx, http.MethodGet, "torrents/trackers?"+url.Values{"hash": {hash}}.Encode(), nil)
	if err != nil {
		return e, err
	}
	var trackers []struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(body, &trackers) != nil || trackers == nil || len(trackers) > 131 {
		return e, ErrResponse
	}
	e = torrentmark.Evidence{Hash: strings.ToLower(row.Hash), Added: *row.Added, Labels: labels, Complete: true, Private: row.Private}
	for _, tracker := range trackers {
		switch tracker.URL {
		case "** [DHT] **", "** [PeX] **", "** [LSD] **":
		default:
			e.Trackers = append(e.Trackers, tracker.URL)
		}
	}
	return e, nil
}

func (c *Client) SetMark(ctx context.Context, before torrentmark.Evidence, want string) error {
	if !ValidHash(before.Hash) || want != torrentmark.PT && want != torrentmark.BT || torrentmark.Classify(before) != want {
		return ErrConfiguration
	}
	post := func(path, label string) error {
		raw, err := c.request(ctx, http.MethodPost, path, url.Values{"hashes": {before.Hash}, "tags": {label}})
		if err != nil {
			return err
		}
		if s := strings.TrimSpace(string(raw)); s != "" && s != "Ok." {
			return ErrResponse
		}
		return nil
	}
	if !slices.Contains(before.Labels, want) {
		if err := post("torrents/addTags", want); err != nil {
			return err
		}
	}
	if !slices.Contains(before.Labels, torrentmark.Other(want)) {
		return nil
	}
	fresh, err := c.MarkEvidence(ctx, before.Hash)
	if err != nil || fresh.Added != before.Added || torrentmark.Classify(fresh) != want || !slices.Contains(fresh.Labels, want) {
		return ErrResponse
	}
	if !slices.Contains(fresh.Labels, torrentmark.Other(want)) {
		return nil
	}
	return post("torrents/removeTags", torrentmark.Other(want))
}
