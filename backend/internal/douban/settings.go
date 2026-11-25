package douban

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const SyncPlugin = "DoubanSync"

var ErrSettings = errors.New("invalid Douban synchronization settings")

type Settings struct {
	InterestList
	Enabled, Once, Search, Subscribe, Remote bool
	Hours, Seconds                           int
}

func ReadSettings(raw string) (Settings, error) {
	var out Settings
	if raw == "" {
		return out, nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		return out, ErrSettings
	}
	for key, dst := range map[string]*bool{"go_remote": &out.Remote, "enable": &out.Enabled, "onlyonce": &out.Once, "auto_search": &out.Search, "auto_rss": &out.Subscribe} {
		if v := m[key]; v != nil && json.Unmarshal(v, dst) != nil {
			return out, ErrSettings
		}
	}
	for key, dst := range map[string]*[]string{"types": &out.Types, "users": &out.Users} {
		if v := m[key]; v != nil && json.Unmarshal(v, dst) != nil {
			var list string
			if json.Unmarshal(v, &list) != nil {
				return out, ErrSettings
			}
			*dst = strings.Split(list, ",")
		}
		var clean []string
		for _, s := range *dst {
			if s = strings.TrimSpace(s); s != "" {
				clean = append(clean, s)
			}
		}
		*dst = clean
	}
	var mode int
	for key, dst := range map[string]*int{"days": &out.Days, "interval": &out.Hours, "rss_interval": &out.Seconds, "sync_type": &mode} {
		v := m[key]
		if v == nil || string(v) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(v, &text) != nil {
			text = string(v)
		}
		if text == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || n < 0 {
			return out, ErrSettings
		}
		*dst = n
	}
	if mode > 1 || out.Hours > int((1<<63-1)/int64(time.Hour)) || int64(out.Seconds) > (1<<63-1)/int64(time.Second) {
		return out, ErrSettings
	}
	out.Recent = mode == 1
	if v := m["cookie"]; v != nil && json.Unmarshal(v, &out.Cookie) != nil {
		return out, ErrSettings
	}
	return out, nil
}

func (s Settings) Interval() time.Duration {
	if s.Recent {
		if s.Seconds == 0 {
			return 0
		}
		return time.Duration(max(300, s.Seconds)) * time.Second
	}
	return time.Duration(s.Hours) * time.Hour
}
