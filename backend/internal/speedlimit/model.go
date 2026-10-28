package speedlimit

import (
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

const Plugin = "SpeedLimiter"
const ConfigKey = "plugin.SpeedLimiter"
const ControlKey = "GoSpeedLimiter"

var ErrState = errors.New("original playback limiter scope or receipt changed")
var ErrMissing = errors.New("playback limiter receipt not found")
var ErrOverride = errors.New("alternative Transmission speeds override the normal session limits")
var ErrObservation = errors.New("complete current playback sessions or rate settings unavailable")

type Spec struct {
	Download     int64    `json:"download"`
	Upload       int64    `json:"upload"`
	IdleDownload int64    `json:"idleDownload"`
	IdleUpload   int64    `json:"idleUpload"`
	Bandwidth    int64    `json:"bandwidth"`
	Interval     int64    `json:"interval"`
	IPv4         string   `json:"ipv4"`
	IPv6         string   `json:"ipv6"`
	Downloaders  []string `json:"downloaders"`
	Ratios       []int64  `json:"ratios"`
	Notify       bool     `json:"notify"`
}
type Target struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Weight int64  `json:"weight"`
}

// qBittorrent values are bytes/s, Transmission values are its RPC KB/s.
// Keep exact observed numeric values, including valid fractional Transmission
// values, instead of rounding a baseline before comparing ownership.
type Rates struct {
	Upload          float64 `json:"upload"`
	Download        float64 `json:"download"`
	OtherUpload     float64 `json:"otherUpload"`
	OtherDownload   float64 `json:"otherDownload"`
	UploadLimited   bool    `json:"uploadLimited"`
	DownloadLimited bool    `json:"downloadLimited"`
	Alternative     bool    `json:"alternative"`
	Scheduled       bool    `json:"scheduled"`
	Unit            string  `json:"unit"`
}

func (r Rates) Valid(kind string) bool {
	if kind != "qbittorrent" && kind != "transmission" {
		return false
	}
	want := "B/s"
	max := float64(int64(1) << 40)
	if kind == "transmission" {
		want = "RPC KB/s"
		max = 1 << 30
	}
	if r.Unit != want {
		return false
	}
	for _, v := range []float64{r.Upload, r.Download, r.OtherUpload, r.OtherDownload} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > max || kind == "qbittorrent" && v != math.Trunc(v) {
			return false
		}
	}
	return kind != "qbittorrent" || (r.UploadLimited == (r.Upload > 0) && r.DownloadLimited == (r.Download > 0))
}
func (r Rates) Desired(kind string, download, upload int64) (Rates, error) {
	if !r.Valid(kind) || download < 0 || upload < 0 || download > 1<<30 || upload > 1<<30 {
		return r, ErrState
	}
	if kind == "transmission" && r.Alternative {
		return r, ErrOverride
	}
	scale := int64(1)
	if kind == "qbittorrent" {
		scale = 1024
	}
	if kind == "qbittorrent" && (upload*scale > 1<<31-1 || download*scale > 1<<31-1) {
		return r, ErrState
	}
	r.Upload = float64(upload * scale)
	r.Download = float64(download * scale)
	r.UploadLimited = upload > 0
	r.DownloadLimited = download > 0
	return r, nil
}

type Baseline struct {
	Target Target `json:"target"`
	Rates  Rates  `json:"rates"`
}
type Control struct {
	Epoch        string     `json:"epoch"`
	Actor        string     `json:"actor"`
	Authority    string     `json:"authority"`
	ConfigDigest string     `json:"configDigest"`
	MediaDigest  string     `json:"mediaDigest"`
	Server       string     `json:"server"`
	MediaID      string     `json:"mediaId"`
	PlexAccount  bool       `json:"plexAccount"`
	PlexHTTP     bool       `json:"plexHTTP"`
	Created      string     `json:"created"`
	Authorized   bool       `json:"authorized"`
	HTTP         bool       `json:"http"`
	Baselines    []Baseline `json:"baselines"`
}
type Observation struct {
	Playing        int   `json:"playing"`
	Exempt         int   `json:"exempt"`
	UnknownBitrate int   `json:"unknownBitrate"`
	Bitrate        int64 `json:"bitrate"`
}

func (s Spec) Limits(observation Observation, targets []Target) ([][2]int64, error) {
	if observation.Playing < 0 || observation.Exempt < 0 || observation.Playing+observation.Exempt > 1000 || observation.UnknownBitrate < 0 || observation.UnknownBitrate > observation.Playing || observation.Bitrate < 0 || observation.Bitrate > 1<<50 || len(targets) < 1 || len(targets) > 64 {
		return nil, ErrObservation
	}
	var weight int64
	for _, t := range targets {
		if t.Weight < 1 || t.Weight > 1000000 {
			return nil, ErrState
		}
		weight += t.Weight
	}
	pool := s.Upload
	if observation.Playing > 0 && s.Bandwidth > 0 {
		if observation.Bitrate == 0 || observation.UnknownBitrate > 0 {
			return nil, ErrObservation
		}
		pool = (s.Bandwidth*1000000 - observation.Bitrate) / (8 * 1024)
		if pool < 0 {
			pool = 10
		}
	}
	out := make([][2]int64, len(targets))
	for i, t := range targets {
		download, upload := s.IdleDownload, s.IdleUpload
		if observation.Playing > 0 {
			download, upload = s.Download, s.Upload
			if s.Bandwidth > 0 {
				upload = pool * t.Weight / weight
				if upload < 10 {
					upload = 10
				}
			}
		}
		out[i] = [2]int64{download, upload}
	}
	return out, nil
}
func Hex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func decodeNumber(v any, empty int64, max int64) (int64, error) {
	if v == nil || v == "" {
		return empty, nil
	}
	var raw string
	switch v := v.(type) {
	case string:
		raw = v
	case json.Number:
		raw = string(v)
	default:
		return 0, ErrState
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > float64(max) {
		return 0, ErrState
	}
	return int64(f), nil // Preserve the legacy int(float(value)) conversion.
}
func DecodeSpec(raw string) (Spec, error) {
	s := Spec{Interval: 300, IPv4: "0.0.0.0/0", IPv6: "::/0", Downloaders: []string{}, Ratios: []int64{}}
	var v map[string]any
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if raw == "" {
		return s, nil
	}
	if len(raw) > 64<<10 || !json.Valid([]byte(raw)) || d.Decode(&v) != nil || v == nil {
		return s, ErrState
	}
	for key, p := range map[string]*int64{"download_limit": &s.Download, "upload_limit": &s.Upload, "download_unlimit": &s.IdleDownload, "upload_unlimit": &s.IdleUpload, "bandwidth": &s.Bandwidth, "interval": &s.Interval} {
		def, max := int64(0), int64(1<<30)
		if key == "interval" {
			def, max = 300, 86400
		}
		if key == "bandwidth" {
			max = 1000000
		}
		n, err := decodeNumber(v[key], def, max)
		if err != nil {
			return s, err
		}
		*p = n
	}
	if s.Interval < 1 {
		return s, ErrState
	}
	for key, p := range map[string]*string{"ipv4": &s.IPv4, "ipv6": &s.IPv6} {
		if value := v[key]; value != nil {
			text, ok := value.(string)
			if !ok {
				return s, ErrState
			}
			if text != "" {
				*p = text
			}
		}
		if _, err := Ranges(*p, key == "ipv4"); err != nil {
			return s, err
		}
	}
	if v["notify"] != nil {
		flag, ok := v["notify"].(bool)
		if !ok {
			return s, ErrState
		}
		s.Notify = flag
	}
	if v["downloaders"] != nil {
		ids, ok := v["downloaders"].([]any)
		if !ok || len(ids) > 64 {
			return s, ErrState
		}
		seen := map[string]bool{}
		for _, id := range ids {
			var value string
			switch id := id.(type) {
			case string:
				value = id
			case json.Number:
				value = string(id)
			default:
				return s, ErrState
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 1 || strconv.FormatInt(n, 10) != value || seen[value] {
				return s, ErrState
			}
			seen[value] = true
			s.Downloaders = append(s.Downloaders, value)
		}
	}
	if value := v["allocation_ratio"]; value != nil && value != "" {
		raw, ok := value.(string)
		if !ok {
			return s, ErrState
		}
		valid := true
		for _, part := range strings.Split(raw, ":") {
			n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err != nil || n < 1 || n > 1000000 {
				valid = false
				break
			}
			s.Ratios = append(s.Ratios, n)
		}
		// The original plugin falls back to equal allocation for malformed or
		// mismatched ratios. The review explicitly displays the resulting weights.
		if !valid || len(s.Ratios) != len(s.Downloaders) {
			s.Ratios = []int64{}
		}
	}
	return s, nil
}
func Ranges(raw string, ipv4 bool) ([]netip.Prefix, error) {
	if len(raw) > 16000 {
		return nil, ErrState
	}
	out := []netip.Prefix{}
	if strings.TrimSpace(raw) == "" {
		raw = "::/0"
		if ipv4 {
			raw = "0.0.0.0/0"
		}
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 256 {
		return nil, ErrState
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		p, err := netip.ParsePrefix(part)
		if err != nil {
			a, e := netip.ParseAddr(part)
			if e != nil {
				return nil, ErrState
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4() != ipv4 || p.Addr().Is4In6() || p.Addr().Zone() != "" || p != p.Masked() {
			return nil, ErrState
		}
		out = append(out, p)
	}
	return out, nil
}
func (s Spec) ExemptAddress(raw string) (bool, error) {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		if ap, e := netip.ParseAddrPort(raw); e == nil {
			addr, err = ap.Addr(), nil
		}
	}
	if err != nil || addr.Zone() != "" {
		return false, ErrObservation
	}
	addr = addr.Unmap()
	ranges, err := Ranges(s.IPv6, false)
	if addr.Is4() {
		ranges, err = Ranges(s.IPv4, true)
	}
	if err != nil {
		return false, err
	}
	for _, p := range ranges {
		if p.Contains(addr) {
			return true, nil
		}
	}
	return false, nil
}
func DecodeControl(raw string) (Control, error) {
	c := Control{}
	if raw == "" {
		return c, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 256<<10 || !json.Valid([]byte(raw)) || d.Decode(&c) != nil || !Hex(c.Epoch, 32) || !Hex(c.Authority, 64) || !Hex(c.ConfigDigest, 64) || !Hex(c.MediaDigest, 64) || c.Actor == "" || len(c.Actor) > 256 {
		return c, ErrState
	}
	if c.Server != "emby" && c.Server != "jellyfin" && c.Server != "plex" {
		return c, ErrState
	}
	if len(c.MediaID) > 512 || c.Authorized && c.MediaID == "" || c.PlexHTTP && !c.PlexAccount {
		return c, ErrState
	}
	if _, err := time.Parse(time.RFC3339Nano, c.Created); err != nil {
		return c, ErrState
	}
	if len(c.Baselines) > 64 {
		return c, ErrState
	}
	seen := map[string]bool{}
	for _, b := range c.Baselines {
		n, err := strconv.ParseInt(b.Target.ID, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != b.Target.ID || seen[b.Target.ID] || !Hex(b.Target.Digest, 64) || !b.Rates.Valid(b.Target.Kind) || b.Target.Weight < 1 || b.Target.Weight > 1000000 {
			return c, ErrState
		}
		seen[b.Target.ID] = true
	}
	if c.Authorized && len(c.Baselines) == 0 {
		return c, ErrState
	}
	return c, nil
}
func Digest(v any) string { return bus.Hash(v) }
