package httpserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

type siteSigninChallengeView struct {
	Kind     string              `json:"kind"`
	Nonce    string              `json:"nonce"`
	Deadline string              `json:"deadline"`
	Question string              `json:"question"`
	Image    string              `json:"image,omitempty"`
	Choices  []sitesignin.Choice `json:"choices"`
	Multiple bool                `json:"multiple"`
}
type siteSigninJobView struct {
	ID            string                   `json:"id"`
	Batch         string                   `json:"batch"`
	SiteID        string                   `json:"siteId"`
	Name          string                   `json:"name"`
	Host          string                   `json:"host"`
	Actor         string                   `json:"actor"`
	State         string                   `json:"state"`
	Day           string                   `json:"day"`
	Created       string                   `json:"created"`
	Updated       string                   `json:"updated"`
	Outcome       sitesignin.Outcome       `json:"outcome"`
	Requests      int                      `json:"requests"`
	Actions       int                      `json:"actions"`
	CanStop       bool                     `json:"canStop"`
	RuntimeActive bool                     `json:"runtimeActive"`
	Challenge     *siteSigninChallengeView `json:"challenge,omitempty"`
}
type siteSigninControlView struct {
	ID         string `json:"id"`
	Enabled    bool   `json:"enabled"`
	Current    bool   `json:"current"`
	Reason     string `json:"reason"`
	NextRun    string `json:"nextRun"`
	NextOnce   string `json:"nextOnce"`
	NextMissed string `json:"nextMissed"`
	Location   string `json:"location"`
	Remote     bool   `json:"remote"`
}
type siteSigninBatchView struct {
	ID       string              `json:"id"`
	Items    []siteSigninJobView `json:"items"`
	Summary  string              `json:"summary"`
	Notified bool                `json:"notified"`
}
type siteSigninStatus struct {
	Installed         bool                    `json:"installed"`
	GoOnly            bool                    `json:"goOnly"`
	BrowserConfigured bool                    `json:"browserConfigured"`
	SavedConfigured   bool                    `json:"savedConfigured"`
	Fingerprint       string                  `json:"fingerprint"`
	MaxConcurrency    int                     `json:"maxConcurrency"`
	Settings          sitesignin.Settings     `json:"settings"`
	Control           siteSigninControlView   `json:"control"`
	Sites             []siteSigninTarget      `json:"sites"`
	Jobs              []siteSigninJobView     `json:"jobs"`
	Dependencies      []sitesignin.Dependency `json:"dependencies"`
	Signals           []siteSigninSignalView  `json:"signals"`
	Remote            []siteSigninRemoteView  `json:"remote"`
}
type siteSigninSignalView struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Created string `json:"created"`
	Batch   string `json:"batch"`
}

func signinError(w http.ResponseWriter, e error) {
	if errors.Is(e, sql.ErrNoRows) {
		writeAPIError(w, 404, 404, "未找到原签到编号")
		return
	}
	if errors.Is(e, sitesignin.ErrBusy) || errors.Is(e, organization.ErrBusy) {
		writeAPIError(w, 409, 409, "签到任务正在执行，请查看原批次")
		return
	}
	writeAPIError(w, 409, 409, "原签到配置、站点或执行范围已变化，请重新读取；不会自动重做请求")
}
func (api *siteSigninAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.store == nil || api.system == nil || api.login == nil {
		writeAPIError(w, 503, 503, "站点签到服务不可用")
		return "", false
	}
	return api.login.actor(w, r, write)
}
func (api *siteSigninAPI) jobView(ctx context.Context, r sitesignin.Job, actor, token string) siteSigninJobView {
	out := siteSigninJobView{ID: r.ID, Batch: r.Batch, SiteID: strconv.FormatInt(r.Source.SiteID, 10), Name: r.Source.Name, Host: signinPublicTarget(r.Source).Host, Actor: r.Actor, State: r.State, Day: r.Day, Created: r.Created, Updated: r.Updated, Outcome: r.Outcome, CanStop: sitesignin.Active(r.State)}
	for _, op := range r.Operations {
		if op.Action {
			out.Actions++
		} else {
			out.Requests++
		}
	}
	api.mu.Lock()
	_, out.RuntimeActive = api.running[r.Batch]
	p := api.prompts[r.ID]
	if p != nil && !p.consumed && r.State == "waiting_input" && r.Nonce == p.nonce && r.Actor == actor {
		c := p.challenge
		out.Challenge = &siteSigninChallengeView{Kind: c.Kind, Nonce: p.nonce, Deadline: r.Deadline, Question: c.Question, Choices: c.Choices, Multiple: c.Multiple}
		if out.Challenge.Choices == nil {
			out.Challenge.Choices = []sitesignin.Choice{}
		}
		if len(c.Image) > 0 {
			out.Challenge.Image = "data:" + c.MIME + ";base64," + base64.StdEncoding.EncodeToString(c.Image)
		}
	}
	api.mu.Unlock()
	if out.Challenge != nil {
		b, _, e := api.store.Batch(ctx, r.Batch)
		if e != nil || api.current(ctx, b, r, token) != nil {
			out.Challenge = nil
		}
	}
	return out
}
func (api *siteSigninAPI) batchView(ctx context.Context, id, actor, token string) (siteSigninBatchView, error) {
	b, rows, e := api.store.Batch(ctx, id)
	out := siteSigninBatchView{ID: id, Items: []siteSigninJobView{}, Summary: b.Summary, Notified: b.Notified}
	if e != nil {
		return out, e
	}
	for _, r := range rows {
		out.Items = append(out.Items, api.jobView(ctx, r, actor, token))
	}
	return out, nil
}
func (api *siteSigninAPI) status(ctx context.Context, actor, token string, page int) (siteSigninStatus, error) {
	out := siteSigninStatus{Sites: []siteSigninTarget{}, Jobs: []siteSigninJobView{}, Remote: []siteSigninRemoteView{}, MaxConcurrency: 32, GoOnly: api.login.pureGo, BrowserConfigured: api.login.browser != nil && api.login.browser.Available()}
	_, _, settings, installed, e := api.pluginSnapshot(ctx)
	if e != nil {
		return out, e
	}
	out.Installed = installed
	out.Settings = settings
	out.Fingerprint, e = api.fingerprint(ctx, actor)
	if e != nil {
		return out, e
	}
	control, e := api.store.Control(ctx)
	if e != nil {
		return out, e
	}
	current := false
	if control.Enabled {
		current = api.base(control.Scope, "", control.Scope.PluginDigest)(ctx) == nil
		for _, source := range control.Scope.Sources {
			site, e := api.login.sites.Get(ctx, source.SiteID)
			if e != nil || sitelogin.Hash(site) != source.SiteDigest {
				current = false
			}
		}
	}
	out.Control = siteSigninControlView{ID: control.Scope.ID, Enabled: control.Enabled, Current: current, Reason: control.Reason, NextRun: control.NextRun, NextOnce: control.NextOnce, NextMissed: control.NextMissed, Location: control.Scope.Location, Remote: control.Scope.Options.Remote}
	if sources, _, e := api.remoteSources(ctx); e == nil {
		for _, s := range sources {
			view := s.public()
			state, e := api.store.Remote(ctx, s.digest)
			if e != nil {
				return out, e
			}
			if state.Scope == control.Scope.ID {
				view.State = state.State
				view.Reason = state.Reason
				view.Updated = state.Updated
			}
			out.Remote = append(out.Remote, view)
		}
	}
	cfg, e := api.login.config.Snapshot()
	if e != nil {
		return out, e
	}
	all, e := api.login.sites.List(ctx)
	if e != nil {
		return out, e
	}
	for _, site := range all {
		source, e := signinAddress(site, cfg)
		if e == nil {
			out.Sites = append(out.Sites, signinPublicTarget(source))
		}
	}
	raw, e := api.login.store.Saved(ctx)
	if e != nil {
		return out, e
	}
	credentials, e := sitelogin.ParseCredentials(raw)
	out.SavedConfigured = e == nil && credentials.Username != "" && credentials.Password != ""
	jobs, e := api.store.List(ctx, page)
	if e != nil {
		return out, e
	}
	for _, r := range jobs {
		out.Jobs = append(out.Jobs, api.jobView(ctx, r, actor, token))
	}
	out.Dependencies, e = api.store.Dependencies(ctx)
	if e != nil {
		return out, e
	}
	signals, e := api.store.Signals(ctx, false)
	if e != nil {
		return out, e
	}
	out.Signals = []siteSigninSignalView{}
	for _, s := range signals {
		out.Signals = append(out.Signals, siteSigninSignalView{s.ID, s.State, s.Reason, s.Created, s.Batch})
	}
	return out, nil
}
func (api *siteSigninAPI) manage(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, 405, 405, "不支持此签到操作")
		return
	}
	actor, ok := api.actor(w, r, r.Method != "GET")
	if !ok {
		return
	}
	token := r.Header.Get("Authorization")
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/site-signin"), "/")
	if r.Method == "GET" {
		if path == "" {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 0 {
				page = 1
			}
			data, e := api.status(r.Context(), actor, token, page)
			if e != nil {
				signinError(w, e)
				return
			}
			signinJSON(w, data)
			return
		}
		parts := strings.Split(path, "/")
		if len(parts) == 2 && parts[0] == "batches" {
			data, e := api.batchView(r.Context(), parts[1], actor, token)
			if e != nil {
				signinError(w, e)
				return
			}
			signinJSON(w, data)
			return
		}
		if len(parts) == 2 && parts[0] == "jobs" {
			job, e := api.store.Get(r.Context(), parts[1])
			if e != nil {
				signinError(w, e)
				return
			}
			signinJSON(w, api.jobView(r.Context(), job, actor, token))
			return
		}
		writeAPIError(w, 404, 404, "未找到签到记录入口")
		return
	}
	switch path {
	case "review":
		var in struct {
			Mode    string             `json:"mode"`
			SiteIDs []string           `json:"siteIds"`
			Options sitesignin.Options `json:"options"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		data, e := api.review(r.Context(), actor, in.Mode, in.SiteIDs, in.Options)
		if e != nil {
			signinError(w, e)
			return
		}
		signinJSON(w, data)
	case "start", "activate":
		var in struct {
			ID          string `json:"id"`
			Fingerprint string `json:"fingerprint"`
			Confirm     bool   `json:"confirm"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		if !in.Confirm {
			writeAPIError(w, 400, 400, "请核对本次签到目标并确认执行")
			return
		}
		if path == "start" {
			if old, e := api.batchView(r.Context(), in.ID, actor, token); e == nil {
				signinJSON(w, old)
				return
			}
		}
		p, e := api.checkedReview(r.Context(), actor, in.ID, in.Fingerprint)
		if e != nil {
			signinError(w, e)
			return
		}
		if path == "activate" {
			if e = api.activate(r.Context(), p); e != nil {
				signinError(w, e)
				return
			}
			signinJSON(w, map[string]any{"id": p.ID})
			return
		}
		if p.Mode != "manual" {
			signinError(w, sitesignin.ErrScope)
			return
		}
		if _, e = api.launch(r.Context(), sitesignin.Batch{ID: p.ID, Trigger: "manual", Scope: p.Scope}, p.Scope.Sources, token, nil, false); e != nil {
			signinError(w, e)
			return
		}
		data, e := api.batchView(r.Context(), p.ID, actor, token)
		if e != nil {
			signinError(w, e)
			return
		}
		signinJSON(w, data)
	case "stop":
		var in struct {
			ID      string `json:"id"`
			Confirm bool   `json:"confirm"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		if !in.Confirm {
			writeAPIError(w, 400, 400, "请确认停止这份签到设置")
			return
		}
		if e := api.store.StopControl(r.Context(), in.ID, "管理员已停止自动签到"); e != nil {
			signinError(w, e)
			return
		}
		api.cancelScope(in.ID)
		signinJSON(w, map[string]bool{"stopped": true})
	case "command":
		var in struct {
			ID      string `json:"id"`
			Command string `json:"command"`
		}
		if !siteLoginJSON(w, r, &in) {
			return
		}
		if !sitelogin.Hex(in.ID, 64) || in.Command != "/pts" {
			writeAPIError(w, 400, 400, "需要本次命令编号和 /pts")
			return
		}
		if e := api.publishCommand(r.Context(), "web:"+actor+":"+in.ID, "web", ""); e != nil {
			signinError(w, e)
			return
		}
		signinJSON(w, map[string]string{"id": in.ID})
	default:
		parts := strings.Split(path, "/")
		if len(parts) == 3 && parts[0] == "jobs" {
			if parts[2] == "input" {
				api.submitInput(w, r, actor, token, parts[1])
				return
			}
			if parts[2] == "stop" {
				api.stopJob(w, r, parts[1])
				return
			}
		}
		writeAPIError(w, 404, 404, "未找到签到操作")
	}
}
func signinJSON(w http.ResponseWriter, data any) {
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
func (api *siteSigninAPI) cancelScope(id string) {
	api.mu.Lock()
	ids := []string{}
	for id := range api.running {
		ids = append(ids, id)
	}
	api.mu.Unlock()
	for _, batch := range ids {
		b, _, e := api.store.Batch(context.Background(), batch)
		if e == nil && b.Trigger != "manual" && b.Scope.ID == id {
			api.mu.Lock()
			cancel := api.running[batch]
			api.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	}
}
func (api *siteSigninAPI) stopJob(w http.ResponseWriter, r *http.Request, id string) {
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		writeAPIError(w, 400, 400, "请确认停止原签到任务")
		return
	}
	api.mu.Lock()
	cancel := api.cancels[id]
	api.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if e := api.store.Finish(r.Context(), id, sitesignin.Outcome{State: "stopped", Reason: "管理员已停止原任务；已发请求仍按回执核对"}); e != nil {
		signinError(w, e)
		return
	}
	signinJSON(w, map[string]bool{"stopped": true})
}
func (api *siteSigninAPI) submitInput(w http.ResponseWriter, r *http.Request, actor, token, id string) {
	var in struct {
		Nonce   string   `json:"nonce"`
		Values  []string `json:"values"`
		Confirm bool     `json:"confirm"`
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if !in.Confirm || len(in.Values) == 0 || len(in.Values) > 64 {
		signinError(w, sitesignin.ErrInput)
		return
	}
	for _, v := range in.Values {
		if v == "" || !utf8.ValidString(v) || !sitelogin.Text(v, 4096) {
			signinError(w, sitesignin.ErrInput)
			return
		}
	}
	job, e := api.store.Get(r.Context(), id)
	if e != nil || job.Actor != actor {
		signinError(w, sitesignin.ErrInput)
		return
	}
	b, _, e := api.store.Batch(r.Context(), job.Batch)
	if e != nil || api.current(r.Context(), b, job, token) != nil {
		signinError(w, sitesignin.ErrInput)
		return
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	p := api.prompts[id]
	if p == nil || p.consumed || p.nonce != in.Nonce {
		signinError(w, sitesignin.ErrInput)
		return
	}
	if len(p.challenge.Choices) > 0 {
		allowed := map[string]bool{}
		for _, c := range p.challenge.Choices {
			allowed[c.Value] = true
		}
		seen := map[string]bool{}
		for _, v := range in.Values {
			if !allowed[v] || seen[v] {
				signinError(w, sitesignin.ErrInput)
				return
			}
			seen[v] = true
		}
		if !p.challenge.Multiple && len(in.Values) != 1 {
			signinError(w, sitesignin.ErrInput)
			return
		}
	} else if len(in.Values) != 1 {
		signinError(w, sitesignin.ErrInput)
		return
	}
	if e = api.store.Consume(r.Context(), id, in.Nonce, api.base(b.Scope, token, b.Scope.PluginDigest)); e != nil {
		signinError(w, e)
		return
	}
	p.consumed = true
	p.input <- append([]string{}, in.Values...)
	signinJSON(w, map[string]bool{"received": true})
}
