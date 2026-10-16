package qbittorrent

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

// Uses the daemon's counter, never time.Now()-completion_on. Missing/sentinel
// counters remain unknown; a failed request is not permission to stop a task.
func (c *Client) SeedingEvidence(ctx context.Context, hash string) (seeding.Evidence, error) {
	e := seeding.Evidence{}
	if !ValidHash(hash) {
		return e, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return e, err
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/info?"+url.Values{"filter": {"all"}, "hashes": {hash}}.Encode(), nil)
	if err != nil {
		return e, err
	}
	var rows []struct {
		Hash     string   `json:"hash"`
		State    string   `json:"state"`
		Progress *float64 `json:"progress"`
		Left     *int64   `json:"amount_left"`
		Seconds  *int64   `json:"seeding_time"`
		Ratio    *float64 `json:"ratio"`
		AddedAt  *int64   `json:"added_on"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 || !strings.EqualFold(rows[0].Hash, hash) || rows[0].State == "" {
		return e, ErrResponse
	}
	r := rows[0]
	e.Hash = strings.ToLower(r.Hash)
	e.State = r.State
	if r.AddedAt != nil && *r.AddedAt > 0 {
		e.AddedAt = r.AddedAt
	}
	if r.Progress != nil && r.Left != nil {
		if math.IsNaN(*r.Progress) || math.IsInf(*r.Progress, 0) || *r.Progress < 0 || *r.Progress > 1 || *r.Left < 0 {
			return e, ErrResponse
		}
		known := true
		complete := false
		switch r.State {
		case "uploading", "stalledUP", "queuedUP", "forcedUP", "pausedUP", "stoppedUP":
			complete = *r.Progress == 1 && *r.Left == 0
		case "downloading", "stalledDL", "queuedDL", "forcedDL", "pausedDL", "stoppedDL", "metaDL", "forcedMetaDL", "checkingDL", "checkingUP", "checkingResumeData", "moving", "error", "missingFiles":
		default:
			known = false
		}
		if known {
			e.Complete = &complete
		}
	}
	if r.Seconds != nil && *r.Seconds >= 0 {
		e.Seconds = r.Seconds
	}
	if r.Ratio != nil && !math.IsNaN(*r.Ratio) && !math.IsInf(*r.Ratio, 0) && *r.Ratio >= 0 {
		e.Ratio = r.Ratio
	}
	return e, nil
}
