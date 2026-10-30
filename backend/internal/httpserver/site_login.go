package httpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type siteLoginChallengeView struct {
	Kind     string `json:"kind"`
	Nonce    string `json:"nonce"`
	Deadline string `json:"deadline"`
	Image    string `json:"image,omitempty"`
}
type siteLoginView struct {
	ID            string                  `json:"id"`
	Batch         string                  `json:"batch"`
	SiteID        string                  `json:"siteId"`
	Name          string                  `json:"name"`
	Host          string                  `json:"host"`
	Actor         string                  `json:"actor"`
	State         string                  `json:"state"`
	Reason        string                  `json:"reason"`
	LoginState    string                  `json:"loginState"`
	Created       string                  `json:"created"`
	Updated       string                  `json:"updated"`
	Requests      int                     `json:"requests"`
	Actions       int                     `json:"actions"`
	CanStop       bool                    `json:"canStop"`
	RuntimeActive bool                    `json:"runtimeActive"`
	Challenge     *siteLoginChallengeView `json:"challenge,omitempty"`
}
type siteLoginBatchView struct {
	ID             string          `json:"id"`
	Items          []siteLoginView `json:"items"`
	RememberState  string          `json:"rememberState"`
	RememberReason string          `json:"rememberReason"`
}

func (api *siteLoginAPI) actor(w http.ResponseWriter, r *http.Request, write bool) (string, bool) {
	if api == nil || api.store == nil || api.config == nil || api.sites == nil || api.auth == nil {
		writeAPIError(w, 503, 503, "站点登录服务不可用")
		return "", false
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "请重新登录")
		return "", false
	}
	if _, err = api.auth.service.FindUser(r.Context(), claims.Username); err != nil {
		writeAPIError(w, 401, 401, "当前账户不可用")
		return "", false
	}
	if _, _, err = api.authority(claims.Username); err != nil {
		writeAPIError(w, 403, 403, "需要当前主管理员账户")
		return "", false
	}
	if write && !api.pureGo {
		writeAPIError(w, 501, 501, "请在 Go 独立运行模式使用站点登录")
		return "", false
	}
	return claims.Username, true
}
func siteLoginJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		writeAPIError(w, 400, 400, "站点登录请求格式无效")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		writeAPIError(w, 400, 400, "站点登录请求包含额外内容")
		return false
	}
	return true
}
func (api *siteLoginAPI) view(ctx context.Context, r sitelogin.Record, actor, token string) siteLoginView {
	out := siteLoginView{ID: r.ID, Batch: r.Batch, SiteID: strconv.FormatInt(r.Source.SiteID, 10), Name: r.Name, Host: r.Host, Actor: r.Actor, State: r.State, Reason: r.Reason, LoginState: r.LoginState, Created: r.Created, Updated: r.Updated, CanStop: sitelogin.Active(r.State)}
	for _, op := range r.Operations {
		if strings.HasPrefix(op.Kind, "action:") {
			out.Actions++
		} else {
			out.Requests++
		}
	}
	api.mu.Lock()
	out.RuntimeActive = api.running[r.Batch]
	prompt := api.prompts[r.ID]
	if prompt != nil && !prompt.consumed && r.State == "waiting_input" && r.Nonce == prompt.nonce && r.Need == prompt.kind {
		out.Challenge = &siteLoginChallengeView{Kind: r.Need, Nonce: r.Nonce, Deadline: r.Deadline}
		if len(prompt.image) > 0 {
			out.Challenge.Image = "data:" + prompt.mime + ";base64," + base64.StdEncoding.EncodeToString(prompt.image)
		}
	}
	api.mu.Unlock()
	if out.Challenge != nil && api.current(ctx, r, actor, token) != nil {
		out.Challenge = nil
	}
	return out
}
func (api *siteLoginAPI) batchView(ctx context.Context, b sitelogin.Batch, records []sitelogin.Record, actor, token string) siteLoginBatchView {
	out := siteLoginBatchView{ID: b.ID, Items: []siteLoginView{}, RememberState: b.RememberState, RememberReason: b.RememberReason}
	for _, r := range records {
		out.Items = append(out.Items, api.view(ctx, r, actor, token))
	}
	return out
}
func (api *siteLoginAPI) status(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, false)
	if !ok {
		return
	}
	_ = actor
	raw, err := api.store.Saved(r.Context())
	if err != nil {
		writeAPIError(w, 503, 503, "原保存账户信息不可用")
		return
	}
	saved, err := sitelogin.ParseCredentials(raw)
	if err != nil {
		writeAPIError(w, 503, 503, "原保存账户信息格式无效，请先核对配置")
		return
	}
	configured := api.browser != nil && api.browser.Available()
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"browserConfigured": configured, "goOnly": api.pureGo, "savedConfigured": saved.Username != "" && saved.Password != "", "maxSites": sitelogin.MaxSites}})
}
func (api *siteLoginAPI) serveReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, true)
	if !ok {
		return
	}
	var in siteLoginOptions
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if api.browser == nil || !api.browser.Available() {
		writeAPIError(w, 503, 503, "原生浏览器未安装或不可执行")
		return
	}
	data, err := api.review(r.Context(), actor, in)
	if err != nil {
		writeAPIError(w, 409, 409, "无法确认所选站点、登录目录、原配置或保存账户，请刷新后核对")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
func (api *siteLoginAPI) serveStart(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, true)
	if !ok {
		return
	}
	var in struct {
		ScopeToken string `json:"scopeToken"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoStep    string `json:"twoStepCode"`
		Confirm    bool   `json:"confirm"`
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if !in.Confirm {
		writeAPIError(w, 400, 400, "请确认登录目标及会话更新")
		return
	}
	plan, err := api.unseal(actor, in.ScopeToken)
	if err != nil {
		writeAPIError(w, 409, 409, "登录目标审核已过期或配置变化，请重新核对")
		return
	}
	if api.browser == nil || !api.browser.Available() {
		writeAPIError(w, 503, 503, "原生浏览器不可用")
		return
	}
	credentials := sitelogin.Credentials{Username: in.Username, Password: in.Password, TwoStep: in.TwoStep}
	if plan.Options.UseSaved {
		raw, err := api.store.Saved(r.Context())
		if err != nil {
			writeAPIError(w, 503, 503, "原保存账户不可用")
			return
		}
		credentials, err = sitelogin.ParseCredentials(raw)
		if err != nil {
			writeAPIError(w, 409, 409, "原保存账户格式无效")
			return
		}
		if in.TwoStep != "" {
			credentials.TwoStep = in.TwoStep
		}
	}
	if credentials.Username == "" || credentials.Password == "" || !sitelogin.Text(credentials.Username, 1024) || !sitelogin.Text(credentials.Password, 4096) || !sitelogin.Text(credentials.TwoStep, 128) {
		writeAPIError(w, 400, 400, "请提供有效的站点账户、密码和本次两步信息")
		return
	}
	_, err = api.launch(r.Context(), actor, r.Header.Get("Authorization"), plan, credentials)
	if err != nil {
		status := 409
		if errors.Is(err, organization.ErrBusy) || errors.Is(err, sitelogin.ErrBusy) {
			writeAPIError(w, status, status, "已有站点登录正在处理，请查看原请求")
			return
		}
		writeAPIError(w, status, status, "原登录来源已变化或无法创建原回执；未确认的请求不会自动重发")
		return
	}
	b, records, err := api.store.Batch(r.Context(), plan.ID)
	if err != nil {
		writeAPIError(w, 503, 503, "登录请求已创建，请按原编号查询")
		return
	}
	writeJSON(w, 202, map[string]any{"code": 0, "success": true, "data": api.batchView(r.Context(), b, records, actor, r.Header.Get("Authorization"))})
}
func (api *siteLoginAPI) serveList(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, false)
	if !ok {
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		var err error
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 100000 {
			writeAPIError(w, 400, 400, "页码无效")
			return
		}
	}
	records, err := api.store.List(r.Context(), page)
	if err != nil {
		writeAPIError(w, 503, 503, "原登录回执无法读取")
		return
	}
	items := []siteLoginView{}
	for _, record := range records {
		items = append(items, api.view(r.Context(), record, actor, r.Header.Get("Authorization")))
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"page": page, "items": items}})
}
func (api *siteLoginAPI) serveDetail(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, false)
	if !ok {
		return
	}
	record, err := api.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, 404, 404, "原登录请求不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": api.view(r.Context(), record, actor, r.Header.Get("Authorization"))})
}
func (api *siteLoginAPI) serveBatch(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, false)
	if !ok {
		return
	}
	b, records, err := api.store.Batch(r.Context(), r.PathValue("id"))
	if err != nil {
		writeAPIError(w, 404, 404, "原登录批次不存在")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": api.batchView(r.Context(), b, records, actor, r.Header.Get("Authorization"))})
}
func (api *siteLoginAPI) serveInput(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.actor(w, r, true)
	if !ok {
		return
	}
	var in struct{ Nonce, Value string }
	if !siteLoginJSON(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	record, err := api.store.Get(r.Context(), id)
	if err != nil || record.Actor != actor || api.current(r.Context(), record, actor, r.Header.Get("Authorization")) != nil {
		writeAPIError(w, 409, 409, "原验证码请求已失效或来源变化")
		return
	}
	if !sitelogin.Text(in.Value, 254) || in.Value == "" {
		writeAPIError(w, 400, 400, "请输入本次验证信息")
		return
	}
	api.mu.Lock()
	prompt := api.prompts[id]
	if prompt == nil || prompt.consumed || prompt.nonce != in.Nonce || record.Nonce != in.Nonce {
		api.mu.Unlock()
		writeAPIError(w, 409, 409, "该输入已处理或原浏览器不在本进程，请核对原回执")
		return
	}
	if prompt.kind != "email" && len(in.Value) > 128 {
		api.mu.Unlock()
		writeAPIError(w, 400, 400, "验证信息过长")
		return
	}
	prompt.consumed = true
	api.mu.Unlock()
	if err = api.store.ConsumePrompt(r.Context(), id, in.Nonce, api.baseGuard(record.Source, actor, r.Header.Get("Authorization"))); err != nil {
		writeAPIError(w, 409, 409, "原输入已失效，请按原编号查询")
		return
	}
	prompt.input <- in.Value
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "本次输入已交给原请求"})
}
func (api *siteLoginAPI) serveStop(w http.ResponseWriter, r *http.Request) {
	_, ok := api.actor(w, r, true)
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	if !sitelogin.Text(in.Reason, 512) || utf8.RuneCountInString(in.Reason) < 10 {
		writeAPIError(w, 400, 400, "请填写至少十个字符的停止说明")
		return
	}
	id := r.PathValue("id")
	record, err := api.store.Get(r.Context(), id)
	if err != nil || !sitelogin.Active(record.State) {
		writeAPIError(w, 409, 409, "原请求已结束或不可停止")
		return
	}
	api.mu.Lock()
	cancel := api.cancels[id]
	live := api.running[record.Batch]
	api.mu.Unlock()
	var guard *organization.JobGuard
	if !live {
		guard, err = api.acquire(r.Context())
		if err != nil {
			writeAPIError(w, 409, 409, "原执行器仍在处理，无法从另一进程关闭")
			return
		}
		defer guard.Close()
	}
	if cancel != nil {
		cancel()
	}
	if err = api.store.Finish(r.Context(), id, sitelogin.Interrupted(record), in.Reason); err != nil {
		writeAPIError(w, 409, 409, "原请求已变化，请只读刷新")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "本地处理已停止；已发送的站点请求不能撤回"})
}
