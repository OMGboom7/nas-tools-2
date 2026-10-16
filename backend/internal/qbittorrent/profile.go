package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// PT requires all three per-task and effective limits, including the inactive
// limit introduced in 4.6. Unknown protocols do not authorize a mutation.
func (c *Client) profileVersion(ctx context.Context) (string, int, error) {
	raw, err := c.request(ctx, http.MethodGet, "app/version", nil)
	if err != nil {
		return "", 0, err
	}
	v := strings.TrimPrefix(strings.TrimSpace(string(raw)), "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 || len(v) > 32 {
		return "", 0, ErrResponse
	}
	n := []int{}
	for _, part := range parts {
		if part == "" {
			return "", 0, ErrResponse
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return "", 0, ErrResponse
			}
		}
		value, e := strconv.Atoi(part)
		if e != nil {
			return "", 0, ErrResponse
		}
		n = append(n, value)
	}
	if n[0] != 5 && (n[0] != 4 || n[1] < 6) {
		return "", 0, ErrResponse
	}
	return v, n[0], nil
}

func (c *Client) preparePTForm(ctx context.Context, form url.Values) error {
	_, major, err := c.profileVersion(ctx)
	if err != nil {
		return err
	}
	form.Set("autoTMM", "false")
	form.Set("stopCondition", "None")
	form.Set("ratioLimit", "-1")
	form.Set("seedingTimeLimit", "-1")
	form.Set("inactiveSeedingTimeLimit", "-1")
	form.Del("paused")
	if major == 4 {
		form.Set("paused", "true")
	} else {
		form.Set("stopped", "true")
	}
	return nil
}

func (c *Client) CheckPTProtocol(ctx context.Context) error {
	if err := c.login(ctx); err != nil {
		return err
	}
	_, _, err := c.profileVersion(ctx)
	return err
}

// 5.0/5.1 neither return nor require share_limit_action. 5.2 adds it.
func needsShareAction(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 || parts[0] != "5" {
		return false
	}
	minor, _ := strconv.Atoi(parts[1])
	return minor >= 2
}

type ProfileState struct {
	Hash           string   `json:"hash"`
	Version        string   `json:"version"`
	State          string   `json:"state"`
	Directory      string   `json:"directory"`
	Category       string   `json:"category"`
	Tags           []string `json:"tags"`
	AutoTMM        bool     `json:"autoTMM"`
	Forced         bool     `json:"forced"`
	UploadLimit    int64    `json:"uploadLimit"`
	DownloadLimit  int64    `json:"downloadLimit"`
	RatioLimit     float64  `json:"ratioLimit"`
	SeedLimit      int64    `json:"seedLimit"`
	IdleLimit      int64    `json:"idleLimit"`
	EffectiveRatio float64  `json:"effectiveRatio"`
	EffectiveSeed  int64    `json:"effectiveSeed"`
	EffectiveIdle  int64    `json:"effectiveIdle"`
	ShareAction    string   `json:"shareAction"`
}

func (s ProfileState) Paused() bool {
	return s.State == "pausedDL" || s.State == "pausedUP" || s.State == "stoppedDL" || s.State == "stoppedUP"
}
func (s ProfileState) Running() bool {
	switch s.State {
	case "downloading", "metaDL", "stalledDL", "queuedDL", "uploading", "stalledUP", "queuedUP":
		return true
	}
	return false
}
func (s ProfileState) SpeedsMatch(o AddOptions) bool {
	match := func(reported int64, kb int) bool {
		if kb == 0 {
			return reported == 0 || reported == -1
		}
		return reported == int64(kb)*1024
	}
	return !s.AutoTMM && !s.Forced && match(s.UploadLimit, o.UploadLimitKB) && match(s.DownloadLimit, o.DownloadLimitKB)
}
func (s ProfileState) Unlimited() bool {
	return s.RatioLimit == -1 && s.SeedLimit == -1 && s.IdleLimit == -1 && s.EffectiveRatio == -1 && s.EffectiveSeed == -1 && s.EffectiveIdle == -1
}
func validShareAction(s string) bool {
	switch s {
	case "Default", "Stop", "Remove", "RemoveWithContent", "EnableSuperSeeding":
		return true
	}
	return false
}

func (c *Client) Profile(ctx context.Context, hash string) (ProfileState, error) {
	s := ProfileState{}
	if !ValidHash(hash) {
		return s, ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return s, err
	}
	version, _, err := c.profileVersion(ctx)
	if err != nil {
		return s, err
	}
	raw, err := c.request(ctx, http.MethodGet, "torrents/info?"+url.Values{"filter": {"all"}, "hashes": {hash}}.Encode(), nil)
	if err != nil {
		return s, err
	}
	var rows []struct {
		Hash      string   `json:"hash"`
		State     string   `json:"state"`
		Directory *string  `json:"save_path"`
		Category  *string  `json:"category"`
		Tags      *string  `json:"tags"`
		AutoTMM   *bool    `json:"auto_tmm"`
		Forced    *bool    `json:"force_start"`
		Upload    *int64   `json:"up_limit"`
		Download  *int64   `json:"dl_limit"`
		Ratio     *float64 `json:"ratio_limit"`
		Seed      *int64   `json:"seeding_time_limit"`
		Idle      *int64   `json:"inactive_seeding_time_limit"`
		MaxRatio  *float64 `json:"max_ratio"`
		MaxSeed   *int64   `json:"max_seeding_time"`
		MaxIdle   *int64   `json:"max_inactive_seeding_time"`
		Action    *string  `json:"share_limit_action"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) != 1 {
		return s, ErrResponse
	}
	r := rows[0]
	if !strings.EqualFold(r.Hash, hash) || r.Directory == nil || *r.Directory == "" || r.Category == nil || r.Tags == nil || r.AutoTMM == nil || r.Forced == nil || r.Upload == nil || r.Download == nil || r.Ratio == nil || r.Seed == nil || r.Idle == nil || r.MaxRatio == nil || r.MaxSeed == nil || r.MaxIdle == nil || needsShareAction(version) && r.Action == nil || r.Action != nil && !validShareAction(*r.Action) {
		return s, ErrResponse
	}
	s = ProfileState{Hash: strings.ToLower(r.Hash), Version: version, State: r.State, Directory: *r.Directory, Category: *r.Category, Tags: []string{}, AutoTMM: *r.AutoTMM, Forced: *r.Forced, UploadLimit: *r.Upload, DownloadLimit: *r.Download, RatioLimit: *r.Ratio, SeedLimit: *r.Seed, IdleLimit: *r.Idle, EffectiveRatio: *r.MaxRatio, EffectiveSeed: *r.MaxSeed, EffectiveIdle: *r.MaxIdle}
	if r.Action != nil {
		s.ShareAction = *r.Action
	}
	if !s.Paused() && !s.Running() {
		return s, ErrResponse
	}
	for _, tag := range strings.Split(*r.Tags, ",") {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			s.Tags = append(s.Tags, tag)
		}
	}
	return s, nil
}

func (c *Client) SetUnlimited(ctx context.Context, hash, action string) error {
	if !ValidHash(hash) || action != "" && !validShareAction(action) {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	version, _, err := c.profileVersion(ctx)
	if err != nil {
		return err
	}
	if needsShareAction(version) && action == "" {
		return ErrResponse
	}
	form := url.Values{"hashes": {hash}, "ratioLimit": {"-1"}, "seedingTimeLimit": {"-1"}, "inactiveSeedingTimeLimit": {"-1"}}
	if action != "" {
		form.Set("shareLimitAction", action)
	}
	raw, err := c.request(ctx, http.MethodPost, "torrents/setShareLimits", form)
	if err != nil {
		return err
	}
	if body := strings.TrimSpace(string(raw)); body != "" && body != "Ok." {
		return ErrResponse
	}
	return nil
}

func (c *Client) StartProfile(ctx context.Context, hash string) error {
	if !ValidHash(hash) {
		return ErrConfiguration
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	_, major, err := c.profileVersion(ctx)
	if err != nil {
		return err
	}
	endpoint := "torrents/resume"
	if major == 5 {
		endpoint = "torrents/start"
	}
	raw, err := c.request(ctx, http.MethodPost, endpoint, url.Values{"hashes": {hash}})
	if err != nil {
		return err
	}
	if body := strings.TrimSpace(string(raw)); body != "" && body != "Ok." {
		return ErrResponse
	}
	return nil
}
