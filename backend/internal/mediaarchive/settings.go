package mediaarchive

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

var ErrSettings = errors.New("invalid media archive settings")

type Settings struct {
	Enabled bool   `json:"enable"`
	Notify  bool   `json:"enable_notify"`
	Once    bool   `json:"run_once"`
	Cron    string `json:"cron"`
	Path    string `json:"archive_path"`
	Count   string `json:"-"`
}

func Parse(raw string) (Settings, int, error) {
	var cfg Settings
	if raw == "" {
		return cfg, 10, nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || fields == nil || json.Unmarshal([]byte(raw), &cfg) != nil {
		return cfg, 0, ErrSettings
	}
	if strings.ContainsRune(cfg.Path, 0) {
		return cfg, 0, ErrSettings
	}
	count := 10
	if v := fields["max_count"]; len(v) > 0 && string(v) != "null" {
		if json.Unmarshal(v, &cfg.Count) != nil {
			var n int
			if json.Unmarshal(v, &n) != nil {
				return cfg, 0, ErrSettings
			}
			// In the original plugin numeric zero is falsy; string "0" is explicit.
			if n != 0 {
				cfg.Count = strconv.Itoa(n)
			}
		}
		if cfg.Count != "" {
			n, err := strconv.Atoi(strings.TrimSpace(cfg.Count))
			if err != nil {
				return cfg, 0, ErrSettings
			}
			count = max(n, 0)
		}
	}
	return cfg, count, nil
}
