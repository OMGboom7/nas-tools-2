// Package opensubtitles implements the original Chinese subtitle consumer.
// Protocol clients do not write files or retry operations; the caller journals
// each request and publishes returned bytes beside its independently proven video.
package opensubtitles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

const Plugin = "OpenSubtitles"
const ConfigKey = "plugin.OpenSubtitles"
const ControlKey = "GoOpenSubtitlesControl"
const Legacy = "org"
const REST = "com"
const DefaultAgent = "NAS-Tools-Go/0.1"
const MaxDocument = 512 << 10

var ErrConfig = errors.New("original OpenSubtitles configuration unavailable or changed")
var ErrState = errors.New("original OpenSubtitles receipt or scope changed")
var ErrChallenge = errors.New("subtitle website requires verification or returned an unsupported page")
var ErrLimit = errors.New("subtitle response or archive exceeds the supported budget")
var ErrResponse = errors.New("subtitle service did not positively acknowledge the original request")
var ErrQuota = errors.New("subtitle service denied the request or reached its limit; no automatic retry")

type Reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type Config struct {
	Enabled                                                 bool
	Provider, APIKey, Username, Password, Cookie, UserAgent string
}
type Control struct {
	Version    int    `json:"version"`
	Epoch      string `json:"epoch"`
	Actor      string `json:"actor"`
	Authority  string `json:"authority"`
	Config     string `json:"config"`
	Authorized bool   `json:"authorized"`
	Remote     bool   `json:"remote"`
	Created    string `json:"created"`
}

// Snapshot is safe to include in a private media definition: no cookie, API key,
// password, login token or temporary download URL is copied into it.
type Snapshot struct {
	Basis, Config, Epoch, Provider, Origin, Actor, Authority string
	Remote                                                   bool
}

func (s Snapshot) Valid() bool {
	return Hex(s.Basis, 64) && Hex(s.Config, 64) && Hex(s.Epoch, 32) && Text(s.Actor, 256) && Hex(s.Authority, 64) && Origin(s.Provider) != "" && s.Origin == Origin(s.Provider)
}
func Origin(provider string) string {
	if provider == Legacy {
		return "https://www.opensubtitles.org"
	}
	if provider == REST {
		return "https://api.opensubtitles.com"
	}
	return ""
}
func Hex(s string, n int) bool { return len(s) == n && strings.Trim(s, "0123456789abcdef") == "" }
func Text(s string, n int) bool {
	return s != "" && len(s) <= n && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func Decode(raw string, into any) error { return DecodeLimit(raw, into, MaxDocument) }
func DecodeLimit(raw string, into any, limit int) error {
	if len(raw) == 0 || limit < 1 || limit > 64<<20 || len(raw) > limit || !utf8.ValidString(raw) {
		return ErrConfig
	}
	// Duplicate keys are ambiguous even if encoding/json would accept them.
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	tokens := 0
	var visit func(int) error
	visit = func(depth int) error {
		tokens++
		if depth > 24 || tokens > 2000000 {
			return ErrLimit
		}
		t, e := d.Token()
		if e != nil {
			return ErrConfig
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				key, ok := k.(string)
				if e != nil || !ok || seen[key] {
					return ErrConfig
				}
				seen[key] = true
				if e = visit(depth + 1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim('}') {
				return ErrConfig
			}
		case '[':
			for d.More() {
				if e := visit(depth + 1); e != nil {
					return e
				}
			}
			end, e := d.Token()
			if e != nil || end != json.Delim(']') {
				return ErrConfig
			}
		default:
			return ErrConfig
		}
		return nil
	}
	if e := visit(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrConfig
	}
	d = json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	if d.Decode(into) != nil {
		return ErrConfig
	}
	return nil
}
func Parse(raw string) (Config, error) {
	c := Config{Provider: Legacy}
	if raw == "" {
		return c, nil
	}
	var v map[string]json.RawMessage
	if len(raw) > 64<<10 || Decode(raw, &v) != nil || v == nil {
		return c, ErrConfig
	}
	if value, ok := v["enable"]; ok && json.Unmarshal(value, &c.Enabled) != nil {
		return c, ErrConfig
	}
	for key, target := range map[string]*string{"provider": &c.Provider, "api_key": &c.APIKey, "username": &c.Username, "password": &c.Password, "cookie": &c.Cookie, "user_agent": &c.UserAgent} {
		if value, ok := v[key]; ok && json.Unmarshal(value, target) != nil {
			return c, ErrConfig
		}
	}
	if c.Provider == "" {
		c.Provider = Legacy
	}
	if c.UserAgent == "" && c.Provider == REST {
		c.UserAgent = DefaultAgent
	}
	if Origin(c.Provider) == "" || c.UserAgent != "" && !Text(c.UserAgent, 256) {
		return c, ErrConfig
	}
	for _, s := range []string{c.APIKey, c.Username, c.Password, c.Cookie} {
		if len(s) > 16000 || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n") {
			return c, ErrConfig
		}
	}
	if c.Provider == REST && c.Enabled && (!Text(c.APIKey, 4096) || !Text(c.Username, 256) || !Text(c.Password, 4096)) {
		return c, ErrConfig
	}
	return c, nil
}
func ReadSetting(ctx context.Context, r Reader, key string) (string, error) {
	var exists bool
	if e := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='SYSTEM_DICT')`).Scan(&exists); e != nil || !exists {
		return "", e
	}
	var raw string
	e := r.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, key).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return raw, e
}

// Wanted reports the original installed+enabled plugin even if the independent
// native grant is absent. Callers must not silently run just another consumer.
func Current(ctx context.Context, r Reader) (c Config, s Snapshot, wanted, ready bool, err error) {
	installed, e := ReadSetting(ctx, r, "UserInstalledPlugins")
	if e != nil {
		return c, s, false, false, e
	}
	raw, e := ReadSetting(ctx, r, ConfigKey)
	if e != nil {
		return c, s, false, false, e
	}
	control, e := ReadSetting(ctx, r, ControlKey)
	if e != nil {
		return c, s, false, false, e
	}
	var ids []string
	if installed != "" && Decode(installed, &ids) != nil {
		return c, s, false, false, ErrConfig
	}
	present := false
	for _, id := range ids {
		present = present || id == Plugin
	}
	if !present {
		return c, s, false, false, nil
	}
	c, e = Parse(raw)
	if e != nil {
		return c, s, present, false, e
	}
	wanted = present && c.Enabled
	if !wanted {
		return c, s, false, false, nil
	}
	var grant Control
	if control == "" {
		return c, s, true, false, nil
	}
	if Decode(control, &grant) != nil || grant.Version != 1 || !Hex(grant.Epoch, 32) || !Text(grant.Actor, 256) || !Hex(grant.Authority, 64) || !Hex(grant.Config, 64) {
		return c, s, true, false, ErrConfig
	}
	if _, e = time.Parse(time.RFC3339Nano, grant.Created); e != nil {
		return c, s, true, false, ErrConfig
	}
	if !grant.Authorized || grant.Config != bus.Hash(raw) {
		return c, s, true, false, nil
	}
	s = Snapshot{Basis: bus.Hash([]string{installed, raw, control}), Config: bus.Hash(raw), Epoch: grant.Epoch, Provider: c.Provider, Origin: Origin(c.Provider), Actor: grant.Actor, Authority: grant.Authority, Remote: grant.Remote}
	return c, s, true, s.Valid(), nil
}
