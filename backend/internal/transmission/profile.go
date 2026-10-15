package transmission

import (
	"context"
	"encoding/json"
	"math"
	"strings"
)

// IdleMinutes is a maximum idle seeding duration, NOT minimum seeding time.
type Limits struct {
	UploadKB    int     `json:"uploadKB"`
	DownloadKB  int     `json:"downloadKB"`
	Ratio       float64 `json:"ratio"`
	IdleMinutes int     `json:"idleMinutes"`
}

func (l Limits) Valid() bool {
	return l.UploadKB >= 0 && l.UploadKB <= 1<<30 && l.DownloadKB >= 0 && l.DownloadKB <= 1<<30 && l.IdleMinutes >= 0 && l.IdleMinutes <= 1<<30 && l.Ratio >= 0 && l.Ratio <= 1e6 && !math.IsNaN(l.Ratio) && !math.IsInf(l.Ratio, 0)
}

type ProfileState struct {
	Hash            string   `json:"hash"`
	Directory       string   `json:"directory"`
	Labels          []string `json:"labels"`
	Status          int      `json:"status"`
	UploadKB        int      `json:"uploadKB"`
	DownloadKB      int      `json:"downloadKB"`
	UploadLimited   bool     `json:"uploadLimited"`
	DownloadLimited bool     `json:"downloadLimited"`
	Ratio           float64  `json:"ratio"`
	RatioMode       int      `json:"ratioMode"`
	IdleMinutes     int      `json:"idleMinutes"`
	IdleMode        int      `json:"idleMode"`
}

func (s ProfileState) Matches(l Limits) bool {
	if !l.Valid() || s.UploadLimited != (l.UploadKB > 0) || s.DownloadLimited != (l.DownloadKB > 0) {
		return false
	}
	if l.UploadKB > 0 && s.UploadKB != l.UploadKB || l.DownloadKB > 0 && s.DownloadKB != l.DownloadKB {
		return false
	}
	if l.Ratio > 0 {
		if s.RatioMode != 1 || math.Abs(s.Ratio-l.Ratio) > 1e-9 {
			return false
		}
	} else if s.RatioMode != 2 {
		return false
	}
	if l.IdleMinutes > 0 {
		return s.IdleMode == 1 && s.IdleMinutes == l.IdleMinutes
	}
	return s.IdleMode == 2
}
func (client *Client) SetLimits(ctx context.Context, hash string, l Limits) error {
	if !ValidHash(hash) || !l.Valid() {
		return ErrConfiguration
	}
	ratioMode, idleMode := 2, 2
	if l.Ratio > 0 {
		ratioMode = 1
	}
	if l.IdleMinutes > 0 {
		idleMode = 1
	}
	legacy := map[string]any{"ids": []string{hash}, "uploadLimited": l.UploadKB > 0, "uploadLimit": l.UploadKB, "downloadLimited": l.DownloadKB > 0, "downloadLimit": l.DownloadKB, "seedRatioMode": ratioMode, "seedRatioLimit": l.Ratio, "seedIdleMode": idleMode, "seedIdleLimit": l.IdleMinutes}
	modern := map[string]any{"ids": []string{hash}, "upload_limited": l.UploadKB > 0, "upload_limit": l.UploadKB, "download_limited": l.DownloadKB > 0, "download_limit": l.DownloadKB, "seed_ratio_mode": ratioMode, "seed_ratio_limit": l.Ratio, "seed_idle_mode": idleMode, "seed_idle_limit": l.IdleMinutes}
	lb, _ := json.Marshal(map[string]any{"method": "torrent-set", "arguments": legacy})
	mb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "torrent_set", "params": modern, "id": 1})
	body, newProtocol, err := client.exchange(ctx, lb, mb)
	if err != nil {
		return err
	}
	if newProtocol {
		var r struct {
			JSONRPC string          `json:"jsonrpc"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &r) != nil || r.JSONRPC != "2.0" || hasJSONError(r.Error) || len(r.Result) == 0 || string(r.Result) == "null" {
			return ErrResponse
		}
		return nil
	}
	var r struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(body, &r) != nil || r.Result != "success" {
		return ErrResponse
	}
	return nil
}
func (client *Client) Profile(ctx context.Context, hash string) (ProfileState, error) {
	var s ProfileState
	if !ValidHash(hash) {
		return s, ErrConfiguration
	}
	legacyFields := []string{"hashString", "downloadDir", "labels", "status", "uploadLimit", "uploadLimited", "downloadLimit", "downloadLimited", "seedRatioLimit", "seedRatioMode", "seedIdleLimit", "seedIdleMode"}
	modernFields := []string{"hash_string", "download_dir", "labels", "status", "upload_limit", "upload_limited", "download_limit", "download_limited", "seed_ratio_limit", "seed_ratio_mode", "seed_idle_limit", "seed_idle_mode"}
	lb, _ := json.Marshal(map[string]any{"method": "torrent-get", "arguments": map[string]any{"ids": []string{hash}, "fields": legacyFields}})
	mb, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "torrent_get", "params": map[string]any{"ids": []string{hash}, "fields": modernFields}, "id": 1})
	body, modern, err := client.exchange(ctx, lb, mb)
	if err != nil {
		return s, err
	}
	var envelope struct {
		JSONRPC   string          `json:"jsonrpc"`
		Result    json.RawMessage `json:"result"`
		Arguments json.RawMessage `json:"arguments"`
		Error     json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return s, ErrResponse
	}
	data := envelope.Arguments
	if modern {
		if envelope.JSONRPC != "2.0" || hasJSONError(envelope.Error) {
			return s, ErrResponse
		}
		data = envelope.Result
	} else if string(envelope.Result) != `"success"` {
		return s, ErrResponse
	}
	var result struct {
		Torrents []map[string]json.RawMessage `json:"torrents"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Torrents) != 1 {
		return s, ErrResponse
	}
	row := result.Torrents[0]
	outputs := []any{&s.Hash, &s.Directory, &s.Labels, &s.Status, &s.UploadKB, &s.UploadLimited, &s.DownloadKB, &s.DownloadLimited, &s.Ratio, &s.RatioMode, &s.IdleMinutes, &s.IdleMode}
	fields := legacyFields
	if modern {
		fields = modernFields
	}
	for i, key := range fields {
		raw, ok := row[key]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, outputs[i]) != nil {
			return ProfileState{}, ErrResponse
		}
	}
	if !ValidHash(s.Hash) || !strings.EqualFold(s.Hash, hash) || s.Directory == "" || len(s.Directory) > 4096 || s.Status < 0 || s.Status > 6 || s.UploadKB < 0 || s.DownloadKB < 0 || s.Ratio < 0 || math.IsNaN(s.Ratio) || math.IsInf(s.Ratio, 0) || s.RatioMode < 0 || s.RatioMode > 2 || s.IdleMode < 0 || s.IdleMode > 2 || s.IdleMinutes < 0 || s.Labels == nil || len(s.Labels) > 64 {
		return ProfileState{}, ErrResponse
	}
	seen := map[string]bool{}
	for _, label := range s.Labels {
		if label == "" || len(label) > 128 || strings.ContainsAny(label, "\r\n\x00") || seen[label] {
			return ProfileState{}, ErrResponse
		}
		seen[label] = true
	}
	s.Hash = strings.ToLower(s.Hash)
	return s, nil
}
