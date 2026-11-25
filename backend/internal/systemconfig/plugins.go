package systemconfig

import (
	"encoding/json"
	"errors"
	"regexp"
)

var PluginIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,79}$`)

var ErrInstalledPluginState = errors.New("installed plugin state is invalid")

func DecodeInstalledPlugins(raw string) ([]string, error) {
	installed := []string{}
	if raw == "" {
		return installed, nil
	}
	if len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &installed) != nil || len(installed) > 1024 {
		return nil, ErrInstalledPluginState
	}
	for _, id := range installed {
		if !PluginIDPattern.MatchString(id) {
			return nil, ErrInstalledPluginState
		}
	}
	return installed, nil
}
