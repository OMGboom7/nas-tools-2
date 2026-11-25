package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/notificationconfig"
)

func TestTelegramCallbackUsesCurrentChannelAndHandsOffOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("security: {telegram_webhook_allow_ip: {ipv4: 127.0.0.1}}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := notificationconfig.Open(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ch := notificationconfig.Channel{Type: "telegram", Name: "fixture", Enabled: 1, Interactive: 1, Config: `{"token":"24:fixture","admin_ids":"9007199254740993","user_ids":"43"}`}
	ch.ID, err = store.Save(t.Context(), ch)
	if err != nil {
		t.Fatal(err)
	}
	call := func(peer string, id, user int64, body string) (telegramDispatch, error) {
		payload := fmt.Sprintf(`{"update_id":%d,"message":{"from":{"id":%d,"username":"name"},"text":%q}}`, id, user, body)
		var out bytes.Buffer
		err := TelegramIncomingCommand(context.Background(), path, strconv.FormatInt(ch.ID, 10), peer, strings.NewReader(payload), &out)
		var result telegramDispatch
		if err == nil {
			err = json.Unmarshal(out.Bytes(), &result)
		}
		return result, err
	}
	const admin int64 = 9007199254740993
	for _, tc := range []struct {
		peer         string
		id, user     int64
		body, action string
	}{
		{"192.0.2.1", 1, admin, "/sta", "forbidden"},
		{"127.0.0.1", 1, admin, "/sta", "dispatch"},
		{"::ffff:127.0.0.1", 1, admin, "/sta", "ignore"},
		{"127.0.0.1", 2, 43, "/sta", "deny"},
		{"127.0.0.1", 3, 43, "查询", "dispatch"},
		{"127.0.0.1", 4, 44, "查询", "deny"},
	} {
		result, err := call(tc.peer, tc.id, tc.user, tc.body)
		if err != nil || result.Action != tc.action {
			t.Fatalf("id %d: result=%+v err=%v", tc.id, result, err)
		}
		if result.Action == "dispatch" && (result.UserID != tc.user || result.Text != tc.body || result.Name != "name") {
			t.Fatal("changed sender or message")
		}
	}
	type outcome struct {
		result telegramDispatch
		err    error
	}
	results := make(chan outcome, 6)
	for range cap(results) {
		go func() { v, e := call("127.0.0.1", 5, admin, "/pts"); results <- outcome{v, e} }()
	}
	n := 0
	for range cap(results) {
		v := <-results
		if v.err != nil {
			t.Fatal(v.err)
		}
		switch v.result.Action {
		case "dispatch":
			n++
		case "ignore":
		default:
			t.Fatal(v.result)
		}
	}
	if n != 1 {
		t.Fatalf("received %d handoffs", n)
	}
	if err = os.WriteFile(path, []byte("security: {telegram_webhook_allow_ip: invalid}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if v, e := call("127.0.0.1", 6, admin, "/pts"); e != nil || v.Action != "forbidden" {
		t.Fatal(v, e)
	}
	if err = os.WriteFile(path, []byte("security: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := ch
	ch.Config = `{"token":"24:rotated","admin_ids":"43"}`
	if _, err = store.Save(t.Context(), ch); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ClaimTelegramUpdate(t.Context(), old, "24", 6); err == nil {
		t.Fatal("accepted stale channel")
	}
	if v, e := call("127.0.0.1", 5, admin, "/pts"); e != nil || v.Action != "ignore" {
		t.Fatal(v, e)
	}
	if v, e := call("127.0.0.1", 6, admin, "/pts"); e != nil || v.Action != "deny" {
		t.Fatal(v, e)
	}
	ch.Enabled = 0
	if _, err = store.Save(t.Context(), ch); err != nil {
		t.Fatal(err)
	}
	if _, e := call("127.0.0.1", 7, 43, "/pts"); e == nil {
		t.Fatal("accepted disabled channel")
	}
}
