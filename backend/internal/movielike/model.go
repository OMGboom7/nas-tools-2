package movielike

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const Plugin = "MovieLike"
const ConfigKey = "plugin.MovieLike"
const ControlKey = "GoMovieLike"

var ErrState = errors.New("favorite movie authority or original receipt changed")
var ErrMissing = errors.New("favorite movie receipt not found")
var ErrLimit = errors.New("favorite movie complete scope exceeds budget")

func Hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Hex(s string, n int) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(s) == n && hex.EncodeToString(b) == s
}
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func Text(s string, n int) bool {
	return utf8.ValidString(s) && len(s) <= n && !strings.ContainsAny(s, "\x00\r\n")
}
func Component(s string) bool {
	return s != "" && s != "." && s != ".." && Text(s, 255) && !strings.ContainsAny(s, "/\\") && !strings.HasPrefix(s, ".nastool-")
}
func Values(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	if len(raw) > 64<<10 {
		return nil, ErrLimit
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if dec.Decode(&v) != nil || v == nil {
		return nil, ErrState
	}
	var tail any
	if dec.Decode(&tail) != io.EOF {
		return nil, ErrState
	}
	return v, nil
}
func MediaPath(s string) (string, error) {
	if !Text(s, 4096) || strings.TrimSpace(s) != s {
		return "", ErrState
	}
	s = strings.ReplaceAll(s, "\\", "/")
	drive := len(s) > 3 && ((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) && s[1] == ':' && s[2] == '/'
	if !path.IsAbs(s) && !drive || strings.Contains(s, "://") || strings.HasPrefix(s, "//") {
		return "", ErrState
	}
	rest := s
	if drive {
		rest = s[2:]
	}
	if path.Clean(rest) != rest || rest == "/" || strings.HasSuffix(rest, "/") {
		return "", ErrState
	}
	return s, nil
}
func mappingPath(s string) (string, error) {
	if s == "/" {
		return s, nil
	}
	if len(s) == 3 && s[1:] == ":/" && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		return s, nil
	}
	return MediaPath(s)
}

type Mapping struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}
type Spec struct {
	Enabled   bool      `json:"enabled"`
	Directory string    `json:"directory"`
	Mappings  []Mapping `json:"mappings"`
}

func ParseSpec(raw string) (Spec, error) {
	s := Spec{Mappings: []Mapping{}}
	v, e := Values(raw)
	if e != nil {
		return s, e
	}
	if b, ok := v["enable"]; ok {
		var yes bool
		yes, ok = b.(bool)
		if !ok {
			return s, ErrState
		}
		s.Enabled = yes
	}
	if d, ok := v["dir_name"]; ok {
		s.Directory, ok = d.(string)
		if !ok {
			return s, ErrState
		}
	}
	if s.Directory != "" && !Component(s.Directory) {
		return s, ErrState
	}
	for _, suffix := range []string{"", "2", "3"} {
		local, remote := "", ""
		for key, dst := range map[string]*string{"local_path" + suffix: &local, "remote_path" + suffix: &remote} {
			if val, ok := v[key]; ok && val != nil {
				str, ok := val.(string)
				if !ok {
					return s, ErrState
				}
				*dst = str
			}
		}
		if local == "" && remote == "" {
			continue
		}
		l, e := mappingPath(local)
		if e != nil || !path.IsAbs(l) {
			return s, ErrState
		}
		r, e := mappingPath(remote)
		if e != nil {
			return s, ErrState
		}
		s.Mappings = append(s.Mappings, Mapping{l, r})
	}
	return s, nil
}

// Apply each old mapping in its stored order, replacing the leading complete
// path component once. A sibling prefix and a later matching substring cannot
// redirect a callback into another directory.
func (s Spec) Map(p string) (string, error) {
	p, e := MediaPath(p)
	if e != nil {
		return "", e
	}
	for _, m := range s.Mappings {
		prefix := strings.TrimSuffix(m.Remote, "/") + "/"
		if p == m.Remote || strings.HasPrefix(p, prefix) {
			suffix := strings.TrimPrefix(strings.TrimPrefix(p, m.Remote), "/")
			p = path.Join(m.Local, suffix)
		}
	}
	if !path.IsAbs(p) {
		return "", ErrState
	}
	return MediaPath(p)
}

type Event struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	ItemID string `json:"itemId"`
}

func Parse(raw []byte) (Event, bool, error) {
	e := Event{}
	if len(raw) > 1<<20 {
		return e, false, ErrLimit
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if dec.Decode(&v) != nil || v == nil {
		return e, false, ErrState
	}
	var tail any
	if dec.Decode(&tail) != io.EOF {
		return e, false, ErrState
	}
	if v["Event"] != "item.rate" {
		return e, false, nil
	}
	item, ok := v["Item"].(map[string]any)
	if !ok || item["Type"] != "Movie" {
		return e, false, nil
	}
	e.Path, ok = item["Path"].(string)
	if !ok {
		return e, true, ErrState
	}
	normalized, err := MediaPath(e.Path)
	if err != nil {
		return e, true, err
	}
	e.Path = normalized
	if n, yes := item["Name"]; yes && n != nil {
		e.Name, ok = n.(string)
		if !ok || !Text(e.Name, 600) {
			return Event{}, true, ErrState
		}
	}
	if id, yes := item["Id"]; yes && id != nil {
		e.ItemID, ok = id.(string)
		if n, number := id.(json.Number); number {
			e.ItemID, ok = n.String(), true
		}
		if !ok || !Text(e.ItemID, 512) {
			return Event{}, true, ErrState
		}
	}
	return e, true, nil
}

type Root struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	Identity       string `json:"identity"`
	ConfiguredPath string `json:"configuredPath"`
}
type Control struct {
	Epoch, Actor, Authority, AppDigest, CategoryDigest, ConfigDigest, MediaID, Created string
	Authorized, HTTP                                                                   bool
	Roots                                                                              []Root
	Categories                                                                         []string
}

func DecodeControl(raw string) (Control, error) {
	c := Control{}
	if raw == "" {
		return c, nil
	}
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.DisallowUnknownFields()
	if len(raw) > 128<<10 || dec.Decode(&c) != nil || !Hex(c.Epoch, 32) || c.Actor == "" || !Text(c.Actor, 256) || !Hex(c.Authority, 64) || !Hex(c.AppDigest, 64) || !Hex(c.ConfigDigest, 64) {
		return c, ErrState
	}
	var tail any
	if dec.Decode(&tail) != io.EOF {
		return c, ErrState
	}
	if _, e := time.Parse(time.RFC3339Nano, c.Created); e != nil {
		return c, ErrState
	}
	if !c.Authorized && c.MediaID == "" && len(c.Roots) == 0 && len(c.Categories) == 0 {
		return c, nil
	}
	if !Hex(c.CategoryDigest, 64) || c.MediaID == "" || !Text(c.MediaID, 512) || len(c.Roots) == 0 || len(c.Roots) > 128 || len(c.Categories) == 0 || len(c.Categories) > 1000 {
		return c, ErrState
	}
	seen := map[string]bool{}
	for _, r := range c.Roots {
		p, e := MediaPath(r.Path)
		if e != nil || !path.IsAbs(p) || r.ID == "" || r.Identity == "" || seen[r.ID] {
			return c, ErrState
		}
		if r.ConfiguredPath != "" {
			p, e = MediaPath(r.ConfiguredPath)
			if e != nil || !path.IsAbs(p) {
				return c, ErrState
			}
		}
		seen[r.ID] = true
	}
	seen = map[string]bool{}
	for _, cat := range c.Categories {
		if !Component(cat) || seen[cat] {
			return c, ErrState
		}
		seen[cat] = true
	}
	return c, nil
}

type Review struct {
	ID, Basis, Created string
	Control            Control
	Spec               Spec
}
type Definition struct {
	Control  Control
	Spec     Spec
	Callback string
	Event    Event
}
type Receipt struct {
	ID              string          `json:"id"`
	Callback        string          `json:"callbackId"`
	Event           Event           `json:"event"`
	State           string          `json:"state"`
	Phase           string          `json:"phase"`
	Reason          string          `json:"reason"`
	Actor           string          `json:"actor"`
	Created         string          `json:"created"`
	Updated         string          `json:"updated"`
	Definition      Definition      `json:"-"`
	Fingerprint     string          `json:"-"`
	Work            json.RawMessage `json:"-"`
	WorkFingerprint string          `json:"-"`
	Proof           json.RawMessage `json:"-"`
}
