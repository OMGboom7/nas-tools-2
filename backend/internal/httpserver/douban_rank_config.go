package httpserver

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

const rankSettingsKey = "plugin." + douban.RankPlugin
const rankControlKey = "GoDoubanRank"

var rankKeys = []string{"UserInstalledPlugins", rankSettingsKey, rankControlKey, "DefaultRssSettingMOV", "DefaultRssSettingTV"}

func rankDetail(_ context.Context, actor string, raw map[string]string) (pluginConfigDetail, error) {
	cfg, err := douban.ReadRankSettings(raw[rankSettingsKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	choices := make([]pluginConfigChoice, 0, len(douban.RankSources))
	for _, rank := range douban.RankSources {
		choices = append(choices, pluginConfigChoice{rank.ID, rank.Name})
	}
	fields := []pluginConfigField{
		{Key: "enable", Title: "启用定时订阅", Type: "switch"},
		{Key: "onlyonce", Title: "保存后刷新一次", Type: "switch"},
		{Key: "cron", Title: "刷新周期", Type: "text", Placeholder: "0 8 * * *"},
		{Key: "release_date", Title: "最低上映年份", Type: "text", Placeholder: "0 表示不限"},
		{Key: "vote", Title: "最低 TMDB 评分", Type: "text", Placeholder: "0 表示不限"},
		{Key: "rss_addrs", Title: "自定义 RSSHub 地址", Type: "textarea", Placeholder: "每行一个地址"},
		{Key: "ranks", Title: "内置榜单", Type: "multiselect", Options: choices},
	}
	for i := range fields {
		if fields[i].Options == nil {
			fields[i].Options = []pluginConfigChoice{}
		}
	}
	return pluginConfigDetail{ID: douban.RankPlugin, Name: "豆瓣榜单订阅", Fields: fields, Values: map[string]any{"enable": cfg.Enabled, "onlyonce": cfg.Once, "cron": cfg.Cron, "release_date": strconv.Itoa(cfg.Year), "vote": strconv.FormatFloat(cfg.Vote, 'f', -1, 64), "ranks": append([]string{}, cfg.Ranks...), "rss_addrs": strings.Join(cfg.Addresses, "\n")}, Meta: pluginConfigDetailMeta{Fingerprint: sitelogin.Hash([]any{actor, raw}), HasPage: true}}, nil
}
func (api *doubanSyncAPI) scheduleScope(actor, settings string) (string, error) {
	auth, cfg, err := api.login.authority(actor)
	if err != nil {
		return "", err
	}
	app := objectValue(cfg["app"])
	media := objectValue(cfg["media"])
	loc, err := rssWorkerLocation()
	if err != nil {
		return "", err
	}
	return sitelogin.Hash([]any{auth, settings, app["tmdb_domain"], app["rmt_tmdbkey"], media, cfg[strings.ToLower(strings.TrimSpace(text(media["media_server"])))], loc.String()}), nil
}
func (api *doubanSyncAPI) rankSummary(_ context.Context, raw map[string]string) (bool, bool) {
	return api.scheduleSummary(raw[rankControlKey], raw[rankSettingsKey])
}
func (api *doubanSyncAPI) scheduleSummary(control, settings string) (bool, bool) {
	c, err := doubanState(control)
	if err != nil {
		return false, false
	}
	if c.Actor == "" || !c.Running && c.Once.IsZero() && c.Next.IsZero() {
		return false, true
	}
	scope, err := api.scheduleScope(c.Actor, settings)
	return err == nil && scope == c.Scope, err == nil
}
func (api *doubanSyncAPI) rankChanges(_ context.Context, actor string, raw map[string]string, in map[string]json.RawMessage, clear []string, now time.Time) (map[string]string, error) {
	values := map[string]json.RawMessage{}
	if old := raw[rankSettingsKey]; old != "" && (json.Unmarshal([]byte(old), &values) != nil || values == nil) {
		return nil, douban.ErrSettings
	}
	if len(clear) != 0 {
		return nil, douban.ErrSettings
	}
	for k, v := range in {
		switch k {
		case "enable", "onlyonce", "cron", "rss_addrs", "vote", "release_date", "ranks":
			values[k] = v
		default:
			return nil, douban.ErrSettings
		}
	}
	b, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	cfg, err := douban.ReadRankSettings(string(b))
	if err != nil {
		return nil, err
	}
	if (cfg.Enabled || cfg.Once) && len(cfg.URLs()) == 0 {
		return nil, douban.ErrSettings
	}
	return api.scheduleChanges(actor, rankSettingsKey, rankControlKey, values, cfg.Enabled, cfg.Once, cfg.Cron, now)
}
func (api *doubanSyncAPI) scheduleChanges(actor, settingsKey, controlKey string, values map[string]json.RawMessage, enabled, once bool, cron string, now time.Time) (map[string]string, error) {
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, err
	}
	next, err := cookieCloudSchedule(cron, loc, now)
	if err != nil {
		return nil, err
	}
	c := doubanControl{}
	if enabled || once {
		if enabled && !once && next.IsZero() {
			return nil, douban.ErrSettings
		}
		if enabled {
			c.Next = next.UTC()
		}
		if once {
			c.Once = now.Add(3 * time.Second).UTC()
			values["onlyonce"] = json.RawMessage("false")
		}
		c.Actor = actor
	}
	b, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	if c.Actor != "" {
		c.Scope, err = api.scheduleScope(actor, string(b))
		if err != nil {
			return nil, err
		}
	}
	state, err := json.Marshal(c)
	return map[string]string{settingsKey: string(b), controlKey: string(state)}, err
}
