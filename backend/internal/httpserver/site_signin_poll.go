package httpserver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func (api *siteSigninAPI) remoteCurrent(ctx context.Context, c sitesignin.Control) error {
	if e := api.base(c.Scope, "", c.Scope.PluginDigest)(ctx); e != nil {
		return e
	}
	now, e := api.store.Control(ctx)
	if e != nil || !now.Enabled || sitelogin.Hash(now.Scope) != sitelogin.Hash(c.Scope) {
		return sitesignin.ErrScope
	}
	return nil
}
func (api *siteSigninAPI) remoteCall(ctx context.Context, c sitesignin.Control, s siteSigninRemoteSource, kind string, request *http.Request, transport http.RoundTripper) ([]byte, int, error) {
	if e := api.remoteCurrent(ctx, c); e != nil {
		return nil, 0, e
	}
	id, e := api.store.RemoteIntent(ctx, c.Scope, s.digest, kind, api.base(c.Scope, "", c.Scope.PluginDigest))
	if e != nil {
		return nil, 0, e
	}
	request.GetBody = nil
	request.Close = true
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		clone.ForceAttemptHTTP2 = false
		clone.MaxResponseHeaderBytes = 64 << 10
		clone.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		transport = clone
	}
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	acknowledged, status := false, 0
	defer func() {
		book, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if !acknowledged {
			_ = api.store.RemoteReceived(book, id, "unknown", status)
		}
	}()
	response, e := client.Do(request)
	if e != nil {
		return nil, 0, sitesignin.ErrUnknown
	}
	defer response.Body.Close()
	status = response.StatusCode
	raw, e := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if e != nil || len(raw) > 2<<20 {
		return nil, status, sitesignin.ErrUnknown
	}
	book, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	e = api.store.RemoteReceived(book, id, "received", status)
	done()
	if e != nil {
		return nil, status, sitesignin.ErrUnknown
	}
	acknowledged = true
	if e = api.remoteCurrent(ctx, c); e != nil {
		return nil, status, e
	}
	return raw, status, nil
}
func (api *siteSigninAPI) pollTelegram(ctx context.Context, c sitesignin.Control, now time.Time) error {
	if !c.Scope.Options.Remote {
		return nil
	}
	sources, _, e := api.remoteSources(ctx)
	if e != nil {
		return e
	}
	api.ensureSlack(ctx, c, sources, now)
	for _, source := range sources {
		if source.channel.Type != "telegram" {
			continue
		}
		state, e := api.store.Remote(ctx, source.digest)
		if e != nil {
			return e
		}
		if state.Scope == c.Scope.ID && (state.State == "blocked" || state.State == "unknown") {
			continue
		}
		if updated, e := time.Parse(time.RFC3339Nano, state.Updated); e == nil && now.Sub(updated) < 5*time.Second {
			continue
		}
		readyState := "polling"
		if source.mode == "webhook" {
			readyState = "webhook"
		}
		if state.Scope != c.Scope.ID || state.State != readyState {
			ready, e := api.syncTelegramMode(ctx, c, source, &state)
			if e != nil {
				return e
			}
			if !ready {
				continue
			}
		}
		if source.mode == "webhook" {
			continue
		}
		raw, status, e := api.telegramCall(ctx, c, source, "getUpdates", url.Values{"offset": {strconv.FormatInt(state.Offset, 10)}, "limit": {"100"}, "timeout": {"0"}, "allowed_updates": {`["message"]`}})
		state.Scope = c.Scope.ID
		state.State = "waiting"
		state.Reason = "读取消息未确认，将按原编号核对输入；不会直接重复签到"
		if e != nil {
			_ = api.store.SetRemote(context.WithoutCancel(ctx), state)
			continue
		}
		var v struct {
			OK     bool              `json:"ok"`
			Error  int               `json:"error_code"`
			Result []json.RawMessage `json:"result"`
		}
		if json.Unmarshal(raw, &v) != nil || !v.OK || status != 200 {
			if status == 401 || status == 403 || v.Error == 401 || v.Error == 403 {
				state.State = "blocked"
				state.Reason = "原 Telegram 凭据被拒绝，请更新原渠道后重新核对"
			}
			_ = api.store.SetRemote(ctx, state)
			continue
		}
		if len(v.Result) > 100 {
			return sitesignin.ErrState
		}
		for _, message := range v.Result {
			var update struct {
				ID json.Number `json:"update_id"`
			}
			if json.Unmarshal(message, &update) != nil {
				return sitesignin.ErrState
			}
			id, e := strconv.ParseInt(update.ID.String(), 10, 64)
			if e != nil || id < 0 || id == math.MaxInt64 {
				return sitesignin.ErrState
			}
			if id < state.Offset {
				continue
			}
			if e = api.telegramUpdate(ctx, c, source, message); e != nil && !errors.Is(e, sitesignin.ErrScope) {
				return e
			}
			if e = api.remoteCurrent(ctx, c); e != nil {
				return e
			}
			if id >= state.Offset {
				state.Offset = id + 1
			}
		}
		state.State = "polling"
		state.Reason = "原渠道输入已核对；授权命令已保存，其他输入保留待迁移"
		if e = api.store.SetRemote(ctx, state); e != nil {
			return e
		}
	}
	return nil
}
