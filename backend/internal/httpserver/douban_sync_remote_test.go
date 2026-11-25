package httpserver

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

func TestDoubanSyncOriginalRemoteCallbacksAndWorkerAcrossRestart(t *testing.T) {
	f := newLoginFixture(t, nil)
	ctx := t.Context()
	api := f.signin.douban
	channel := notificationconfig.Channel{Name: "Original Synology", Type: "synologychat", Config: `{"token":"fixture-token"}`, Enabled: 1, Interactive: 1, Switches: "[]"}
	var err error
	channel.ID, err = f.signin.channels.Save(ctx, channel)
	if err != nil {
		t.Fatal(err)
	}
	const path = "/api/v1/plugins/DoubanSync"
	if w := f.request(t, "POST", path+"/install", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	save := func(enabled bool) {
		t.Helper()
		w := f.request(t, "GET", path, nil)
		var detail struct{ Data pluginConfigDetail }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil {
			t.Fatal(w.Body.String())
		}
		w = f.request(t, "PUT", path, map[string]any{"fingerprint": detail.Data.Meta.Fingerprint, "values": map[string]any{"go_remote": enabled, "users": "fixture", "types": []string{"wish"}, "sync_type": "1", "rss_interval": 300}})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	post := func(id, command, token string, at time.Time, want int) {
		t.Helper()
		v := url.Values{"post_id": {id}, "text": {command}, "token": {token}, "user_id": {"fixture-user"}, "username": {"User"}, "timestamp": {strconv.FormatInt(at.Unix(), 10)}}
		r := httptest.NewRequest("POST", "/synology?apikey=native-site-login-fixture", strings.NewReader(v.Encode()))
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: got %d, want %d: %s", command, w.Code, want, w.Body.String())
		}
	}
	post("0", "/db", "fixture-token", time.Now(), 409)
	save(true)
	post("0", "/db", "wrong", time.Now(), 403)
	post("0", "/db", "fixture-token", time.Now().Add(-time.Hour), 403)
	post("0", "/pts", "fixture-token", time.Now(), 403)
	post("0", "/sta", "fixture-token", time.Now(), 403)
	post("1", "/db", "fixture-token", time.Now(), 200)
	post("1", "/db", "fixture-token", time.Now(), 200)
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check := func(query string, want int) {
		t.Helper()
		var actual int
		if err := db.QueryRow(query).Scan(&actual); err != nil || actual != want {
			t.Fatal(query, actual, want, err)
		}
	}
	check("SELECT COUNT(*) FROM GO_DOUBAN_COMMANDS WHERE PENDING=1", 1)
	check("SELECT COUNT(*) FROM GO_BUSINESS_EVENT_DATA WHERE json_extract(DATA,'$.user_id')='fixture-user'", 1)
	check("SELECT COUNT(*) FROM GO_NOTIFICATION_DELIVERIES", 1)
	if _, err = db.Exec("CREATE TRIGGER reject_fixture_reply BEFORE INSERT ON GO_NOTIFICATION_DELIVERIES BEGIN SELECT RAISE(ABORT,'fixture'); END"); err != nil {
		t.Fatal(err)
	}
	post("2", "/db", "fixture-token", time.Now(), 403)
	check("SELECT COUNT(*) FROM GO_DOUBAN_COMMANDS", 1)
	if _, err = db.Exec("DROP TRIGGER reject_fixture_reply"); err != nil {
		t.Fatal(err)
	}

	reads := 0
	restart := func() {
		var runner *rssRunAPI
		h, err := buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: f.configPath}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "www.douban.com" || r.URL.Path != "/feed/people/fixture/interests" {
				return nil, fmt.Errorf("unexpected fixture route %s", r.URL.Host)
			}
			reads++
			if reads == 1 {
				post("2", "/db", "fixture-token", time.Now(), 200)
			}
			return jsonResponse(r, "<rss><channel></channel></rss>"), nil
		}), &runner)
		if err != nil {
			t.Fatal(err)
		}
		f.handler = h
		api = runner.siteSignin.douban
		api.pause = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	tick := func() {
		t.Helper()
		if err := api.tick(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	restart()
	tick()
	check("SELECT COUNT(*) FROM GO_DOUBAN_COMMANDS WHERE PENDING=1", 1)
	restart()
	tick()
	tick()
	post("1", "/db", "fixture-token", time.Now(), 200)
	tick()
	if reads != 2 {
		t.Fatal("remote runs", reads)
	}
	check("SELECT COUNT(*) FROM GO_DOUBAN_COMMANDS WHERE PENDING=0", 2)
	check("SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='douban_sync_ack'", 2)
	post("3", "/db", "fixture-token", time.Now(), 200)
	channel.Config = `{"token":"fixture-token-2"}`
	if _, err = f.signin.channels.Save(ctx, channel); err != nil {
		t.Fatal(err)
	}
	post("4", "/db", "fixture-token-2", time.Now(), 409)
	tick()
	save(true)
	tick()
	post("1", "/db", "fixture-token-2", time.Now(), 200)
	tick()
	if reads != 2 {
		t.Fatal("changed grant replayed command")
	}
	save(false)
	post("5", "/db", "fixture-token-2", time.Now(), 409)
}

func TestDoubanSyncPollsTelegramWithOnlyRemoteSyncEnabled(t *testing.T) {
	f := newLoginFixture(t, nil)
	ctx := t.Context()
	if err := f.api.config.Update(map[string]any{"laboratory.telegram_domain": "http://telegram.fixture.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signin.channels.Save(ctx, notificationconfig.Channel{Name: "Telegram", Type: "telegram", Enabled: 1, Interactive: 1, Config: `{"token":"12345:fixture-token","admin_ids":"42","webhook":false}`, Switches: "[]"}); err != nil {
		t.Fatal(err)
	}
	const p = "/api/v1/plugin/"
	for _, v := range []struct {
		action string
		form   url.Values
	}{
		{"install", url.Values{"id": {"DoubanSync"}}},
		{"config", url.Values{"id": {"DoubanSync"}, "config": {`{"users":"fixture","types":"wish","interval":1,"go_remote":true}`}}},
	} {
		w := performFormRequest(f.handler, p+v.action, f.token, v.form)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	scope, _, err := f.signin.remoteControl(ctx)
	if err != nil || scope.Scope.PluginDigest != sitesignin.DoubanReceiver {
		t.Fatal(scope, err)
	}
	polls := 0
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "telegram.fixture.test" {
			t.Fatal(r.URL.Host)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
			return jsonResponse(r, `{"ok":true,"result":{"url":""}}`), nil
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			polls++
			return jsonResponse(r, fmt.Sprintf(`{"ok":true,"result":[{"update_id":1,"message":{"text":"/db","date":%d,"from":{"id":7}}},{"update_id":2,"message":{"text":"/db","date":%d,"from":{"id":42}}}]}`, time.Now().Unix(), time.Now().Unix())), nil
		default:
			return nil, fmt.Errorf("unexpected fixture operation")
		}
	})
	for i := 0; i < 3; i++ {
		f.signin.pollPluginReceiver(ctx, time.Now().Add(time.Duration(i)*6*time.Second))
	}
	if polls < 1 {
		t.Fatal("no polling")
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	var user string
	if err = db.QueryRow("SELECT COUNT(*),json_extract(DATA,'$.user_id') FROM GO_BUSINESS_EVENT_DATA").Scan(&n, &user); err != nil || n != 1 || user != "42" {
		t.Fatal(n, user, err)
	}
}

func TestDoubanSyncWechatMenuAndSlackMentionPublishOriginalEvents(t *testing.T) {
	f := newLoginFixture(t, nil)
	ctx := t.Context()
	key := bytes.Repeat([]byte("k"), 32)
	options, _ := json.Marshal(map[string]string{"corpid": "fixture-corp", "token": "fixture-wechat", "encodingAESKey": base64.RawStdEncoding.EncodeToString(key), "adminUser": "owner"})
	for _, channel := range []notificationconfig.Channel{
		{Name: "WeCom", Type: "wechat", Enabled: 1, Interactive: 1, Config: string(options), Switches: "[]"},
		{Name: "Slack", Type: "slack", Enabled: 1, Interactive: 1, Config: `{"app_token":"fixture-app","bot_token":"fixture-bot"}`, Switches: "[]"},
	} {
		if _, err := f.signin.channels.Save(ctx, channel); err != nil {
			t.Fatal(err)
		}
	}
	s := f.signin.system
	settings := map[string]any{"webhook_url": "https://external.fixture.test/events", "method": "post", "enable": true}
	hook := bus.EventWebhookControl{Epoch: strings.Repeat("2", 32), Actor: "admin", Authority: bus.Hash("fixture"), ConfigDigest: bus.Hash(settings), Created: time.Now().UTC().Format(time.RFC3339Nano), Authorized: true}
	for k, v := range map[string]any{"UserInstalledPlugins": []string{"DoubanSync", bus.EventWebhookPlugin}, bus.EventWebhookKey: settings, bus.EventWebhookControlKey: hook} {
		b, _ := json.Marshal(v)
		if err := s.Set(ctx, k, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	w := performFormRequest(f.handler, "/api/v1/plugin/config", f.token, url.Values{"id": {"DoubanSync"}, "config": {`{"users":["fixture"],"types":["wish"],"interval":2,"go_remote":true}`}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	check := func(path, body string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:5151"
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	wechat := func(user string, want int) {
		t.Helper()
		doc := "<xml><ToUserName>fixture-corp</ToUserName><FromUserName>" + user + "</FromUserName><CreateTime>" + stamp + "</CreateTime><MsgType>event</MsgType><AgentID>5</AgentID><Event>click</Event><EventKey>fixture#menu#_1_1</EventKey></xml>"
		raw := make([]byte, 20)
		binary.BigEndian.PutUint32(raw[16:], uint32(len(doc)))
		raw = append(raw, doc...)
		raw = append(raw, "fixture-corp"...)
		padding := 32 - len(raw)%32
		raw = append(raw, bytes.Repeat([]byte{byte(padding)}, padding)...)
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(raw, raw)
		encoded := base64.StdEncoding.EncodeToString(raw)
		parts := []string{"fixture-wechat", stamp, "nonce", encoded}
		sort.Strings(parts)
		query := url.Values{"timestamp": {stamp}, "nonce": {"nonce"}, "msg_signature": {fmt.Sprintf("%x", sha1.Sum([]byte(strings.Join(parts, ""))))}}
		check("/wechat?"+query.Encode(), "<xml><Encrypt>"+encoded+"</Encrypt></xml>", want)
	}
	wechat("guest", 403)
	wechat("owner", 200)
	wechat("owner", 200)
	for i := 0; i < 2; i++ {
		check("/slack?apikey=native-site-login-fixture", fmt.Sprintf(`{"type":"event_callback","event_id":"fixture-slack-id","event":{"text":"<@U1234> /db","user":"U9876","event_ts":"%s.0"}}`, stamp), 200)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		"SELECT COUNT(*) FROM GO_DOUBAN_COMMANDS WHERE PENDING=1",
		"SELECT COUNT(*) FROM GO_NOTIFICATION_DELIVERIES",
		"SELECT COUNT(*) FROM GO_EVENT_WEBHOOK_OUTBOX WHERE TYPE='douban.sync' AND json_extract(DOCUMENT,'$.envelope.data')='{}'",
		"SELECT COUNT(*) FROM GO_EVENT_WEBHOOK_OUTBOX WHERE TYPE='message.incoming' AND json_extract(DOCUMENT,'$.envelope.data.message')='/db'",
	} {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil || n != 2 {
			t.Fatal(query, n, err)
		}
	}
}
