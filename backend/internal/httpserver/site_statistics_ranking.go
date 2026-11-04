package httpserver

import (
	"bytes"
	"cmp"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

func (api *siteStatisticsAPI) readStatsSites(ctx context.Context) ([]siteconfig.Site, error) {
	sites, err := api.login.sites.List(ctx)
	if err != nil {
		return nil, err
	}
	eligible := make([]siteconfig.Site, 0, len(sites))
	for _, site := range sites {
		record, err := nativeSiteRecord(site)
		if err != nil {
			return nil, err
		}
		if truthy(record["statistic_enable"]) && text(record["strict_url"]) != "" {
			eligible = append(eligible, site)
		}
	}
	return eligible, nil
}

func statsAddressByName(sites []siteconfig.Site, name string) string {
	address := ""
	for _, site := range sites {
		if site.Name != name {
			continue
		}
		origin := safeSiteOrigin(defaultString(site.SignURL, site.RSSURL))
		if address != "" && address != origin {
			return ""
		}
		address = origin
	}
	return address
}

func (api *siteStatisticsAPI) rankingCatalog() (indexercatalog.Catalog, error) {
	if api.login.catalogPath == "" {
		return indexercatalog.Catalog{}, nil
	}
	return indexercatalog.Load(api.login.catalogPath)
}

func (api *siteStatisticsAPI) rankAt(ctx context.Context, address, name string) (sitestats.RankStatus, error) {
	item, err := api.store.Snapshot(ctx, address)
	if err != nil {
		return sitestats.RankStatus{}, err
	}
	catalog, err := api.rankingCatalog()
	if err != nil {
		return sitestats.RankStatus{}, err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return sitestats.RankStatus{}, err
	}
	item.Site = name
	return sitestats.Rank(item, catalog.RankingRules(name, address), time.Now().In(loc))
}

func (api *siteStatisticsAPI) statsOverview(ctx context.Context, sites []siteconfig.Site) (statsOverview, error) {
	items, err := api.store.List(ctx, statsURLs(sites))
	if err != nil {
		return statsOverview{}, err
	}
	for i := range items {
		items[i].Level = sitestats.NormalizeLevel(items[i].Level)
	}
	targets := make([]statsTarget, 0, len(sites))
	for _, site := range sites {
		v, err := nativeSiteRecord(site)
		if err != nil {
			return statsOverview{}, err
		}
		address := text(v["strict_url"])
		targets = append(targets, statsTarget{strconv.FormatInt(site.ID, 10), site.Name, safeSiteHost(address), address, truthy(v["unread_msg_notify"])})
	}
	return statsOverview{Targets: targets, Items: items}, nil
}

func (api *siteStatisticsAPI) serveStatsRead(w http.ResponseWriter, r *http.Request) {
	if !api.readyForStatsRead(w) || !api.authorizeStatsRead(w, r) {
		return
	}
	ctx := r.Context()
	sites, err := api.readStatsSites(ctx)
	if err != nil {
		statsError(w, err)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		overview, err := api.statsOverview(ctx, sites)
		if err != nil {
			statsError(w, err)
			return
		}
		// Omit admin scheduling settings and refresh receipts from the viewer endpoint.
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": overview})
		return
	}
	for _, site := range sites {
		if strconv.FormatInt(site.ID, 10) != id {
			continue
		}
		address := safeSiteOrigin(defaultString(site.SignURL, site.RSSURL))
		if strings.HasPrefix(r.URL.Path, "/api/v1/site-statistics/detail/") {
			history, err := api.store.History(ctx, address, 730)
			if err != nil {
				statsError(w, err)
				return
			}
			seeds, err := api.store.Seeds(ctx, address)
			if err != nil {
				statsError(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"siteId": id, "history": history, "seedingInfo": sitestats.SeedStrings(seeds)}})
			return
		}
		rank, err := api.rankAt(ctx, address, site.Name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeAPIError(w, 404, 404, "该站点尚无统计快照")
		case err != nil:
			writeAPIError(w, 503, 503, "站点等级规则或统计快照不可用")
		default:
			writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"siteId": id, "ranking": rank}})
		}
		return
	}
	writeAPIError(w, 404, 404, "未找到参与统计的站点")
}

func (api *siteStatisticsAPI) statsSiteDataset(ctx context.Context, action, address string) ([][]any, error) {
	if action == "seedinfo" {
		rows, err := api.store.Seeds(ctx, address)
		if err != nil {
			return nil, err
		}
		result := [][]any{{"seeders", "size"}}
		for _, v := range rows {
			result = append(result, []any{v[0], v[1]})
		}
		return result, nil
	}
	rows, err := api.store.History(ctx, address, 730)
	if err != nil {
		return nil, err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, err
	}
	result := [][]any{{"time", "upload", "download", "bonus", "seeding", "seeding_size"}}
	for _, v := range rows {
		if day, err := time.ParseInLocation("2006-01-02", v.Date, loc); err == nil {
			result = append(result, []any{day.UnixMilli(), v.Upload, v.Download, v.Bonus, v.Seeding, v.SeedingSize})
		}
	}
	return result, nil
}

func (api *siteStatisticsAPI) statsHistoryDataset(ctx context.Context, sites []siteconfig.Site, days int, endDay string) ([][]any, error) {
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, err
	}
	end := time.Now().In(loc)
	if endDay != "" {
		end, err = time.ParseInLocation("2006-01-02", endDay, loc)
		if err != nil {
			return nil, err
		}
	}
	rows, err := api.store.Deltas(ctx, statsURLs(sites), days+1, end)
	if err != nil {
		return nil, err
	}
	result := [][]any{{"site", "upload", "download"}}
	labels := make(map[string]int, len(rows))
	for _, v := range rows {
		labels[v.Site]++
	}
	for _, v := range rows {
		label := v.Site
		if labels[label] > 1 {
			label += " (" + v.URL + ")"
		}
		result = append(result, []any{label, v.Upload, v.Download})
	}
	return result, nil
}

type legacyStatsInput struct {
	Site     string   `json:"site"`
	Name     string   `json:"name"`
	Sites    []string `json:"sites"`
	SortBy   string   `json:"sort_by"`
	SortOn   string   `json:"sort_on"`
	SiteHash string   `json:"site_hash"`
	Days     *int     `json:"days"`
	EndDay   string   `json:"end_day"`
}

func legacyStatsError(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"code": 1, "msg": message})
}

func (api *siteStatisticsAPI) legacyAction(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			next(w, r)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
		var envelope struct {
			Cmd  string          `json:"cmd"`
			Data json.RawMessage `json:"data"`
		}
		if err != nil || json.Unmarshal(raw, &envelope) != nil {
			writeAPIError(w, 400, 400, "原操作格式无效")
			return
		}
		switch envelope.Cmd {
		case "get_user_ranking_status", "get_site_user_statistics", "get_site_activity", "get_site_seeding_info", "get_site_history":
		default:
			r.Body = io.NopCloser(bytes.NewReader(raw))
			next(w, r)
			return
		}
		if !api.readyForStatsRead(w) || !api.authorizeStatsRead(w, r) {
			return
		}
		var in legacyStatsInput
		if len(envelope.Data) > 0 && json.Unmarshal(envelope.Data, &in) != nil {
			legacyStatsError(w, "查询参数错误")
			return
		}
		api.serveLegacyStats(w, r, envelope.Cmd, in)
	}
}

func (api *siteStatisticsAPI) serveLegacyStats(w http.ResponseWriter, r *http.Request, action string, in legacyStatsInput) {
	ctx := r.Context()
	sites, err := api.readStatsSites(ctx)
	if err != nil {
		legacyStatsError(w, "原站点统计范围不可用")
		return
	}
	var result any
	key := "data"
	switch action {
	case "get_user_ranking_status":
		if in.Site == "" {
			legacyStatsError(w, "未指定站点")
			return
		}
		address := statsAddressByName(sites, in.Site)
		if address == "" {
			legacyStatsError(w, "未找到唯一参与统计的站点")
			return
		}
		result, err = api.rankAt(ctx, address, in.Site)
		if errors.Is(err, sql.ErrNoRows) {
			legacyStatsError(w, "未找到站点的统计数据")
			return
		}
	case "get_site_user_statistics":
		result, err = api.legacyStatsList(ctx, sites, in)
	case "get_site_history":
		if in.Days == nil || *in.Days < 1 || *in.Days > 729 {
			legacyStatsError(w, "查询参数错误")
			return
		}
		key = "dataset"
		result, err = api.statsHistoryDataset(ctx, sites, *in.Days, in.EndDay)
	case "get_site_activity", "get_site_seeding_info":
		address := statsAddressByName(sites, in.Name)
		if address == "" {
			legacyStatsError(w, "未找到唯一参与统计的站点")
			return
		}
		dataset := "activity"
		if action == "get_site_seeding_info" {
			dataset = "seedinfo"
		}
		key = "dataset"
		result, err = api.statsSiteDataset(ctx, dataset, address)
	}
	if err != nil {
		legacyStatsError(w, "原统计或站点等级规则不可用")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, key: result})
}

func (api *siteStatisticsAPI) legacyStatsList(ctx context.Context, sites []siteconfig.Site, in legacyStatsInput) ([]map[string]any, error) {
	byAddress := make(map[string]siteconfig.Site, len(sites))
	urls := []string{}
	for _, site := range sites {
		if len(in.Sites) > 0 && !slices.Contains(in.Sites, site.Name) {
			continue
		}
		address := safeSiteOrigin(defaultString(site.SignURL, site.RSSURL))
		if _, exists := byAddress[address]; !exists {
			byAddress[address] = site
			urls = append(urls, address)
		}
	}
	out := []map[string]any{}
	if len(urls) == 0 {
		return out, nil
	}
	rows, err := api.store.List(ctx, urls)
	if err != nil {
		return nil, err
	}
	catalog, err := api.rankingCatalog()
	if err != nil {
		return nil, err
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, err
	}
	now := time.Now().In(loc)
	slices.SortStableFunc(rows, func(a, b sitestats.Snapshot) int {
		first, _ := strconv.Atoi(byAddress[a.URL].Priority)
		second, _ := strconv.Atoi(byAddress[b.URL].Priority)
		return cmp.Compare(first, second)
	})
	rows = rows[:min(len(rows), 100)]
	if in.SortOn == "asc" || in.SortOn == "desc" {
		slices.SortStableFunc(rows, func(a, b sitestats.Snapshot) int {
			if in.SortOn == "desc" {
				a, b = b, a
			}
			return compareLegacyStats(a, b, in.SortBy)
		})
	}
	for _, row := range rows {
		site := byAddress[row.URL]
		rank, err := sitestats.Rank(row, catalog.RankingRules(site.Name, row.URL), now)
		if err != nil {
			return nil, err
		}
		ageLabel := ""
		if joined, err := time.ParseInLocation(time.DateTime, row.Joined, loc); err == nil && now.Sub(joined) < 32*24*time.Hour {
			ageLabel = "新手"
		}
		item := map[string]any{
			"site": row.Site, "username": row.Username, "user_level": rank.CurrentLevel, "join_at": row.Joined,
			"last_seen": legacyStatsAgo(row.LastSeen, now), "update_at": legacyStatsAgo(row.Updated, now),
			"upload": row.Upload, "download": row.Download, "ratio": row.Ratio, "bonus": row.Bonus,
			"seeding": row.Seeding, "leeching": row.Leeching, "seeding_size": row.SeedingSize, "url": row.URL,
			"msg_unread": row.Unread, "level_description": ageLabel,
			"keep_account_level": rank.KeepAccountLevel, "is_keep_account_level": rank.CurrentIsKeep,
		}
		if in.SiteHash == "Y" {
			item["site_hash"] = ""
			if row.Site != "" {
				sum := md5.Sum([]byte(row.Site))
				item["site_hash"] = hex.EncodeToString(sum[:])
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func compareLegacyStats(a, b sitestats.Snapshot, key string) int {
	switch key {
	case "upload":
		return cmp.Compare(a.Upload, b.Upload)
	case "download":
		return cmp.Compare(a.Download, b.Download)
	case "ratio":
		return cmp.Compare(a.Ratio, b.Ratio)
	case "seeding_size":
		return cmp.Compare(a.SeedingSize, b.SeedingSize)
	case "seeding":
		return cmp.Compare(a.Seeding, b.Seeding)
	case "bonus":
		return cmp.Compare(a.Bonus, b.Bonus)
	case "site":
		return cmp.Compare(a.Site, b.Site)
	default:
		return 0
	}
}

func legacyStatsAgo(raw string, now time.Time) string {
	at, err := time.ParseInLocation(time.DateTime, raw, now.Location())
	if err != nil || at.After(now) {
		return ""
	}
	minutes := int64(now.Sub(at) / time.Minute)
	days, hours := minutes/(24*60), minutes/60%24
	minutes %= 60
	var out strings.Builder
	if days > 0 {
		out.WriteString(strconv.FormatInt(days, 10) + "天")
	}
	if hours > 0 {
		out.WriteString(strconv.FormatInt(hours, 10) + "时")
	}
	if minutes > 0 || days == 0 && hours == 0 {
		out.WriteString(strconv.FormatInt(minutes, 10) + "分")
	}
	return out.String() + "前"
}
