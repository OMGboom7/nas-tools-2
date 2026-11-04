package cookiecloud

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const Plugin = "CookieCloud"
const ConfigKey = "plugin." + Plugin

var (
	ErrConfig = errors.New("CookieCloud 配置无效")
	ErrScope  = errors.New("原 CookieCloud 设置或站点范围已变化")
	ErrCloud  = errors.New("CookieCloud 未返回可用数据，请核对服务器和凭据")
	ErrBusy   = errors.New("原 CookieCloud 同步仍在执行")
)

type Settings struct {
	Server    string `json:"server"`
	Key       string `json:"key"`
	Password  string `json:"password"`
	Cron      string `json:"cron"`
	Mode      string `json:"synchronousMode"`
	BlackList string `json:"black_list"`
	WhiteList string `json:"white_list"`
	Notify    bool   `json:"notify"`
	OnlyOnce  bool   `json:"onlyonce"`
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func (s Settings) Ready() bool {
	return s.Server != "" && s.Key != "" && s.Password != ""
}

func Parse(raw string) (Settings, error) {
	s := Settings{Mode: "all_mode"}
	if raw != "" && (len(raw) > 128<<10 || json.Unmarshal([]byte(raw), &s) != nil) {
		return Settings{}, ErrConfig
	}
	s.Server = strings.TrimSpace(s.Server)
	s.Cron = strings.TrimSpace(s.Cron)
	if s.Mode == "" {
		s.Mode = "all_mode"
	}
	if s.Mode != "all_mode" && s.Mode != "black_mode" && s.Mode != "white_mode" {
		return Settings{}, ErrConfig
	}
	for _, field := range []struct {
		value string
		limit int
	}{
		{s.Server, 4096}, {s.Password, 4096}, {s.Cron, 512}, {s.BlackList, 32000}, {s.WhiteList, 32000},
	} {
		if len(field.value) > field.limit || !utf8.ValidString(field.value) || strings.ContainsRune(field.value, 0) {
			return Settings{}, ErrConfig
		}
	}
	if s.Key != "" && !keyPattern.MatchString(s.Key) {
		return Settings{}, ErrConfig
	}
	if s.Server != "" {
		u, err := serverURL(s.Server)
		if err != nil {
			return Settings{}, err
		}
		s.Server = u.String()
	}
	return s, nil
}

func serverURL(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(u.Path, "\\\r\n\x00") || u.RawPath != "" {
		return nil, ErrConfig
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, ErrConfig
		}
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func Merge(raw string, changes map[string]json.RawMessage) (string, error) {
	values := map[string]json.RawMessage{}
	if raw != "" && (len(raw) > 128<<10 || json.Unmarshal([]byte(raw), &values) != nil) {
		return "", ErrConfig
	}
	if values == nil {
		values = map[string]json.RawMessage{}
	}
	for key, value := range changes {
		switch key {
		case "server", "key", "password", "cron", "synchronousMode", "black_list", "white_list":
			var v string
			if len(value) == 0 || string(value) == "null" || json.Unmarshal(value, &v) != nil {
				return "", ErrConfig
			}
		case "notify", "onlyonce":
			var v bool
			if len(value) == 0 || string(value) == "null" || json.Unmarshal(value, &v) != nil {
				return "", ErrConfig
			}
		default:
			return "", ErrConfig
		}
		values[key] = value
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", ErrConfig
	}
	if _, err = Parse(string(encoded)); err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (s Settings) Receiver() string {
	if s.Server == "" {
		return ""
	}
	return s.Server + "/get/{标识}"
}
