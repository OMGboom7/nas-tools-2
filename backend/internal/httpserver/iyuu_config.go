package httpserver

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/iyuu"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

const iyuuGeneration = "go.iyuu.generation"

const iyuuControlKey = "GoIYUUAutoSeed"

var iyuuKeys = []string{"UserInstalledPlugins", iyuu.ConfigKey, iyuuGeneration, iyuuControlKey}

type iyuuAPI struct {
	downloads *downloadService
	transfer  *torrentTransferAPI
	transport http.RoundTripper
}

func (api *iyuuAPI) plugin() feedPluginDefinition {
	return feedPluginDefinition{id: iyuu.Plugin, name: "IYUU 自动辅种", control: iyuuControlKey, keys: iyuuKeys, detail: api.detail, summary: api.scheduleSummary, changes: api.changes}
}

func (api *iyuuAPI) choices(ctx context.Context) ([]pluginConfigChoice, []pluginConfigChoice, error) {
	ds, err := api.transfer.downloaders.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	sites, err := api.transfer.login.sites.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	d, s := []pluginConfigChoice{}, []pluginConfigChoice{}
	for _, v := range ds {
		if v.Enabled == 1 && (v.Type == "transmission" || v.Type == "qbittorrent") {
			d = append(d, pluginConfigChoice{strconv.FormatInt(v.ID, 10), v.Name})
		}
	}
	for _, v := range sites {
		s = append(s, pluginConfigChoice{strconv.FormatInt(v.ID, 10), v.Name})
	}
	return d, s, nil
}
func (api *iyuuAPI) detail(ctx context.Context, actor string, raw map[string]string) (pluginConfigDetail, error) {
	c, err := iyuu.Read(raw[iyuu.ConfigKey])
	if err != nil {
		return pluginConfigDetail{}, err
	}
	ds, ss, err := api.choices(ctx)
	if err != nil {
		return pluginConfigDetail{}, err
	}
	fields := []pluginConfigField{
		{Key: "enable", Title: "启用自动辅种", Type: "switch"},
		{Key: "cron", Title: "辅种周期", Type: "text", Placeholder: "0 3 * * *"},
		{Key: "onlyonce", Title: "保存后运行一次", Type: "switch"},
		{Key: "notify", Title: "发送结果通知", Type: "switch"},
		{Key: "clearcache", Title: "清除成功与临时失败缓存", Type: "switch"},
		{Key: "token", Title: "IYUU Token", Type: "password", WriteOnly: true, Configured: c.Token != "", Options: []pluginConfigChoice{}},
		{Key: "downloaders", Title: "辅种下载器", Type: "multiselect", Options: ds},
		{Key: "sites", Title: "辅种站点", Type: "multiselect", Options: ss, Tooltip: "留空时使用全部已配置的匹配站点。"},
		{Key: "nolabels", Title: "排除标签", Type: "text", Placeholder: "多个标签用英文逗号分隔", Options: []pluginConfigChoice{}},
	}
	for i := range fields {
		if fields[i].Options == nil {
			fields[i].Options = []pluginConfigChoice{}
		}
	}
	return pluginConfigDetail{ID: iyuu.Plugin, Name: "IYUU 自动辅种", Fields: fields, Values: map[string]any{"enable": c.Enabled, "cron": c.Cron, "notify": c.Notify, "clearcache": c.Clear, "onlyonce": c.Once, "downloaders": append([]string{}, c.Downloaders...), "sites": append([]string{}, c.Sites...), "nolabels": strings.Join(c.Excluded, ",")}, Meta: pluginConfigDetailMeta{Fingerprint: sitelogin.Hash([]any{actor, raw}), HasPage: true}}, nil
}
func (api *iyuuAPI) changes(ctx context.Context, actor string, raw map[string]string, in map[string]json.RawMessage, clear []string, now time.Time) (map[string]string, error) {
	v, err := iyuu.Values(raw[iyuu.ConfigKey])
	if err != nil {
		return nil, err
	}
	if len(clear) > 0 {
		if len(clear) != 1 || clear[0] != "token" || in["token"] != nil {
			return nil, iyuu.ErrInput
		}
		delete(v, "token")
	}
	for k, b := range in {
		switch k {
		case "token":
			var token string
			if json.Unmarshal(b, &token) != nil || !iyuu.ValidToken(token) {
				return nil, iyuu.ErrInput
			}
		case "downloaders", "sites", "nolabels", "enable", "cron", "notify", "onlyonce", "clearcache":
		default:
			return nil, iyuu.ErrInput
		}
		v[k] = b
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	c, err := iyuu.Read(string(b))
	if err != nil {
		return nil, err
	}
	ds, ss, err := api.choices(ctx)
	if err != nil {
		return nil, err
	}
	for _, group := range []struct {
		key     string
		ids     []string
		choices []pluginConfigChoice
	}{{"downloaders", c.Downloaders, ds}, {"sites", c.Sites, ss}} {
		if in[group.key] == nil {
			continue
		}
		for _, id := range group.ids {
			if !slices.ContainsFunc(group.choices, func(v pluginConfigChoice) bool { return v.Value == id }) {
				return nil, iyuu.ErrInput
			}
		}
	}
	return api.configureSchedule(ctx, actor, raw, v, c, now)
}

type iyuuScope struct {
	actor, proof, authority string
	raw                     map[string]string
	cfg                     iyuu.Settings
	client                  iyuu.Client
}

func (api *iyuuAPI) scope(ctx context.Context, actor string) (iyuuScope, error) {
	var s iyuuScope
	auth, _, err := api.transfer.login.authority(actor)
	if err != nil {
		return s, err
	}
	raw, err := api.transfer.system.ReadMany(ctx, iyuuKeys)
	if err != nil {
		return s, err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], iyuu.Plugin)
	if err != nil || !installed {
		return s, iyuu.ErrInput
	}
	cfg, err := iyuu.Read(raw[iyuu.ConfigKey])
	if err != nil || cfg.Token == "" {
		return s, iyuu.ErrInput
	}
	return iyuuScope{actor: actor, authority: auth, proof: sitelogin.Hash([]any{actor, auth, raw}), raw: raw, cfg: cfg, client: iyuu.Client{Token: cfg.Token, Transport: api.transport}}, nil
}
func (api *iyuuAPI) current(ctx context.Context, s iyuuScope) error {
	n, err := api.scope(ctx, s.actor)
	if err != nil || n.proof != s.proof || !maps.Equal(n.raw, s.raw) {
		return iyuu.ErrInput
	}
	return ctx.Err()
}
