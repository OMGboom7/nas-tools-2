package httpserver

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

const movieRandomID = "MovieRandom"
const movieRandomSettingsKey = "plugin." + movieRandomID
const movieRandomControlKey = "GoMovieRandom"

var movieRandomKeys = []string{"UserInstalledPlugins", movieRandomSettingsKey, movieRandomControlKey, "DefaultRssSettingMOV"}

// Legacy settings store the displayed Chinese name. TMDB expects the value.
var randomGenres = []pluginConfigChoice{{"", "全部"}, {"12", "冒险"}, {"16", "动画"}, {"35", "喜剧"}, {"80", "犯罪"}, {"18", "剧情"}, {"14", "奇幻"}, {"27", "恐怖"}, {"9648", "悬疑"}, {"10749", "爱情"}, {"878", "科幻"}, {"53", "惊悚"}, {"10752", "战争"}}
var randomLanguages = []pluginConfigChoice{{"", "全部"}, {"zh", "中文"}, {"en", "英语"}, {"ja", "日语"}, {"ko", "韩语"}, {"fr", "法语"}, {"de", "德语"}, {"ru", "俄语"}, {"hi", "印地语"}}

type movieRandomSettings struct {
	Enabled, Once         bool
	Cron, Genre, Language string
	Year, Vote            string
}

func decodeRandomSettings(values map[string]json.RawMessage) (movieRandomSettings, map[string]string, error) {
	var cfg movieRandomSettings
	filters := map[string]string{}
	for key, dst := range map[string]any{"enable": &cfg.Enabled, "onlyonce": &cfg.Once, "cron": &cfg.Cron, "genres": &cfg.Genre, "language": &cfg.Language} {
		if b := values[key]; b != nil && json.Unmarshal(b, dst) != nil {
			return cfg, nil, douban.ErrSettings
		}
	}
	for _, field := range []struct {
		key string
		dst *string
	}{{"date", &cfg.Year}, {"vote", &cfg.Vote}} {
		b := values[field.key]
		if len(b) == 0 || string(b) == "null" {
			continue
		}
		s := string(b)
		if b[0] == '"' && json.Unmarshal(b, &s) != nil {
			return cfg, nil, douban.ErrSettings
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if field.key == "date" {
			n, err := strconv.Atoi(s)
			if err != nil || n != 0 && (n < 1000 || n > 9999) {
				return cfg, nil, douban.ErrSettings
			}
			if n > 0 {
				filters["primary_release_date.gte"] = strconv.Itoa(n) + "-01-01"
			}
		} else {
			n, err := strconv.ParseFloat(s, 64)
			if err != nil || math.IsNaN(n) || n < 0 || n > 10 {
				return cfg, nil, douban.ErrSettings
			}
			if n > 0 {
				filters["vote_average.gte"] = strconv.FormatFloat(n, 'f', -1, 64)
			}
		}
		*field.dst = s
	}
	for _, field := range []struct {
		key     string
		name    *string
		options []pluginConfigChoice
	}{
		{"with_genres", &cfg.Genre, randomGenres}, {"with_original_language", &cfg.Language, randomLanguages},
	} {
		if *field.name == "" {
			*field.name = "全部"
		}
		found := false
		for _, choice := range field.options {
			if choice.Label == *field.name {
				found = true
				if choice.Value != "" {
					filters[field.key] = choice.Value
				}
				break
			}
		}
		if !found {
			return cfg, nil, douban.ErrSettings
		}
	}
	return cfg, filters, nil
}

func movieRandomValues(raw string) (map[string]json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	if raw != "" && (json.Unmarshal([]byte(raw), &values) != nil || values == nil) {
		return nil, douban.ErrSettings
	}
	return values, nil
}
func movieRandomDetail(actor string, raw map[string]string) (pluginConfigDetail, error) {
	values, err := movieRandomValues(raw[movieRandomSettingsKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	cfg, _, err := decodeRandomSettings(values)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	choices := func(opts []pluginConfigChoice) []pluginConfigChoice {
		out := make([]pluginConfigChoice, 0, len(opts))
		for _, o := range opts {
			out = append(out, pluginConfigChoice{o.Label, o.Label})
		}
		return out
	}
	fields := []pluginConfigField{
		{Key: "enable", Title: "启用定时订阅", Type: "switch"},
		{Key: "onlyonce", Title: "保存后随机订阅一次", Type: "switch"},
		{Key: "cron", Title: "随机周期", Type: "text", Placeholder: "0 8 * * *"},
		{Key: "date", Title: "最低上映年份", Type: "text"},
		{Key: "vote", Title: "最低 TMDB 评分", Type: "text"},
		{Key: "genres", Title: "电影类型", Type: "select", Options: choices(randomGenres)},
		{Key: "language", Title: "电影语言", Type: "select", Options: choices(randomLanguages)},
	}
	for i := range fields {
		if fields[i].Options == nil {
			fields[i].Options = []pluginConfigChoice{}
		}
	}
	return pluginConfigDetail{ID: movieRandomID, Name: "电影随机订阅", Fields: fields, Values: map[string]any{"enable": cfg.Enabled, "onlyonce": cfg.Once, "cron": cfg.Cron, "date": cfg.Year, "vote": cfg.Vote, "genres": cfg.Genre, "language": cfg.Language}, Meta: pluginConfigDetailMeta{Fingerprint: sitelogin.Hash([]any{actor, raw}), HasPage: true}}, nil
}
func (api *doubanSyncAPI) movieRandomChanges(_ context.Context, actor string, raw map[string]string, in map[string]json.RawMessage, clear []string, now time.Time) (map[string]string, error) {
	values, err := movieRandomValues(raw[movieRandomSettingsKey])
	if err != nil || len(clear) != 0 {
		return nil, douban.ErrSettings
	}
	for k, v := range in {
		switch k {
		case "enable", "onlyonce", "cron", "date", "vote", "genres", "language":
			values[k] = v
		default:
			return nil, douban.ErrSettings
		}
	}
	cfg, _, err := decodeRandomSettings(values)
	if err != nil {
		return nil, err
	}
	return api.scheduleChanges(actor, movieRandomSettingsKey, movieRandomControlKey, values, cfg.Enabled, cfg.Once, cfg.Cron, now)
}
func (api *doubanSyncAPI) movieRandomSummary(_ context.Context, raw map[string]string) (bool, bool) {
	return api.scheduleSummary(raw[movieRandomControlKey], raw[movieRandomSettingsKey])
}
