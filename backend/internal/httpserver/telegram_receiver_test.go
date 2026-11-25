package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramSharedReceiverLockOffsetAndDispatchOwnership(t *testing.T) {
	f, api, c := signinRemoteFixture(t, false)
	ch, err := api.channels.TelegramReceiver(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sources, _, err := api.remoteSources(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	source := sources[0]
	bot := telegramBotID(text(source.config["token"]))
	if err = api.channels.SaveTelegramOffset(t.Context(), bot, 72); err != nil {
		t.Fatal(err)
	}
	guard, err := holdTelegramReceiver(t.Context(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	f.api.transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if strings.HasSuffix(req.URL.Path, "getWebhookInfo") {
			return jsonResponse(req, `{"ok":true,"result":{"url":""}}`), nil
		}
		if req.URL.Query().Get("offset") != "72" {
			t.Errorf("unexpected offset: %v", req.URL.Query())
		}
		return jsonResponse(req, `{"ok":true,"result":[]}`), nil
	})
	if err = api.pollTelegram(t.Context(), c, time.Now()); err != nil || calls != 0 {
		guard()
		t.Fatalf("peer polled: %d, %v", calls, err)
	}
	guard()
	if err = api.pollTelegram(t.Context(), c, time.Now()); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	message := func(id int64) []byte {
		raw, e := json.Marshal(map[string]any{"update_id": id, "message": map[string]any{"date": time.Now().Unix(), "from": map[string]int{"id": 42}, "text": "/pts"}})
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	if yes, e := api.channels.ClaimTelegramUpdate(t.Context(), ch, bot, 72); e != nil || !yes {
		t.Fatal(yes, e)
	}
	if err = api.telegramUpdate(t.Context(), c, source, message(72)); err != nil {
		t.Fatal(err)
	}
	signals, e := api.store.Signals(t.Context(), true)
	if e != nil || len(signals) != 0 {
		t.Fatal(signals, e)
	}
	if err = api.telegramUpdate(t.Context(), c, source, message(73)); err != nil {
		t.Fatal(err)
	}
	if yes, e := api.channels.ClaimTelegramUpdate(t.Context(), ch, bot, 73); e != nil || yes {
		t.Fatal(yes, e)
	}
	signals, e = api.store.Signals(t.Context(), true)
	if e != nil || len(signals) != 1 {
		t.Fatal(signals, e)
	}
}

func TestTelegramPairedWebhookQueueAndModeReconciliation(t *testing.T) {
	f, api, _ := signinRemoteFixture(t, true)
	var writes atomic.Int32
	var want, registered atomic.Value
	registered.Store("")
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getWebhookInfo"):
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]string{"url": registered.Load().(string)}})
		case strings.HasSuffix(r.URL.Path, "setWebhook"):
			writes.Add(1)
			if r.Method != "POST" || r.FormValue("drop_pending_updates") != "" || r.FormValue("allowed_updates") != `["message"]` || r.FormValue("url") != want.Load().(string) {
				t.Error("incorrect webhook setup")
			}
			// Incomplete response leaves the outcome unknown.
			io.WriteString(w, `{"ok":`)
		default:
			t.Errorf("unexpected bot operation %s", r.URL.Path)
		}
	}))
	defer bot.Close()
	if e := f.api.config.Update(map[string]any{"laboratory.telegram_domain": bot.URL}); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	r := telegramReceiver{settings: f.api.config, store: api.channels, out: json.NewEncoder(&out)}
	s, e := r.snapshot(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	want.Store(s.source.callback)
	if e = r.reconcile(t.Context(), s); e == nil {
		t.Fatal("accepted missing receipt")
	}
	if e = r.reconcile(t.Context(), s); e == nil || writes.Load() != 1 {
		t.Fatal("repeated unresolved request", writes.Load(), e)
	}
	registered.Store(want.Load().(string))
	if e = r.reconcile(t.Context(), s); e != nil || writes.Load() != 1 {
		t.Fatal(writes.Load(), e)
	}
	u, e := url.Parse(want.Load().(string))
	if e != nil {
		t.Fatal(e)
	}
	call := func(key string, id int) int {
		body := `{"update_id":` + strconv.Itoa(id) + `,"message":{"text":"hello","from":{"id":42,"username":"fixture"}}}`
		req := httptest.NewRequest("POST", "/telegram?apikey="+key, strings.NewReader(body))
		w := httptest.NewRecorder()
		r.webhook(w, req)
		return w.Code
	}
	if call("wrong", 1) != 401 || call(u.Query().Get("apikey"), 1) != 200 || call(u.Query().Get("apikey"), 1) != 200 {
		t.Fatal("callback response")
	}
	if e = r.drain(t.Context(), s); e != nil {
		t.Fatal(e)
	}
	var d telegramDispatch
	if e = json.Unmarshal(out.Bytes(), &d); e != nil || d.Text != "hello" || d.Action != "dispatch" || d.UserID != 42 {
		t.Fatal(d, e)
	}
	out.Reset()
	if call(u.Query().Get("apikey"), 1) != 200 {
		t.Fatal("duplicate callback response")
	}
	if e = r.drain(t.Context(), s); e != nil || out.Len() != 0 {
		t.Fatal("duplicate delivered", e)
	}
}
