package cloudflare

import (
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
)

const Plugin = "CloudflareSpeedTest"
const ConfigKey = "plugin." + Plugin

var ErrState = errors.New("original Cloudflare configuration or receipt is unavailable")
var ErrResult = errors.New("current Cloudflare speed test result is not confirmed")
var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:[-.][A-Za-z0-9.-]+)?$`)

type Settings struct {
	CFIP, Cron, Version, AdditionalArgs        string
	IPv4, IPv6, Check, Reinstall, Notify, Once bool
}

func object(raw string) (map[string]json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	if raw == "" {
		return values, nil
	}
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &values) != nil || values == nil {
		return nil, ErrState
	}
	return values, nil
}

func DecodeSettings(raw string) (Settings, error) {
	v, err := object(raw)
	if err != nil {
		return Settings{}, err
	}
	s := Settings{}
	for key, target := range map[string]*string{"cf_ip": &s.CFIP, "cron": &s.Cron, "version": &s.Version, "additional_args": &s.AdditionalArgs} {
		if b := v[key]; len(b) != 0 && string(b) != "null" && json.Unmarshal(b, target) != nil {
			return s, ErrState
		}
		*target = strings.TrimSpace(*target)
	}
	for key, target := range map[string]*bool{"ipv4": &s.IPv4, "ipv6": &s.IPv6, "check": &s.Check, "re_install": &s.Reinstall, "notify": &s.Notify, "onlyonce": &s.Once} {
		if b := v[key]; len(b) != 0 && string(b) != "null" && json.Unmarshal(b, target) != nil {
			return s, ErrState
		}
	}
	if s.CFIP != "" {
		if ip, err := netip.ParseAddr(s.CFIP); err != nil || ip.Zone() != "" {
			return s, ErrState
		}
	}
	if len(s.Cron) > 512 || s.Version != "" && !versionPattern.MatchString(s.Version) {
		return s, ErrState
	}
	if !s.IPv4 && !s.IPv6 {
		s.IPv4 = true
	}
	if _, err := Arguments(s.AdditionalArgs); err != nil {
		return s, err
	}
	return s, nil
}

// Patch preserves unknown JSON values, including integers beyond float64 range.
func Patch(raw string, updates map[string]json.RawMessage) (string, error) {
	v, err := object(raw)
	if err != nil {
		return "", err
	}
	for key, value := range updates {
		switch key {
		case "cf_ip", "cron", "version", "additional_args", "ipv4", "ipv6", "check", "re_install", "notify", "onlyonce":
		default:
			return "", ErrState
		}
		if !json.Valid(value) {
			return "", ErrState
		}
		v[key] = value
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if _, err := DecodeSettings(string(b)); err != nil {
		return "", err
	}
	return string(b), nil
}

// Arguments implements quoting only. It never expands variables, substitutions,
// redirections or commands; all output is passed directly to exec.Command.
func Arguments(raw string) ([]string, error) {
	if len(raw) > 4096 || strings.ContainsRune(raw, 0) {
		return nil, ErrState
	}
	var out []string
	var word strings.Builder
	var quote rune
	escaped, active := false, false
	flush := func() {
		if active {
			out = append(out, word.String())
			word.Reset()
			active = false
		}
	}
	for _, c := range raw {
		if escaped {
			word.WriteRune(c)
			active = true
			escaped = false
			continue
		}
		if c == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
			active = true
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			active = true
			continue
		}
		if unicode.IsSpace(c) {
			flush()
			continue
		}
		word.WriteRune(c)
		active = true
	}
	if escaped || quote != 0 {
		return nil, ErrState
	}
	flush()
	if len(out) > 128 {
		return nil, ErrState
	}
	for _, arg := range out {
		key := strings.SplitN(arg, "=", 2)[0]
		switch key {
		case "-f", "--f", "-o", "--o", "-h", "--help", "-help", "-v", "--version", "--":
			return nil, ErrState
		}
	}
	return out, nil
}
