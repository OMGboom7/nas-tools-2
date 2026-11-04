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
	URL            string `json:"url"`
	MessageEnabled bool   `json:"messageEnabled"`
}
type statsStatus struct {
	Fingerprint string            `json:"fingerprint"`
	GoOnly      bool              `json:"goOnly"`
	Cron        string            `json:"cron"`
	Control     sitestats.Control `json:"control"`
	Current     bool              `json:"current"`
	statsOverview
	Jobs []sitestats.Job `json:"jobs"`
}

type statsOverview struct {
	Targets []statsTarget        `json:"targets"`
	Items   []sitestats.Snapshot `json:"items"`
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
		overview, e := api.statsOverview(r.Context(), sites)
		if e != nil {
			statsError(w, e)
			return
		}
		jobs, e := api.store.Jobs(r.Context())
		if e != nil {
			statsError(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": statsStatus{
			Fingerprint: fp, GoOnly: api.login.pureGo, Cron: text(objectValue(cfg["pt"])["ptrefresh_date_cron"]),
			Control: c, Current: c.Enabled && c.Fingerprint == fp && c.Authority == authority, statsOverview: overview, Jobs: jobs,
		}})
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

func (api *siteStatisticsAPI) readyForStatsRead(w http.ResponseWriter) bool {
	if api == nil || api.store == nil || api.login == nil || api.login.sites == nil || api.login.auth == nil {
		writeAPIError(w, 503, 503, "统计服务不可用")
		return false
	}
	return true
}

func (api *siteStatisticsAPI) authorizeStatsRead(w http.ResponseWriter, r *http.Request) bool {
	claims, err := api.login.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "请重新登录")
		return false
	}
	if _, err = api.login.auth.service.FindUser(r.Context(), claims.Username); err != nil {
		writeAPIError(w, 401, 401, "当前账户不可用")
		return false
	}
	return true
}

func (api *siteStatisticsAPI) compat(w http.ResponseWriter, r *http.Request) {
	if !api.readyForStatsRead(w) {
		return
	}
	if r.Method == "GET" {
		service := siteService{configStore: api.login.config}
		if !service.requireSiteAPIKey(w, r) {
			return
		}
	} else if !api.authorizeStatsRead(w, r) || !parseSiteForm(w, r) {
		return
	}
	eligible, e := api.readStatsSites(r.Context())
	if e != nil {
		statsError(w, e)
		return
	}
	if r.URL.Path == "/api/v1/site/statistics" {
		out, e := api.legacyStatsList(r.Context(), eligible, legacyStatsInput{})
		if e != nil {
			statsError(w, e)
			return
		}
		for _, row := range out {
			// Retain the alias previously emitted by the Go endpoint.
			row["message_unread"] = row["msg_unread"]
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"user_statistics": out}})
		return
	}
	var dataset [][]any
	action := strings.TrimPrefix(r.URL.Path, "/api/v1/site/statistics/")
	address := statsAddressByName(eligible, r.FormValue("name"))
	switch r.URL.Path {
	case "/api/v1/site/statistics/activity", "/api/v1/site/statistics/seedinfo":
		if address == "" {
			writeAPIError(w, 404, 404, "未找到唯一参与统计的站点")
			return
		}
		dataset, e = api.statsSiteDataset(r.Context(), action, address)
	case "/api/v1/site/statistics/history":
		days, err := strconv.Atoi(r.FormValue("days"))
		if err != nil || days < 1 || days > 729 {
			writeAPIError(w, 400, 400, "统计天数无效")
			return
		}
		dataset, e = api.statsHistoryDataset(r.Context(), eligible, days, r.FormValue("end_day"))
		if _, ok := e.(*time.ParseError); ok {
			writeAPIError(w, 400, 400, "统计日期无效")
			return
		}
	default:
		writeAPIError(w, 404, 404, "未找到统计接口")
		return
	}
	if e != nil {
		statsError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "dataset": dataset})
}
