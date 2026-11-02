package httpserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

func (api *siteSigninAPI) ensureSlack(parent context.Context, c sitesignin.Control, sources []siteSigninRemoteSource, now time.Time) {
	for _, source := range sources {
		if source.channel.Type != "slack" {
			continue
		}
		state, e := api.store.Remote(parent, source.digest)
		if e != nil {
			continue
		}
		if state.Scope == c.Scope.ID && (state.State == "unknown" || state.State == "blocked") {
			continue
		}
		if stamp, e := time.Parse(time.RFC3339Nano, state.Updated); e == nil && state.Scope == c.Scope.ID && state.State == "disconnected" && now.Sub(stamp) < 30*time.Second {
			continue
		}
		api.mu.Lock()
		if api.closed {
			api.mu.Unlock()
			return
		}
		if api.sockets == nil {
			api.sockets = map[string]context.CancelFunc{}
		}
		if api.sockets[source.digest] != nil {
			api.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithCancel(parent)
		api.sockets[source.digest] = cancel
		api.wait.Add(1)
		api.mu.Unlock()
		go func(s siteSigninRemoteSource) {
			defer api.wait.Done()
			defer cancel()
			defer func() { api.mu.Lock(); delete(api.sockets, s.digest); api.mu.Unlock() }()
			guard, e := (*api.login.guards).AcquireJob(ctx, signinGuard+"-remote-"+s.digest)
			if e != nil {
				return
			}
			defer guard.Close()
			unconfirmed, e := api.store.UnconfirmedRemoteConnect(ctx, c.Scope.ID, s.digest)
			if e != nil {
				return
			}
			if unconfirmed {
				_ = api.store.SetRemote(ctx, sitesignin.RemoteState{Source: s.digest, Scope: c.Scope.ID, State: "unknown", Reason: "原 Slack 连接初始化中断，请重新核对接收设置"})
				return
			}
			e = api.slackSocket(ctx, c, s)
			book, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer done()
			state, _ := api.store.Remote(book, s.digest)
			if state.Scope == c.Scope.ID && state.State != "unknown" && state.State != "blocked" {
				state.State = "disconnected"
				state.Reason = "实时连接已关闭；已保存的命令不会重做"
				_ = api.store.SetRemote(book, state)
			}
		}(source)
	}
}
func (api *siteSigninAPI) slackSocket(parent context.Context, c sitesignin.Control, s siteSigninRemoteSource) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// A revocation closes the live connection too, without waiting for another
	// provider event or allowing an obsolete reader to enqueue under a new grant.
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if api.remoteCurrent(ctx, c) != nil {
					cancel()
					return
				}
			}
		}
	}()
	state := sitesignin.RemoteState{Source: s.digest, Scope: c.Scope.ID, State: "connecting", Reason: "正在连接原 Slack 工作区"}
	if e := api.store.SetRemote(ctx, state); e != nil {
		return e
	}
	request, e := http.NewRequestWithContext(ctx, "POST", "https://slack.com/api/apps.connections.open", strings.NewReader(""))
	if e != nil {
		return e
	}
	request.Header.Set("Authorization", "Bearer "+text(s.config["app_token"]))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	raw, status, e := api.remoteCall(ctx, c, s, "slack-connect", request, api.login.transport)
	if e != nil {
		state.State = "unknown"
		state.Reason = "原 Slack 连接请求的应答未确认，请重新核对接收设置"
		_ = api.store.SetRemote(context.WithoutCancel(parent), state)
		return e
	}
	var response struct {
		OK  bool   `json:"ok"`
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &response) != nil || !response.OK || status != 200 {
		state.State = "blocked"
		state.Reason = "Slack 拒绝原连接请求，请核对原 app token"
		_ = api.store.SetRemote(context.WithoutCancel(parent), state)
		return sitesignin.ErrScope
	}
	u, e := url.Parse(response.URL)
	if e != nil || u.Scheme != "wss" || u.User != nil || u.Fragment != "" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".slack.com") || len(response.URL) > 16000 {
		return sitesignin.ErrScope
	}
	if e = api.remoteCurrent(ctx, c); e != nil {
		return e
	}
	var conn net.Conn
	var buffered *bufio.Reader
	if api.socketDial != nil {
		conn, buffered, e = api.socketDial(ctx, response.URL)
	} else {
		dialer := ws.Dialer{Timeout: 10 * time.Second, NetDial: signinSocketDial}
		conn, buffered, _, e = dialer.Dial(ctx, response.URL)
	}
	if e != nil {
		return sitesignin.ErrUnknown
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	state.State = "connected"
	state.Reason = "原 Slack 实时连接有效，/pts 交给唯一签到执行器"
	if e = api.store.SetRemote(ctx, state); e != nil {
		return e
	}
	var input io.Reader = conn
	if buffered != nil {
		input = buffered
	}
	reader := wsutil.NewReader(input, ws.StateClientSide)
	reader.CheckUTF8 = true
	reader.MaxFrameSize = 1 << 20
	reader.OnIntermediate = wsutil.ControlFrameHandler(conn, ws.StateClientSide)
	for {
		if e = api.remoteCurrent(ctx, c); e != nil {
			return e
		}
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Second))
		header, e := reader.NextFrame()
		if e != nil {
			return e
		}
		body, e := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
		if e != nil || len(body) > 1<<20 {
			return sitesignin.ErrState
		}
		if header.OpCode == ws.OpClose {
			return nil
		}
		if header.OpCode == ws.OpPing {
			_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
			if e = wsutil.WriteClientMessage(conn, ws.OpPong, body); e != nil {
				return e
			}
			continue
		}
		if header.OpCode != ws.OpText {
			continue
		}
		var envelope struct {
			ID      string          `json:"envelope_id"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			return sitesignin.ErrState
		}
		if envelope.Type == "disconnect" {
			return nil
		}
		if envelope.Type == "hello" {
			continue
		}
		if envelope.ID == "" || len(envelope.ID) > 512 {
			return sitesignin.ErrState
		}
		if envelope.Type == "events_api" || envelope.Type == "interactive" {
			if e = api.slackIncoming(ctx, c, s, envelope.Payload); e != nil && !errors.Is(e, sitesignin.ErrScope) {
				return e
			}
		}
		// The provider is acknowledged only after the original input and its
		// unique domain event have committed. A lost ACK can redeliver safely.
		ack, _ := json.Marshal(map[string]string{"envelope_id": envelope.ID})
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if e = wsutil.WriteClientMessage(conn, ws.OpText, ack); e != nil {
			return e
		}
	}
}

// Preserve the SDK's standard HTTPS proxy environment for Socket Mode. Proxy
// credentials stay in the CONNECT exchange and never enter an event document.
func signinSocketDial(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	proxyURL, e := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: address}})
	if e != nil {
		return nil, e
	}
	if proxyURL == nil {
		return dialer.DialContext(ctx, network, address)
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, sitesignin.ErrScope
	}
	host := proxyURL.Host
	if proxyURL.Port() == "" {
		port := "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
		host = net.JoinHostPort(proxyURL.Hostname(), port)
	}
	conn, e := dialer.DialContext(ctx, "tcp", host)
	if e != nil {
		return nil, e
	}
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	rawConn := conn
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if proxyURL.Scheme == "https" {
		secured := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if e = secured.HandshakeContext(ctx); e != nil {
			return nil, e
		}
		conn = secured
	}
	request := &http.Request{Method: "CONNECT", URL: &url.URL{Opaque: address}, Host: address, Header: http.Header{}}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username()+":"+password)))
	}
	if e = request.Write(conn); e != nil {
		return nil, e
	}
	buffered := bufio.NewReader(conn)
	response, e := http.ReadResponse(buffered, request)
	if e != nil {
		return nil, e
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		return nil, sitesignin.ErrScope
	}
	if buffered.Buffered() != 0 {
		return nil, sitesignin.ErrScope
	}
	_ = conn.SetDeadline(time.Time{})
	success = true
	return conn, nil
}
