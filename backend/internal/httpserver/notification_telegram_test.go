package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTelegramCommandMediaList(t *testing.T) {
	t.Parallel()
	received := make(chan string, 3)
	bot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/local/botfixture/sendMessage" || q.Get("chat_id") != "-9007199254740993" || q.Get("message_thread_id") != "87" || q.Get("parse_mode") != "Markdown" {
			t.Errorf("unexpected delivery fields: %s %v", r.URL.Path, q)
		}
		received <- q.Get("text")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer bot.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("laboratory:\n  telegram_domain: "+bot.URL+"/local/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	q := telegramMessage{Token: "fixture", ChatID: "-9007199254740993", ThreadID: "87", Title: "选择", Medias: []telegramMedia{{Title: "电影", URL: "film", Type: "类型：电影", Vote: "评分：8.5"}, {Title: "剧集", URL: "tv", Type: "类型：剧集"}}}
	send := func() error {
		raw, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		return SendTelegramCommand(context.Background(), configPath, "list", strings.NewReader(string(raw)))
	}
	if err := send(); err != nil {
		t.Fatal(err)
	}
	q.Medias = []telegramMedia{}
	if err := send(); err != nil {
		t.Fatal(err)
	}
	q.Medias = nil
	if err := send(); err == nil {
		t.Fatal("accepted absent list")
	}
	if len(received) != 2 || <-received != "*选择*\n1. [电影](film)\n类型：电影，评分：8.5\n2. [剧集](tv)\n类型：剧集" || <-received != "*选择*" {
		t.Fatal("unexpected Telegram list delivery")
	}
}

func TestTelegramUnconfirmedPhotoStopsFallback(t *testing.T) {
	t.Parallel()
	for _, lostAt := range []string{"initial photo", "uploaded photo", "malformed receipt", "gateway failure"} {
		t.Run(lostAt, func(t *testing.T) {
			var calls []string
			service := notificationService{client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch lostAt {
				case "malformed receipt":
					return jsonResponse(r, `{"ok":false}trailing`), nil
				case "gateway failure":
					result := jsonResponse(r, `{"ok":false}`)
					result.StatusCode = http.StatusBadGateway
					return result, nil
				}
				if lostAt == "uploaded photo" {
					if r.URL.Host == "image.example" {
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("image"))}, nil
					}
					if r.Method == "GET" {
						return jsonResponse(r, `{"ok":false}`), nil
					}
				}
				return nil, errors.New("connection closed")
			})}}
			sent, err := service.deliverTelegram(context.Background(), "fixture", "-10", "4", "title", "https://image.example/poster")
			want := "GET /botfixture/sendPhoto"
			if lostAt == "uploaded photo" {
				want += ";GET /poster;POST /botfixture/sendPhoto"
			}
			if sent || err == nil || strings.Join(calls, ";") != want {
				t.Fatalf("sent=%v err=%v calls=%v", sent, err, calls)
			}
		})
	}
}
