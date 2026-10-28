package mediasyncdel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const Plugin = "MediaSyncDel"
const ConfigKey = "plugin.MediaSyncDel"
const ControlKey = "GoMediaSyncDel"
const HistoryLimit = 20000

var ErrState = errors.New("original media deletion authority or receipt changed")
var ErrMissing = errors.New("original media deletion receipt not found")
var ErrLimit = errors.New("complete media deletion selection exceeds explicit limits")

func Hash(v any) string { return bus.Hash(v) }
func Hex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func Values(raw string) (map[string]any, error) {
	v := map[string]any{}
	if raw == "" {
		return v, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if len(raw) > 64<<10 || !json.Valid([]byte(raw)) || d.Decode(&v) != nil || v == nil {
		return nil, ErrState
	}
	return v, nil
}

type Spec struct {
	Enabled      bool   `json:"enabled"`
	DeleteSource bool   `json:"deleteSource"`
	Notify       bool   `json:"notify"`
	Exclude      string `json:"exclude"`
}

func DecodeSpec(raw string) (Spec, error) {
	s := Spec{}
	v, err := Values(raw)
	if err != nil {
		return s, err
	}
	for k, p := range map[string]*bool{"enable": &s.Enabled, "del_source": &s.DeleteSource, "send_notify": &s.Notify} {
		if value := v[k]; value != nil {
			n, ok := value.(bool)
			if !ok {
				return s, ErrState
			}
			*p = n
		}
	}
	if v["exclude_path"] != nil {
		var ok bool
		s.Exclude, ok = v["exclude_path"].(string)
		if !ok {
			return s, ErrState
		}
	}
	if len(s.Exclude) > 16000 {
		return s, ErrLimit
	}
	if strings.TrimSpace(s.Exclude) != "" {
		parts := strings.Split(s.Exclude, ",")
		if len(parts) > 256 {
			return s, ErrLimit
		}
		for _, p := range parts {
			if _, err := excludePath(p); err != nil {
				return s, err
			}
		}
	}
	return s, nil
}
func ConfigDigest(raw string) string {
	v, err := Values(raw)
	if err != nil {
		return ""
	}
	return Hash(v)
}
func validText(v string, n int) bool {
	return len(v) <= n && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}

// Callback paths select versions and exclusions only. They are never opened,
// used as deletion roots, or followed as URLs.
func MediaPath(raw string) (string, error) {
	if raw == "" || !validText(raw, 4096) {
		return "", ErrState
	}
	p := strings.ReplaceAll(raw, `\`, "/")
	windows := len(p) > 2 && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') && p[1:3] == ":/"
	if strings.Contains(p, "://") || !path.IsAbs(p) && !windows || path.Clean(p) != p || p == "/" || strings.Contains(p, "//") || strings.HasSuffix(p, "/") {
		return "", ErrState
	}
	return p, nil
}
func (s Spec) Excluded(p string) bool {
	if strings.TrimSpace(s.Exclude) == "" {
		return false
	}
	for _, raw := range strings.Split(s.Exclude, ",") {
		base, err := excludePath(raw)
		if err == nil && (p == base || base == "/" && strings.HasPrefix(p, "/") || strings.HasSuffix(base, "/") && strings.HasPrefix(p, base) || strings.HasPrefix(p, base+"/")) {
			return true
		}
	}
	return false
}
func excludePath(raw string) (string, error) {
	p := strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/")
	if p == "/" {
		return p, nil
	}
	if len(p) == 3 && p[1:] == ":/" && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') {
		return p, nil
	}
	return MediaPath(strings.TrimRight(p, "/"))
}

type Event struct {
	Type           string `json:"type"`
	Name           string `json:"name"`
	Path           string `json:"path"`
	TMDB           string `json:"tmdbId"`
	Season         int    `json:"season"`
	Episode        int    `json:"episode"`
	Virtual        bool   `json:"virtual"`
	MissingVirtual bool   `json:"missingVirtual"`
}

func callbackText(v json.RawMessage) (string, error) {
	if len(v) == 0 || bytes.Equal(v, []byte("null")) {
		return "", nil
	}
	var s string
	if v[0] == '"' {
		if json.Unmarshal(v, &s) != nil {
			return "", ErrState
		}
	} else {
		var n json.Number
		if json.Unmarshal(v, &n) != nil {
			return "", ErrState
		}
		s = string(n)
	}
	if !validText(s, 4096) {
		return "", ErrState
	}
	return s, nil
}
func Parse(raw json.RawMessage) (Event, bool, error) {
	e := Event{}
	var values map[string]json.RawMessage
	if len(raw) > 1<<20 || json.Unmarshal(raw, &values) != nil || values == nil {
		return e, false, ErrState
	}
	action, err := callbackText(values["event_type"])
	if err != nil {
		return e, false, nil
	}
	if action != "media_del" {
		return e, false, nil
	}
	flag := values["item_isvirtual"]
	if len(flag) == 0 || string(flag) == "null" || string(flag) == `""` {
		e.MissingVirtual = true
		return e, true, nil
	}
	var boolean bool
	if json.Unmarshal(flag, &boolean) == nil {
		e.Virtual = boolean
	} else {
		v, err := callbackText(flag)
		if err != nil {
			return e, true, ErrState
		}
		switch strings.ToLower(v) {
		case "true":
			e.Virtual = true
		case "false":
		default:
			e.MissingVirtual = true
			return e, true, nil
		}
	}
	if e.Virtual {
		return e, true, nil
	}
	for key, p := range map[string]*string{"media_type": &e.Type, "media_name": &e.Name, "media_path": &e.Path, "tmdb_id": &e.TMDB} {
		v, err := callbackText(values[key])
		if err != nil {
			return e, true, err
		}
		*p = v
	}
	if e.Type != "Movie" && e.Type != "Series" && e.Type != "Season" && e.Type != "Episode" {
		return e, true, ErrState
	}
	if e.Path, err = MediaPath(e.Path); err != nil {
		return e, true, err
	}
	n, err := strconv.ParseInt(e.TMDB, 10, 64)
	if err != nil || n < 1 {
		return e, true, ErrState
	}
	e.TMDB = strconv.FormatInt(n, 10)
	for key, p := range map[string]*int{"season_num": &e.Season, "episode_num": &e.Episode} {
		text, err := callbackText(values[key])
		if err != nil {
			return e, true, err
		}
		if text == "" {
			continue
		}
		n, err := strconv.Atoi(text)
		if err != nil || n < 0 || n > 99999 {
			return e, true, ErrState
		}
		*p = n
	}
	if e.Type == "Season" || e.Type == "Episode" {
		if len(values["season_num"]) == 0 || string(values["season_num"]) == "null" || string(values["season_num"]) == `""` {
			return e, true, ErrState
		}
	}
	if e.Type == "Episode" && (len(values["episode_num"]) == 0 || string(values["episode_num"]) == "null" || string(values["episode_num"]) == `""`) {
		return e, true, ErrState
	}
	if len(e.Name) > 2000 {
		return e, true, ErrLimit
	}
	return e, true, nil
}

type Root struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Identity string `json:"identity"`
}
type Control struct {
	Epoch        string `json:"epoch"`
	Actor        string `json:"actor"`
	Authority    string `json:"authority"`
	AppDigest    string `json:"appDigest"`
	ConfigDigest string `json:"configDigest"`
	MediaID      string `json:"mediaId"`
	Created      string `json:"created"`
	Authorized   bool   `json:"authorized"`
	HTTP         bool   `json:"http"`
	Roots        []Root `json:"roots"`
}

func DecodeControl(raw string) (Control, error) {
	c := Control{}
	if raw == "" {
		return c, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if len(raw) > 256<<10 || !json.Valid([]byte(raw)) || d.Decode(&c) != nil || !Hex(c.Epoch, 32) || !Hex(c.Authority, 64) || !Hex(c.AppDigest, 64) || !Hex(c.ConfigDigest, 64) || c.Actor == "" || !validText(c.Actor, 256) || len(c.Roots) > 256 {
		return c, ErrState
	}
	if _, err := time.Parse(time.RFC3339Nano, c.Created); err != nil {
		return c, ErrState
	}
	if c.Authorized && c.MediaID == "" {
		return c, ErrState
	}
	seen := map[string]bool{}
	for _, r := range c.Roots {
		if r.ID == "" || seen[r.ID] || r.Identity == "" || r.Path == "" {
			return c, ErrState
		}
		seen[r.ID] = true
	}
	return c, nil
}
func RequestID(receipt string) string { return Hash([]string{"media-sync-deletion", receipt})[:32] }
func EpisodeKey(e Event) string       { return fmt.Sprintf("S%02d E%02d", e.Season, e.Episode) }

var seasonNumber = regexp.MustCompile(`S([0-9]+)`)

func SeasonMatches(raw string, n int) bool {
	for _, m := range seasonNumber.FindAllStringSubmatch(raw, -1) {
		v, err := strconv.Atoi(m[1])
		if err == nil && v == n {
			return true
		}
	}
	return false
}
