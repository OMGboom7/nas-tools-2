package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func (api *siteSigninAPI) telegramCall(ctx context.Context, c sitesignin.Control, s siteSigninRemoteSource, method string, values url.Values) ([]byte, int, error) {
	cfg, e := api.login.config.Snapshot()
	if e != nil {
		return nil, 0, e
	}
	base := "https://api.telegram.org"
	if custom := text(objectValue(cfg["laboratory"])["telegram_domain"]); custom != "" {
		base = strings.TrimRight(custom, "/")
	}
	transport := api.login.transport
	u, e := url.Parse(base)
	if e != nil || u == nil || !validHTTPURL(base) {
		return nil, 0, sitesignin.ErrScope
	}
	if text(objectValue(objectValue(cfg["app"])["proxies"])[u.Scheme]) != "" {
		transport, e = siteProxyTransport(transport, objectValue(objectValue(cfg["app"])["proxies"]), u.Scheme)
		if e != nil {
			return nil, 0, e
		}
	}
	address := base + "/bot" + text(s.config["token"]) + "/" + method
	verb, body := "GET", ""
	if strings.HasPrefix(method, "get") {
		if len(values) > 0 {
			address += "?" + values.Encode()
		}
	} else {
		verb, body = "POST", values.Encode()
	}
	request, e := http.NewRequestWithContext(ctx, verb, address, strings.NewReader(body))
	if e != nil {
		return nil, 0, e
	}
	if verb == "POST" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return api.remoteCall(ctx, c, s, "telegram-"+method, request, transport)
}

func (api *siteSigninAPI) syncTelegramMode(ctx context.Context, c sitesignin.Control, s siteSigninRemoteSource, state *sitesignin.RemoteState) (bool, error) {
	state.Scope = c.Scope.ID
	state.State = "verifying"
	state.Reason = "正在核对原 Telegram 接收方式"
	save := func(ready bool) (bool, error) { return ready, api.store.SetRemote(ctx, *state) }
	raw, status, e := api.telegramCall(ctx, c, s, "getWebhookInfo", nil)
	if e != nil {
		return save(false)
	}
	var info struct {
		OK     bool `json:"ok"`
		Error  int  `json:"error_code"`
		Result struct {
			URL string `json:"url"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &info) != nil || !info.OK || status != 200 {
		if status == 401 || status == 403 || info.Error == 401 || info.Error == 403 {
			state.State = "blocked"
			state.Reason = "原 Telegram 凭据被拒绝，请更新原渠道后重新核对"
		}
		return save(false)
	}
	wanted := ""
	if s.mode == "webhook" {
		wanted = s.callback
	}
	if info.Result.URL == wanted {
		state.State = "polling"
		state.Reason = "原 Telegram 轮询接收已确认"
		if s.mode == "webhook" {
			state.State = "webhook"
			state.Reason = "原 Telegram 回调地址已确认"
		}
		return save(true)
	}
	method := "deleteWebhook"
	values := url.Values{}
	if s.mode == "webhook" {
		method = "setWebhook"
		values.Set("url", wanted)
		values.Set("allowed_updates", `["message"]`)
	}
	attempted, e := api.store.RemoteAttempted(ctx, c.Scope.ID, s.digest, "telegram-"+method)
	if e != nil {
		return false, e
	}
	if attempted {
		state.State = "unknown"
		state.Reason = "原接收方式变更已发送，但当前状态尚未确认；请重新核对，未重复变更"
		return save(false)
	}
	// Pending updates stay with the bot: never set drop_pending_updates.
	raw, status, e = api.telegramCall(ctx, c, s, method, values)
	if e != nil {
		state.Reason = "接收方式变更应答未确认，将只读核对当前状态"
		return save(false)
	}
	var result struct {
		OK     bool `json:"ok"`
		Result bool `json:"result"`
		Error  int  `json:"error_code"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.OK || !result.Result || status != 200 {
		state.Reason = "原接收方式变更未确认，将只读核对当前状态"
		if status == 401 || status == 403 || result.Error == 401 || result.Error == 403 {
			state.State = "blocked"
			state.Reason = "原 Telegram 凭据被拒绝，请更新原渠道后重新核对"
		}
		return save(false)
	}
	state.State = "polling"
	state.Reason = "原 Telegram webhook 已移除，原有消息继续保留"
	if s.mode == "webhook" {
		state.State = "webhook"
		state.Reason = "原 Telegram 回调地址已设置，原有消息继续保留"
	}
	return save(true)
}
