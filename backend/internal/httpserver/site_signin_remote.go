package httpserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

type siteSigninRemoteSource struct {
	channel                notificationconfig.Channel
	config                 map[string]any
	digest, mode, receiver string
}
type siteSigninRemoteView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Mode       string `json:"mode"`
	Receiver   string `json:"receiver"`
	Permission string `json:"permission"`
	State      string `json:"state,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Updated    string `json:"updated,omitempty"`
}

func (s siteSigninRemoteSource) public() siteSigninRemoteView {
	permission := "原渠道已认证的用户可发送 /pts"
	if s.channel.Type == "telegram" {
		permission = "仅原 Telegram 管理员名单可执行 /pts"
	}
	if s.channel.Type == "wechat" && text(s.config["adminUser"]) != "" {
		permission = "仅原企业微信管理员名单可执行 /pts"
	}
	return siteSigninRemoteView{ID: strconv.FormatInt(s.channel.ID, 10), Name: s.channel.Name, Type: s.channel.Type, Mode: s.mode, Receiver: s.receiver, Permission: permission}
}
func (api *siteSigninAPI) remoteSources(ctx context.Context) ([]siteSigninRemoteSource, string, error) {
	out := []siteSigninRemoteSource{}
	if api.channels == nil {
		return out, sitelogin.Hash(out), nil
	}
	all, e := api.channels.List(ctx)
	if e != nil {
		return nil, "", e
	}
	cfg, e := api.login.config.Snapshot()
	if e != nil {
		return nil, "", e
	}
	for _, c := range all {
		if c.Enabled == 0 || c.Interactive == 0 || !interactiveNotificationTypes[c.Type] {
			continue
		}
		var conf map[string]any
		d := json.NewDecoder(strings.NewReader(c.Config))
		d.UseNumber()
		if d.Decode(&conf) != nil || conf == nil {
			return nil, "", sitesignin.ErrScope
		}
		s := siteSigninRemoteSource{channel: c, config: conf, digest: sitelogin.Hash(c), mode: "webhook", receiver: "本机已配置回调"}
		switch c.Type {
		case "telegram":
			if !validTelegramToken(text(conf["token"])) {
				return nil, "", sitesignin.ErrScope
			}
			base := "https://api.telegram.org"
			if custom := text(objectValue(cfg["laboratory"])["telegram_domain"]); custom != "" {
				base = strings.TrimRight(custom, "/")
			}
			if browserruntime.Origin(base) == "" {
				return nil, "", sitesignin.ErrScope
			}
			if !truthy(conf["webhook"]) {
				s.mode = "poll"
				s.receiver = base + "/bot[原机器人]/getUpdates"
			} else {
				s.receiver = "原 /telegram 回调、API key 与 IP 范围"
			}
		case "slack":
			s.mode = "socket"
			s.receiver = "https://slack.com/api/apps.connections.open 及返回的 *.slack.com 实时连接"
			if text(conf["app_token"]) == "" || text(conf["bot_token"]) == "" {
				return nil, "", sitesignin.ErrScope
			}
		case "synologychat":
			if text(conf["token"]) == "" {
				return nil, "", sitesignin.ErrScope
			}
			s.receiver = "原 /synology 回调、API key、机器人 token 与 IP 范围"
		case "wechat":
			if text(conf["token"]) == "" || text(conf["encodingAESKey"]) == "" || text(conf["corpid"]) == "" {
				return nil, "", sitesignin.ErrScope
			}
			s.receiver = "原 /wechat 加密回调、签名及企业身份"
		}
		out = append(out, s)
	}
	snapshots := []notificationconfig.Channel{}
	for _, s := range out {
		snapshots = append(snapshots, s.channel)
	}
	return out, sitelogin.Hash(snapshots), nil
}
func (api *siteSigninAPI) remoteControl(ctx context.Context) (sitesignin.Control, []siteSigninRemoteSource, error) {
	c, e := api.store.Control(ctx)
	if e != nil || !c.Enabled || !c.Scope.Options.Remote {
		return c, nil, sitesignin.ErrScope
	}
	if e = api.base(c.Scope, "", c.Scope.PluginDigest)(ctx); e != nil {
		return c, nil, e
	}
	sources, _, e := api.remoteSources(ctx)
	return c, sources, e
}
func (api *siteSigninAPI) publishCommand(ctx context.Context, key, origin, source string) error {
	c, sources, e := api.remoteControl(ctx)
	if e != nil {
		return e
	}
	if source != "" {
		found := false
		for _, s := range sources {
			if s.digest == source {
				found = true
			}
		}
		if !found {
			return sitesignin.ErrScope
		}
	}
	return api.store.Publish(ctx, c.Scope, key, origin, map[string]string{"command": "/pts", "origin": origin}, api.base(c.Scope, "", c.Scope.PluginDigest))
}
func signinStringList(raw, separator string) map[string]bool {
	out := map[string]bool{}
	for _, v := range strings.Split(raw, separator) {
		v = strings.TrimSpace(v)
		if v != "" {
			out[v] = true
		}
	}
	return out
}
func (s siteSigninRemoteSource) allowed(user, body string) bool {
	if user == "" {
		return false
	}
	switch s.channel.Type {
	case "telegram":
		admins := signinStringList(text(s.config["admin_ids"]), ",")
		if strings.HasPrefix(body, "/") {
			return admins[user]
		}
		return admins[user] || signinStringList(text(s.config["user_ids"]), ",")[user]
	case "wechat":
		if strings.HasPrefix(body, "/") && text(s.config["adminUser"]) != "" {
			return signinStringList(text(s.config["adminUser"]), ";")[user]
		}
	}
	return true
}
func (api *siteSigninAPI) incoming(ctx context.Context, c sitesignin.Control, s siteSigninRemoteSource, id, user, username, body string, at time.Time, raw []byte) error {
	created, e := time.Parse(time.RFC3339Nano, c.Scope.Created)
	if e != nil {
		return e
	}
	if id == "" || len(id) > 1000 || !sitelogin.Text(user, 512) || len(username) > 1000 || len(body) > 32000 || at.IsZero() || at.Before(created.Truncate(time.Second)) || at.After(time.Now().Add(2*time.Minute)) || !s.allowed(user, body) {
		return sitesignin.ErrScope
	}
	channel := map[string]string{"telegram": "Telegram", "wechat": "微信", "synologychat": "Synology Chat", "slack": "Slack"}[s.channel.Type]
	data := map[string]any{"channel": channel, "user_id": user, "user_name": username, "message": body}
	return api.store.Incoming(ctx, c.Scope, s.digest, id, user, s.channel.ID, json.RawMessage(raw), data, body == "/pts", api.base(c.Scope, "", c.Scope.PluginDigest))
}

type signinTelegramUpdate struct {
	ID      json.Number `json:"update_id"`
	Message struct {
		ID   json.Number `json:"message_id"`
		Date int64       `json:"date"`
		Text string      `json:"text"`
		From struct {
			ID   json.Number `json:"id"`
			Bot  bool        `json:"is_bot"`
			Name string      `json:"username"`
		} `json:"from"`
	} `json:"message"`
}

func (api *siteSigninAPI) telegramUpdate(ctx context.Context, c sitesignin.Control, s siteSigninRemoteSource, raw []byte) error {
	var u signinTelegramUpdate
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&u) != nil || u.Message.From.Bot || u.ID.String() == "" || u.Message.Text == "" {
		return sitesignin.ErrScope
	}
	return api.incoming(ctx, c, s, u.ID.String(), u.Message.From.ID.String(), u.Message.From.Name, u.Message.Text, time.Unix(u.Message.Date, 0), raw)
}
func constant(a, b string) bool {
	return a != "" && len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func signinRemoteIP(r *http.Request) netip.Addr {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		host = r.RemoteAddr
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}
func signinIPAllowed(raw any, ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	values := objectValue(raw)
	key := "ipv4"
	if ip.Is6() {
		key = "ipv6"
	}
	list := text(values[key])
	if list == "" {
		return true
	}
	for _, str := range strings.Split(list, ",") {
		str = strings.TrimSpace(str)
		if p, e := netip.ParsePrefix(str); e == nil && p.Contains(ip) {
			return true
		}
		if candidate, e := netip.ParseAddr(str); e == nil && candidate.Unmap() == ip {
			return true
		}
	}
	return false
}
func (api *siteSigninAPI) remoteHTTP(w http.ResponseWriter, r *http.Request) {
	if api == nil || api.store == nil || api.login == nil || !api.login.pureGo {
		writeAPIError(w, 501, 501, "消息签到需要独立运行模式")
		return
	}
	c, sources, e := api.remoteControl(r.Context())
	if e != nil {
		writeAPIError(w, 409, 409, "消息签到尚未启用或原设置已变化")
		return
	}
	typeName := map[string]string{"/telegram": "telegram", "/synology": "synologychat", "/slack": "slack", "/wechat": "wechat"}[r.URL.Path]
	var source *siteSigninRemoteSource
	for i := range sources {
		if sources[i].channel.Type == typeName {
			source = &sources[i]
			break
		}
	}
	if source == nil {
		writeAPIError(w, 409, 409, "原交互渠道未启用")
		return
	}
	if typeName == "wechat" {
		api.wechatHTTP(w, r, c, *source)
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, 405, 405, "需要消息回调")
		return
	}
	cfg, e := api.login.config.Snapshot()
	if e != nil {
		signinError(w, e)
		return
	}
	security := objectValue(cfg["security"])
	if !constant(r.URL.Query().Get("apikey"), text(security["api_key"])) {
		writeAPIError(w, 401, 401, "消息回调凭据无效")
		return
	}
	ip := signinRemoteIP(r)
	allowed := true
	if typeName == "telegram" {
		allowed = signinIPAllowed(security["telegram_webhook_allow_ip"], ip)
	}
	if typeName == "synologychat" {
		allowed = signinIPAllowed(security["synology_webhook_allow_ip"], ip)
	}
	if typeName == "slack" {
		allowed = ip.IsLoopback()
	}
	if !allowed {
		writeAPIError(w, 403, 403, "消息来源不在原允许范围内")
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if e != nil {
		writeAPIError(w, 400, 400, "消息过大")
		return
	}
	if typeName == "telegram" {
		e = api.telegramUpdate(r.Context(), c, *source, raw)
	} else if typeName == "slack" {
		e = api.slackIncoming(r.Context(), c, *source, raw)
	} else {
		values, parseErr := url.ParseQuery(string(raw))
		if parseErr != nil || !constant(values.Get("token"), text(source.config["token"])) {
			writeAPIError(w, 403, 403, "机器人凭据无效")
			return
		}
		at, parseErr := strconv.ParseInt(values.Get("timestamp"), 10, 64)
		if parseErr != nil {
			writeAPIError(w, 400, 400, "消息时间无效")
			return
		}
		payload, _ := json.Marshal(values)
		e = api.incoming(r.Context(), c, *source, values.Get("post_id"), values.Get("user_id"), values.Get("username"), values.Get("text"), time.Unix(at, 0), payload)
	}
	if e != nil {
		writeAPIError(w, 403, 403, "消息权限、时间或原执行范围无效")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Ok"))
}
func (api *siteSigninAPI) slackIncoming(ctx context.Context, c sitesignin.Control, s siteSigninRemoteSource, raw []byte) error {
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return sitesignin.ErrState
	}
	kind := text(v["type"])
	id, user, name, body, stamp := "", "", "", "", ""
	switch kind {
	case "event_callback":
		event := objectValue(v["event"])
		if text(event["bot_id"]) != "" {
			return nil
		}
		id = text(v["event_id"])
		user = text(event["user"])
		body = regexp.MustCompile(`<@[0-9A-Z]+>`).ReplaceAllString(text(event["text"]), "")
		body = strings.TrimSpace(body)
		stamp = text(event["event_ts"])
		if stamp == "" {
			stamp = text(event["ts"])
		}
	case "message":
		if text(v["bot_id"]) != "" {
			return nil
		}
		id = text(v["client_msg_id"])
		if id == "" {
			id = text(v["ts"])
		}
		user = text(v["user"])
		name = user
		body = text(v["text"])
		stamp = text(v["ts"])
	case "block_actions":
		u := objectValue(v["user"])
		user = text(u["id"])
		name = text(u["name"])
		actions, ok := v["actions"].([]any)
		if !ok || len(actions) != 1 {
			return sitesignin.ErrScope
		}
		a := objectValue(actions[0])
		body = text(a["value"])
		stamp = text(a["action_ts"])
		id = text(v["trigger_id"])
		if id == "" {
			id = stamp
		}
	case "shortcut":
		u := objectValue(v["user"])
		user = text(u["id"])
		name = text(u["username"])
		body = text(v["callback_id"])
		stamp = text(v["action_ts"])
		id = text(v["trigger_id"])
	default:
		return sitesignin.ErrScope
	}
	seconds, e := strconv.ParseFloat(stamp, 64)
	if e != nil {
		return sitesignin.ErrScope
	}
	return api.incoming(ctx, c, s, id, user, name, body, time.Unix(int64(seconds), 0), raw)
}
