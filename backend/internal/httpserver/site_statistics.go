package httpserver

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

var siteClaimPattern = regexp.MustCompile(`claim\.php\?uid=(\d+)`)

func siteClaimUserID(page string) string {
	m := siteClaimPattern.FindStringSubmatch(page)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}
func decodeJSONMap(raw []byte, v *map[string]any) error {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	return d.Decode(v)
}
func statsError(w http.ResponseWriter, e error) {
	writeAPIError(w, 409, 409, "统计配置或任务状态已变化，请查看原回执或重新读取")
}

type statsTarget struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Host           string `json:"host"`
	MessageEnabled bool   `json:"messageEnabled"`
}
type statsStatus struct {
	Fingerprint string               `json:"fingerprint"`
	GoOnly      bool                 `json:"goOnly"`
	Cron        string               `json:"cron"`
	Control     sitestats.Control    `json:"control"`
	Current     bool                 `json:"current"`
	Targets     []statsTarget        `json:"targets"`
	Items       []sitestats.Snapshot `json:"items"`
	Jobs        []sitestats.Job      `json:"jobs"`
}

func statsURLs(sites []siteconfig.Site) []string {
	out := []string{}
	for _, s := range sites {
		out = append(out, safeSiteOrigin(defaultString(s.SignURL, s.RSSURL)))
	}
	return out
}
func (api *siteStatisticsAPI) manage(w http.ResponseWriter, r *http.Request) {
	if api == nil || api.store == nil || api.signin == nil || api.signin.channels == nil {
		writeAPIError(w, 503, 503, "统计服务不可用")
		return
	}
	actor, ok := api.login.actor(w, r, r.Method != "GET")
	if !ok {
		return
	}
	fp, authority, sites, cfg, e := api.snapshot(r.Context(), actor)
	if e != nil {
		statsError(w, e)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/site-statistics")
	if r.Method == "GET" && path == "" {
		api.recover(r.Context())
		c, e := api.store.Control(r.Context())
		if e != nil {
			statsError(w, e)
			return
		}
		items, e := api.store.List(r.Context(), statsURLs(sites))
		if e != nil {
			statsError(w, e)
			return
		}
		jobs, e := api.store.Jobs(r.Context())
		if e != nil {
			statsError(w, e)
			return
		}
		targets := []statsTarget{}
		for _, s := range sites {
			v, _ := nativeSiteRecord(s)
			targets = append(targets, statsTarget{strconv.FormatInt(s.ID, 10), s.Name, safeSiteHost(text(v["strict_url"])), truthy(v["unread_msg_notify"])})
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": statsStatus{fp, api.login.pureGo, text(objectValue(cfg["pt"])["ptrefresh_date_cron"]), c, c.Enabled && c.Fingerprint == fp && c.Authority == authority, targets, items, jobs}})
		return
	}
	if r.Method == "GET" && strings.HasPrefix(path, "/detail/") {
		id := strings.TrimPrefix(path, "/detail/")
		address := ""
		for _, s := range sites {
			if strconv.FormatInt(s.ID, 10) == id {
				address = safeSiteOrigin(defaultString(s.SignURL, s.RSSURL))
			}
		}
		if address == "" {
			writeAPIError(w, 404, 404, "未找到参与统计的站点")
			return
		}
		history, e := api.store.History(r.Context(), address, 730)
		if e != nil {
			statsError(w, e)
			return
		}
		seeds, e := api.store.Seeds(r.Context(), address)
		if e != nil {
			statsError(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"siteId": id, "history": history, "seedingInfo": sitestats.SeedStrings(seeds)}})
		return
	}
	if r.Method != "POST" {
		writeAPIError(w, 404, 404, "未找到统计操作")
		return
	}
	switch path {
	case "/refresh":
		var in struct {
			Fingerprint  string   `json:"fingerprint"`
			RequestID    string   `json:"requestId"`
			SiteIDs      []string `json:"siteIds"`
			ReadMessages bool     `json:"readMessages"`
			Confirm      bool     `json:"confirm"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		if !in.Confirm || in.Fingerprint != fp || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(in.RequestID) || len(in.SiteIDs) > 1000 {
			writeAPIError(w, 400, 400, "请重新读取统计范围并确认刷新")
			return
		}
		origin := "manual:" + in.RequestID
		j, e := api.launch(r.Context(), sitestats.Job{ID: sitestats.Hash([]any{actor, origin}), Origin: origin, Actor: actor, Fingerprint: fp, Authority: authority, SiteIDs: in.SiteIDs, ReadMessages: in.ReadMessages, Summary: true})
		if e != nil {
			statsError(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": j})
	case "/activate":
		var in struct {
			Fingerprint  string `json:"fingerprint"`
			Expected     string `json:"expectedEpoch"`
			ReadMessages bool   `json:"readMessages"`
			Remote       bool   `json:"remote"`
			Confirm      bool   `json:"confirm"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		if !in.Confirm || in.Fingerprint != fp || len(sites) == 0 {
			statsError(w, sitestats.ErrState)
			return
		}
		location, e := rssWorkerLocation()
		if e != nil {
			statsError(w, e)
			return
		}
		cron := text(objectValue(cfg["pt"])["ptrefresh_date_cron"])
		next := ""
		if cron != "" {
			if _, e = parseSigninSchedule(cron, location); e != nil {
				writeAPIError(w, 400, 400, "原统计定时配置无效")
				return
			}
			next = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		}
		c := sitestats.Control{Created: time.Now().UTC().Format(time.RFC3339Nano), Enabled: true, Epoch: sitestats.Hash([]any{actor, fp, time.Now().UnixNano()}), Actor: actor, Authority: authority, Fingerprint: fp, Cron: cron, Location: location.String(), ReadMessages: in.ReadMessages, Remote: in.Remote, NextRun: next}
		if e = api.store.SetControl(r.Context(), c, in.Expected); e != nil {
			statsError(w, e)
			return
		}
		api.mu.Lock()
		if api.cancel != nil {
			api.cancel()
		}
		api.mu.Unlock()
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": c})
	case "/stop":
		var in struct {
			Expected string `json:"expectedEpoch"`
			Confirm  bool   `json:"confirm"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		if !in.Confirm {
			statsError(w, sitestats.ErrState)
			return
		}
		c, e := api.store.Control(r.Context())
		if e != nil {
			statsError(w, e)
			return
		}
		c.Enabled = false
		c.NextRun = ""
		if e = api.store.SetControl(r.Context(), c, in.Expected); e != nil {
			statsError(w, e)
			return
		}
		api.mu.Lock()
		if api.cancel != nil {
			api.cancel()
		}
		api.mu.Unlock()
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": c})
	default:
		writeAPIError(w, 404, 404, "未找到统计操作")
	}
}

func (api *siteStatisticsAPI) compat(w http.ResponseWriter, r *http.Request) {
	if api == nil || api.store == nil || api.login == nil {
		writeAPIError(w, 503, 503, "统计服务不可用")
		return
	}
	if r.Method == "GET" {
		service := siteService{configStore: api.login.config}
		if !service.requireSiteAPIKey(w, r) {
			return
		}
	} else {
		claims, err := api.login.auth.service.VerifyToken(r.Header.Get("Authorization"))
		if err != nil {
			writeAPIError(w, 401, 401, "请重新登录")
			return
		}
		if _, err = api.login.auth.service.FindUser(r.Context(), claims.Username); err != nil {
			writeAPIError(w, 401, 401, "当前账户不可用")
			return
		}
		if !parseSiteForm(w, r) {
			return
		}
	}
	sites, e := api.login.sites.List(r.Context())
	if e != nil {
		statsError(w, e)
		return
	}
	eligible := []siteconfig.Site{}
	address := ""
	for _, s := range sites {
		v, e := nativeSiteRecord(s)
		if e != nil {
			statsError(w, e)
			return
		}
		if truthy(v["statistic_enable"]) {
			eligible = append(eligible, s)
			if s.Name == r.FormValue("name") {
				address = text(v["strict_url"])
			}
		}
	}
	if r.URL.Path == "/api/v1/site/statistics" {
		rows, e := api.store.List(r.Context(), statsURLs(eligible))
		if e != nil {
			statsError(w, e)
			return
		}
		out := []any{}
		for _, v := range rows {
			out = append(out, map[string]any{"site": v.Site, "username": v.Username, "user_level": v.Level, "join_at": v.Joined, "last_seen": v.LastSeen, "update_at": v.Updated, "upload": v.Upload, "download": v.Download, "ratio": v.Ratio, "seeding": v.Seeding, "leeching": v.Leeching, "seeding_size": v.SeedingSize, "bonus": v.Bonus, "url": v.URL, "message_unread": v.Unread})
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"user_statistics": out}})
		return
	}
	dataset := [][]any{}
	switch r.URL.Path {
	case "/api/v1/site/statistics/activity":
		if address == "" {
			writeAPIError(w, 404, 404, "站点不参与统计")
			return
		}
		rows, e := api.store.History(r.Context(), address, 730)
		if e != nil {
			statsError(w, e)
			return
		}
		loc, e := rssWorkerLocation()
		if e != nil {
			statsError(w, e)
			return
		}
		dataset = append(dataset, []any{"time", "upload", "download", "bonus", "seeding", "seeding_size"})
		for _, v := range rows {
			day, e := time.ParseInLocation("2006-01-02", v.Date, loc)
			if e != nil {
				continue
			}
			dataset = append(dataset, []any{day.UnixMilli(), v.Upload, v.Download, v.Bonus, v.Seeding, v.SeedingSize})
		}
	case "/api/v1/site/statistics/seedinfo":
		if address == "" {
			writeAPIError(w, 404, 404, "站点不参与统计")
			return
		}
		seeds, e := api.store.Seeds(r.Context(), address)
		if e != nil {
			statsError(w, e)
			return
		}
		dataset = append(dataset, []any{"seeders", "size"})
		for _, v := range seeds {
			dataset = append(dataset, []any{v[0], v[1]})
		}
	case "/api/v1/site/statistics/history":
		days, e := strconv.Atoi(r.FormValue("days"))
		if e != nil || days < 1 || days > 729 {
			writeAPIError(w, 400, 400, "统计天数无效")
			return
		}
		loc, e := rssWorkerLocation()
		if e != nil {
			statsError(w, e)
			return
		}
		end := time.Now().In(loc)
		if raw := r.FormValue("end_day"); raw != "" {
			end, e = time.ParseInLocation("2006-01-02", raw, loc)
			if e != nil {
				writeAPIError(w, 400, 400, "统计日期无效")
				return
			}
		}
		rows, e := api.store.Deltas(r.Context(), statsURLs(eligible), days+1, end)
		if e != nil {
			statsError(w, e)
			return
		}
		dataset = append(dataset, []any{"site", "upload", "download"})
		for _, v := range rows {
			dataset = append(dataset, []any{v.Site, v.Upload, v.Download})
		}
	default:
		writeAPIError(w, 404, 404, "未找到统计接口")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "dataset": dataset})
}
