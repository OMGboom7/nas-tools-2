package httpserver

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

const mteamFeedID = "MteamRssGen"
const mteamFeedKey = "plugin." + mteamFeedID

var mteamFeedKeys = []string{"UserInstalledPlugins", mteamFeedKey}
var mteamModes = []pluginConfigChoice{{"adult", "成人"}, {"normal", "综合"}, {"movie", "电影"}, {"tvshow", "剧集"}}

type mteamFeedSettings struct {
	enabled    bool
	minimumGiB int64
	modes      []string
}

func parseMTeamFeed(values map[string]json.RawMessage) (mteamFeedSettings, error) {
	var s mteamFeedSettings
	if v := values["enabled"]; v != nil && json.Unmarshal(v, &s.enabled) != nil {
		return s, errMetadataConfig
	}
	if v := values["torrent_min_size"]; len(v) > 0 && string(v) != "null" {
		n := string(v)
		if v[0] == '"' && json.Unmarshal(v, &n) != nil {
			return s, errMetadataConfig
		}
		if n = strings.TrimSpace(n); n != "" {
			i, err := strconv.ParseInt(n, 10, 64)
			if err != nil || i < 0 || i > math.MaxInt64/(1<<30) {
				return s, errMetadataConfig
			}
			s.minimumGiB = i
		}
	}
	if v := values["watch_categories"]; v != nil && json.Unmarshal(v, &s.modes) != nil {
		var single string
		if json.Unmarshal(v, &single) != nil {
			return s, errMetadataConfig
		}
		if single != "" {
			s.modes = []string{single}
		}
	}
	if len(s.modes) == 0 {
		s.modes = []string{"adult"}
	}
	var modes []string
	for _, mode := range s.modes {
		if !slices.ContainsFunc(mteamModes, func(v pluginConfigChoice) bool { return v.Value == mode }) {
			return s, errMetadataConfig
		}
		if !slices.Contains(modes, mode) {
			modes = append(modes, mode)
		}
	}
	s.modes = modes
	return s, nil
}

func mteamStored(raw string) (map[string]json.RawMessage, error) {
	v := map[string]json.RawMessage{}
	if raw != "" && (len(raw) > 64<<10 || json.Unmarshal([]byte(raw), &v) != nil || v == nil) {
		return nil, errStoredMetadataConfig
	}
	return v, nil
}
func readMTeamFeed(raw string) (mteamFeedSettings, error) {
	v, err := mteamStored(raw)
	if err != nil {
		return mteamFeedSettings{}, err
	}
	return parseMTeamFeed(v)
}

func (feed *mteamFeedService) plugin() feedPluginDefinition {
	return feedPluginDefinition{id: mteamFeedID, name: "馒头大包监控", keys: mteamFeedKeys, summary: feed.summary, detail: feed.detail, changes: feed.changes}
}
func (feed *mteamFeedService) detail(_ context.Context, actor string, raw map[string]string) (pluginConfigDetail, error) {
	s, err := readMTeamFeed(raw[mteamFeedKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	fields := []pluginConfigField{
		{Key: "enabled", Title: "启用 RSS 服务", Type: "switch", Options: []pluginConfigChoice{}},
		{Key: "torrent_min_size", Title: "种子体积最小值（GiB）", Type: "text", Placeholder: "0", Tooltip: "仅收录大于此大小的免费或限时免费种子。", Options: []pluginConfigChoice{}},
		{Key: "watch_categories", Title: "监控类别", Type: "multiselect", Options: mteamModes},
	}
	return pluginConfigDetail{ID: mteamFeedID, Name: "馒头大包监控", Fields: fields, Values: map[string]any{"enabled": s.enabled, "torrent_min_size": strconv.FormatInt(s.minimumGiB, 10), "watch_categories": s.modes}, Meta: pluginConfigDetailMeta{Fingerprint: sitelogin.Hash([]any{actor, raw})}}, nil
}
func (feed *mteamFeedService) changes(_ context.Context, _ string, raw map[string]string, in map[string]json.RawMessage, clear []string, _ time.Time) (map[string]string, error) {
	v, err := mteamStored(raw[mteamFeedKey])
	if err != nil || len(clear) > 0 {
		return nil, errMetadataConfig
	}
	for k, b := range in {
		switch k {
		case "enabled", "torrent_min_size", "watch_categories":
			v[k] = b
		default:
			return nil, errMetadataConfig
		}
	}
	settings, err := parseMTeamFeed(v)
	if err != nil {
		return nil, err
	}
	// Old configurations with an empty selection defaulted to adult. When
	// enabling from the editor, use the categories the administrator selected.
	if selected, ok := in["watch_categories"]; ok && settings.enabled {
		var modes []string
		if json.Unmarshal(selected, &modes) == nil && len(modes) == 0 || string(selected) == `""` {
			return nil, errMetadataConfig
		}
	}
	b, err := json.Marshal(v)
	return map[string]string{mteamFeedKey: string(b)}, err
}
func (feed *mteamFeedService) summary(_ context.Context, raw map[string]string) (bool, bool) {
	s, err := readMTeamFeed(raw[mteamFeedKey])
	if err != nil {
		return false, false
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	return s.enabled && feed.server != nil, true
}
