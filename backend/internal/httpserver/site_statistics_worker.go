package httpserver

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

type siteStatisticsAPI struct {
	store     *sitestats.Store
	login     *siteLoginAPI
	signin    *siteSigninAPI
	mu        sync.Mutex
	cancel    context.CancelFunc
	lifecycle context.Context
	closed    bool
	wait      sync.WaitGroup
}

func (api *siteStatisticsAPI) snapshot(ctx context.Context, actor string) (string, string, []siteconfig.Site, map[string]any, error) {
	authority, cfg, e := api.login.authority(actor)
	if e != nil {
		return "", "", nil, nil, e
	}
	all, e := api.login.sites.List(ctx)
	if e != nil {
		return "", "", nil, nil, e
	}
	items := []siteconfig.Site{}
	seen := map[string]bool{}
	for _, s := range all {
		v, e := nativeSiteRecord(s)
		if e != nil {
			return "", "", nil, nil, e
		}
		if !truthy(v["statistic_enable"]) {
			continue
		}
		u := text(v["strict_url"])
		if u == "" || seen[u] {
			return "", "", nil, nil, sitestats.ErrState
		}
		seen[u] = true
		items = append(items, s)
	}
	channels, e := api.signin.channels.List(ctx)
	if e != nil {
		return "", "", nil, nil, e
	}
	app := objectValue(cfg["app"])
	fp := sitestats.Hash([]any{items, app["user_agent"], app["proxies"], objectValue(cfg["pt"])["ptrefresh_date_cron"], channels})
	return fp, authority, items, cfg, nil
}
func (api *siteStatisticsAPI) acquire(ctx context.Context) (*organization.JobGuard, error) {
	if api.login.guards == nil || *api.login.guards == nil {
		return nil, sitestats.ErrState
	}
	return (*api.login.guards).AcquireJob(ctx, "native-site-statistics")
}
func (api *siteStatisticsAPI) valid(ctx context.Context, j sitestats.Job) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	fp, authority, _, _, e := api.snapshot(ctx, j.Actor)
	if e != nil || fp != j.Fingerprint || authority != j.Authority {
		return sitestats.ErrState
	}
	if j.Epoch != "" {
		c, e := api.store.Control(ctx)
		if e != nil || !c.Enabled || c.Epoch != j.Epoch || c.Fingerprint != fp || c.Authority != authority {
			return sitestats.ErrState
		}
	}
	row, e := api.store.Get(ctx, j.ID)
	if e != nil || row.State != "running" || row.StopRequested {
		return sitestats.ErrState
	}
	return nil
}
func (api *siteStatisticsAPI) launch(ctx context.Context, j sitestats.Job) (sitestats.Job, error) {
	if old, e := api.store.Get(ctx, j.ID); e == nil {
		return old, nil
	}
	guard, e := api.acquire(ctx)
	if e != nil {
		return j, e
	}
	transferred := false
	defer func() {
		if !transferred {
			guard.Close()
		}
	}()
	if e = api.store.Recover(ctx); e != nil {
		return j, e
	}
	fp, authority, sites, _, e := api.snapshot(ctx, j.Actor)
	if e != nil || fp != j.Fingerprint || authority != j.Authority {
		return j, sitestats.ErrState
	}
	wanted := map[string]bool{}
	for _, id := range j.SiteIDs {
		if wanted[id] {
			return j, sitestats.ErrState
		}
		wanted[id] = true
	}
	selected := []siteconfig.Site{}
	for _, s := range sites {
		if len(wanted) == 0 || wanted[strconv.FormatInt(s.ID, 10)] {
			selected = append(selected, s)
		}
	}
	if len(selected) == 0 || (len(wanted) > 0 && len(selected) != len(wanted)) {
		return j, sitestats.ErrState
	}
	if j.Epoch != "" {
		c, e := api.store.Control(ctx)
		if e != nil || !c.Enabled || c.Epoch != j.Epoch || c.Fingerprint != fp || c.Authority != authority || j.ReadMessages != c.ReadMessages {
			return j, sitestats.ErrState
		}
	}
	j, old, e := api.store.Create(ctx, j)
	if e != nil || old {
		return j, e
	}
	api.mu.Lock()
	if api.closed {
		api.mu.Unlock()
		_ = api.store.Change(ctx, j.ID, func(v *sitestats.Job) error { v.State = "stopped"; return nil })
		return j, sitestats.ErrState
	}
	parent := api.lifecycle
	if parent == nil {
		parent = context.Background()
	}
	run, cancel := context.WithTimeout(parent, 30*time.Minute)
	api.cancel = cancel
	api.wait.Add(1)
	api.mu.Unlock()
	transferred = true
	go func() {
		defer api.wait.Done()
		defer guard.Close()
		defer cancel()
		defer func() { api.mu.Lock(); api.cancel = nil; api.mu.Unlock() }()
		api.execute(run, j, selected)
	}()
	return j, nil
}
func (api *siteStatisticsAPI) execute(ctx context.Context, j sitestats.Job, sites []siteconfig.Site) {
	if e := api.store.Change(ctx, j.ID, func(v *sitestats.Job) error {
		if v.State != "queued" {
			return sitestats.ErrState
		}
		v.State = "running"
		return nil
	}); e != nil {
		return
	}
	location, e := rssWorkerLocation()
	if e != nil {
		_ = api.store.Change(ctx, j.ID, func(v *sitestats.Job) error { v.State = "failed"; return nil })
		return
	}
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, site := range sites {
		site := site
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			result := sitestats.Result{SiteID: strconv.FormatInt(site.ID, 10), Name: site.Name, State: "failed", Reason: "站点抓取未完成，保留原统计"}
			if e := api.refresh(ctx, j, site, location); e == nil {
				result.State = "complete"
				result.Reason = "统计已更新"
			} else if errors.Is(e, context.Canceled) || errors.Is(e, sitestats.ErrState) {
				result.State = "stopped"
				result.Reason = "配置变化或任务已停止，保留原统计"
			} else if errors.Is(e, sitestats.ErrLogin) {
				result.Reason = "登录失效，请更新 Cookie"
			} else if errors.Is(e, sitestats.ErrPage) {
				result.Reason = "分页不完整或超过上限，保留原统计"
			}
			book, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer done()
			_ = api.store.Change(book, j.ID, func(v *sitestats.Job) error { v.Results = append(v.Results, result); return nil })
		}()
	}
	wg.Wait()
	book, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	row, e := api.store.Get(book, j.ID)
	if e != nil {
		return
	}
	state := "complete"
	success := 0
	for _, r := range row.Results {
		if r.State == "complete" {
			success++
		}
	}
	if success < len(sites) {
		state = "partial"
	}
	if success == 0 {
		state = "failed"
	}
	if ctx.Err() != nil {
		state = "stopped"
	}
	if api.valid(book, j) != nil {
		state = "stopped"
	}
	urls := []string{}
	for _, s := range sites {
		urls = append(urls, safeSiteOrigin(defaultString(s.SignURL, s.RSSURL)))
	}
	if e = api.store.Finish(book, j.ID, state, urls, time.Now().In(location)); e != nil {
		_ = api.store.Change(book, j.ID, func(v *sitestats.Job) error {
			if v.State == "running" {
				v.State = "unknown"
			}
			return nil
		})
	}
}
func (api *siteStatisticsAPI) refresh(ctx context.Context, j sitestats.Job, site siteconfig.Site, loc *time.Location) error {
	if e := api.valid(ctx, j); e != nil {
		return e
	}
	_, _, _, cfg, e := api.snapshot(ctx, j.Actor)
	if e != nil {
		return e
	}
	note := map[string]any{}
	if site.Note != "" && site.Note != "null" && decodeJSONMap([]byte(site.Note), &note) != nil {
		return sitestats.ErrState
	}
	origin := safeSiteOrigin(defaultString(site.SignURL, site.RSSURL))
	u, e := url.Parse(origin)
	if e != nil || u.Host == "" {
		return sitestats.ErrOrigin
	}
	app := objectValue(cfg["app"])
	transport := api.login.transport
	if note["proxy"] == "Y" || truthy(note["proxy"]) {
		transport, e = siteProxyTransport(transport, objectValue(app["proxies"]), u.Scheme)
		if e != nil {
			return e
		}
	}
	ua := text(note["ua"])
	if ua == "" {
		ua = text(app["user_agent"])
	}
	if ua == "" {
		ua = defaultSiteUserAgent
	}
	apiOrigin := ""
	if i := strings.Index(strings.ToLower(u.Hostname()), "m-team"); i >= 0 {
		apiURL := *u
		host := "api." + u.Hostname()[i:]
		if port := u.Port(); port != "" {
			host += ":" + port
		}
		apiURL.Host = host
		apiOrigin = apiURL.String()
	}
	client := &sitestats.Client{Origin: origin, APIOrigin: apiOrigin, Cookie: site.Cookie, APIKey: site.APIKey, UserAgent: ua, Transport: transport, Renderer: api.login.browser, Emulate: note["chrome"] == "Y" || truthy(note["chrome"]), ReadMessages: j.ReadMessages, Guard: func(ctx context.Context) error { return api.valid(ctx, j) }, Before: func(ctx context.Context, r sitestats.Request) error {
		if e := api.valid(ctx, j); e != nil {
			return e
		}
		if e := api.login.limits.Wait(ctx, api.login.sites, site.ID); e != nil {
			return e
		}
		return api.store.Change(ctx, j.ID, func(v *sitestats.Job) error {
			if v.State != "running" {
				return sitestats.ErrState
			}
			v.Requests++
			if r.Message {
				v.MessageRequests++
			}
			return nil
		})
	}}
	home, e := client.Home(ctx)
	if e != nil {
		return e
	}
	if strings.Contains(strings.ToLower(u.Hostname()), "hhanclub") {
		if id := siteClaimUserID(string(home)); id != "" {
			home, e = client.Fetch(ctx, sitestats.Request{Path: "userdetails.php?id=" + id})
			if e != nil {
				return e
			}
		}
	}
	if apiOrigin != "" {
		home = append(home, []byte(" M-Team")...)
	}
	v, e := sitestats.Parse(ctx, client, home, sitestats.Options{Site: site.Name, URL: origin, ReadMessages: j.ReadMessages, MTeamAPI: apiOrigin})
	if e != nil {
		return e
	}
	if e = api.valid(ctx, j); e != nil {
		return e
	}
	now := time.Now().In(loc)
	return api.store.Save(ctx, j, v, now.Format("2006-01-02"), now.Format("2006-01-02 15:04:05"), note["message"] == "Y" || truthy(note["message"]), site)
}

func startSiteStatisticsWorker(ctx context.Context, api *siteStatisticsAPI) func() {
	if api == nil || api.store == nil {
		return func() {}
	}
	api.mu.Lock()
	api.lifecycle = ctx
	api.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				api.tick(ctx, time.Now())
				api.tickRemote(ctx, time.Now())
			}
		}
	}()
	return func() {
		api.mu.Lock()
		api.closed = true
		if api.cancel != nil {
			api.cancel()
		}
		api.mu.Unlock()
		<-done
		api.wait.Wait()
	}
}
func (api *siteStatisticsAPI) tick(ctx context.Context, now time.Time) {
	api.recover(ctx)
	c, e := api.store.Control(ctx)
	if e != nil || !c.Enabled || c.Cron == "" || c.NextRun == "" {
		return
	}
	due, e := time.Parse(time.RFC3339Nano, c.NextRun)
	if e != nil || now.Before(due) {
		return
	}
	fp, authority, _, _, e := api.snapshot(ctx, c.Actor)
	if e != nil || fp != c.Fingerprint || authority != c.Authority {
		return
	}
	loc, e := time.LoadLocation(c.Location)
	if e != nil {
		return
	}
	schedule, e := parseSigninSchedule(c.Cron, loc)
	if e != nil {
		return
	}
	next, e := schedule.following(due, now)
	if e != nil || next.IsZero() {
		return
	}
	origin := "timer:" + c.Epoch + ":" + c.NextRun
	j := sitestats.Job{ID: sitestats.Hash([]any{c.Actor, origin}), Origin: origin, Actor: c.Actor, Epoch: c.Epoch, Fingerprint: fp, Authority: authority, ReadMessages: c.ReadMessages, Summary: true, SiteIDs: []string{}}
	if _, e = api.launch(ctx, j); e != nil {
		return
	}
	_ = api.store.Advance(ctx, c, next.UTC().Format(time.RFC3339Nano))
}

func (api *siteStatisticsAPI) recover(ctx context.Context) {
	guard, err := api.acquire(ctx)
	if err != nil {
		return
	}
	defer guard.Close()
	_ = api.store.Recover(ctx)
}
