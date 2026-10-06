package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

const legacyNotificationList = `{"code":0,"detail":{"12":{"id":12,"name":"家庭通知","type":"telegram","config":{"token":"bot-secret","chat_id":"123456","interactive":1},"switchs":["download_start","transfer_finished"],"interactive":1,"enabled":1},"15":{"id":15,"name":"备用推送","type":"bark","config":{"server":"https://push.example","apikey":"device-secret","interactive":0},"switchs":"[\"download_fail\"]","interactive":0,"enabled":0}}}`

const legacyNotificationOptions = `{"code":0,"channels":[{"type":"telegram","name":"Telegram","can_interact":true,"fields":[{"key":"token","title":"Bot Token","type":"text","required":true,"tooltip":"","placeholder":"","default":null,"options":[],"write_only":true},{"key":"chat_id","title":"Chat ID","type":"text","required":true,"tooltip":"","placeholder":"","default":null,"options":[],"write_only":true},{"key":"webhook","title":"Webhook","type":"switch","required":false,"tooltip":"","placeholder":"","default":null,"options":[],"write_only":false}]},{"type":"webhook","name":"Webhook","can_interact":false,"fields":[{"key":"url","title":"URL","type":"text","required":true,"tooltip":"","placeholder":"","default":null,"options":[],"write_only":true},{"key":"method","title":"HTTP方法","type":"select","required":true,"tooltip":"","placeholder":"","default":"POST","options":[{"value":"GET","label":"GET"},{"value":"POST","label":"POST"}],"write_only":false},{"key":"token","title":"Token","type":"text","required":false,"tooltip":"","placeholder":"","default":null,"options":[],"write_only":true}]}],"events":[{"id":"download_start","label":"新增下载"},{"id":"download_fail","label":"下载失败"}]}`

func TestNativeNotificationReadsDatabaseWithoutLegacyInfo(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (20, '自动化', 'webhook', ?, '["download_start"]', 0, 1)`, `{"url":"https://secret.example/hook","method":"POST","token":"hook-secret"}`)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/v1/notifications", "/api/v1/notifications/20"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", path, response.Code, response.Body.String())
		}
		for _, secret := range []string{"secret.example", "hook-secret"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s leaked %q: %s", path, secret, response.Body.String())
			}
		}
		if path == "/api/v1/notifications" {
			var result struct {
				Data notificationsData `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Data.Items) != 1 || result.Data.Items[0].ID != "20" || !result.Data.Items[0].Configured {
				t.Fatalf("unexpected native channels: %#v", result.Data.Items)
			}
			continue
		}
		var result struct {
			Data notificationDetail `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Data.ID != "20" || result.Data.Config["method"] != "POST" || len(result.Data.ConfiguredFields) != 3 {
			t.Fatalf("unexpected native detail: %#v", result.Data)
		}
	}
	optionsRequest := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/options", nil)
	optionsRequest.Header.Set("Authorization", token)
	optionsResponse := httptest.NewRecorder()
	handler.ServeHTTP(optionsResponse, optionsRequest)
	var optionsResult struct {
		Data notificationOptionsData `json:"data"`
	}
	if optionsResponse.Code != http.StatusOK || json.Unmarshal(optionsResponse.Body.Bytes(), &optionsResult) != nil {
		t.Fatalf("native options: status = %d: %s", optionsResponse.Code, optionsResponse.Body.String())
	}
	if len(optionsResult.Data.Channels) != 13 || len(optionsResult.Data.Events) != 14 {
		t.Fatalf("unexpected native options: %#v", optionsResult.Data)
	}
	legacyInfo := func(cid, authorization string) *httptest.ResponseRecorder {
		form := url.Values{}
		if cid != "" {
			form.Set("cid", cid)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/message/client/info", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := legacyInfo("20", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated legacy info status = %d", response.Code)
	}
	for _, cid := range []string{"", "20"} {
		response := legacyInfo(cid, token)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "hook-secret") || !strings.Contains(response.Body.String(), `"interactive":0`) {
			t.Fatalf("legacy info cid=%q: status = %d: %s", cid, response.Code, response.Body.String())
		}
	}
	response := legacyInfo("999", token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"detail":null`) {
		t.Fatalf("missing legacy info: status = %d: %s", response.Code, response.Body.String())
	}
	legacyOptionsRequest := httptest.NewRequest(http.MethodPost, "/api/v1/message/client/options", nil)
	legacyOptionsRequest.Header.Set("Authorization", token)
	legacyOptionsResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyOptionsResponse, legacyOptionsRequest)
	if legacyOptionsResponse.Code != http.StatusOK || !strings.Contains(legacyOptionsResponse.Body.String(), `"can_interact":true`) || !strings.Contains(legacyOptionsResponse.Body.String(), `"write_only":true`) {
		t.Fatalf("legacy options: status = %d: %s", legacyOptionsResponse.Code, legacyOptionsResponse.Body.String())
	}
}

func TestNativeServerChanTestAndCustomDelivery(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Host != "sctapi.ftqq.com" || request.URL.Path != "/SCT_SECRET_123.send" {
			t.Errorf("unexpected notification destination: %s %s", request.Method, request.URL.Host)
			return nil, nil
		}
		switch requests {
		case 1:
			if request.URL.Query().Get("title") != "测试" {
				t.Errorf("unexpected test title: %q", request.URL.Query().Get("title"))
			}
		case 2:
			if request.URL.Query().Get("title") != "今晚入库" || request.URL.Query().Get("desp") != "两部影片已完成" {
				t.Errorf("unexpected custom message query: %s", request.URL.RawQuery)
			}
		case 3:
			if request.URL.Query().Get("title") != "旧版消息" || request.URL.Query().Get("desp") != "旧版内容" {
				t.Errorf("unexpected legacy custom message query: %s", request.URL.RawQuery)
			}
		}
		return jsonResponse(request, `{"code":0,"message":"success"}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (12, 'Server酱', 'serverchan', ?, '[]', 0, 1)`, `{"sckey":"SCT_SECRET_123"}`); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/notifications/12/test", ""},
		{http.MethodPost, "/api/v1/notifications/custom-message", `{"title":"今晚入库","text":"两部影片已完成","channelIds":["12"]}`},
	} {
		request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"success":true`) || strings.Contains(response.Body.String(), "SCT_SECRET_123") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	legacyRequest := httptest.NewRequest(http.MethodPost, "/api/v1/message/custom/send", strings.NewReader(url.Values{"title": {"旧版消息"}, "text": {"旧版内容"}, "message_clients": {"12"}}.Encode()))
	legacyRequest.Header.Set("Authorization", token)
	legacyRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	legacyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyResponse, legacyRequest)
	if legacyResponse.Code != http.StatusOK || !strings.Contains(legacyResponse.Body.String(), `"code":0`) {
		t.Fatalf("legacy custom send = %d: %s", legacyResponse.Code, legacyResponse.Body.String())
	}
	if requests != 3 {
		t.Fatalf("expected three native sends, got %d", requests)
	}
}

func TestNativeNtfyAndGotifyDelivery(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" || request.URL.Host == "legacy:3000" {
			t.Errorf("unexpected notification request: %s %s", request.Method, request.URL.Host)
			return nil, nil
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch request.URL.Host {
		case "ntfy.example":
			if request.URL.Path != "/" || request.Header.Get("Authorization") != "Bearer ntfy-secret" || body["topic"] != "movies" || body["priority"] != float64(4) {
				t.Errorf("unexpected ntfy request: %#v", body)
			}
		case "gotify.example":
			if request.URL.Path != "/message" || request.URL.Query().Get("token") != "gotify-secret" || body["priority"] != float64(8) {
				t.Errorf("unexpected gotify request: %#v", body)
			}
		default:
			t.Errorf("unexpected notification host: %s", request.URL.Host)
		}
		return jsonResponse(request, `{}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, item := range []struct {
		id                        int
		name, channelType, config string
	}{
		{21, "ntfy", "ntfy", `{"server":"https://ntfy.example/path","token":"ntfy-secret","topic":"movies","tags":""}`},
		{22, "Gotify", "gotify", `{"server":"https://gotify.example/path","token":"gotify-secret"}`},
	} {
		if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (?, ?, ?, ?, '[]', 0, 1)`, item.id, item.name, item.channelType, item.config); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/v1/notifications/21/test", "/api/v1/notifications/22/test", "/api/v1/notifications/custom-message"} {
		body := ""
		if strings.HasSuffix(path, "custom-message") {
			body = `{"title":"今晚入库","text":"两部影片已完成","channelIds":["21","22"]}`
		}
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "ntfy-secret") || strings.Contains(response.Body.String(), "gotify-secret") {
			t.Fatalf("%s: status = %d: %s", path, response.Code, response.Body.String())
		}
	}
	if requests != 4 {
		t.Fatalf("expected four native sends, got %d", requests)
	}
}

func TestLegacyNotificationTestUsesGoDelivery(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Host != "sctapi.ftqq.com" || request.URL.Query().Get("title") != "测试" || request.URL.Query().Get("desp") != "这是一条测试消息" {
			t.Errorf("unexpected notification request: %s", request.URL.String())
		}
		return jsonResponse(request, `{"code":0}`), nil
	})
	handler, token, _ := nativeServicesFixture(t, "", nil, transport)
	call := func(auth string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/message/client/test", strings.NewReader(form.Encode()))
		request.Header.Set("Authorization", auth)
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	result := call(token, url.Values{"type": {"serverchan"}, "config": {`{"sckey":"SCT_SECRET_123"}`}})
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"code":0`) || strings.Contains(result.Body.String(), "SCT_SECRET_123") || requests != 1 {
		t.Fatalf("native old test = %d, requests = %d: %s", result.Code, requests, result.Body.String())
	}
	unsupported := call(token, url.Values{"type": {"webhook"}, "config": {`{"url":"https://example.org/hook","json_tpl":"{{ title }}"}`}})
	if unsupported.Code != http.StatusNotImplemented || requests != 1 {
		t.Fatalf("templated webhook = %d, requests = %d: %s", unsupported.Code, requests, unsupported.Body.String())
	}
	unauthorized := call("invalid", url.Values{"type": {"serverchan"}, "config": {`{"sckey":"SCT_SECRET_123"}`}})
	if unauthorized.Code != http.StatusUnauthorized || requests != 1 {
		t.Fatalf("invalid token = %d, requests = %d: %s", unauthorized.Code, requests, unauthorized.Body.String())
	}
}

func TestNativeNotificationDeliveryRejectsRedirectAndUnsafeKey(t *testing.T) {
	t.Parallel()
	requests := 0
	service := notificationService{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"https://other.example/steal"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}, nil
	})}}
	sent, err := service.sendNativeNotification(context.Background(), "ntfy", map[string]any{
		"server": "https://ntfy.example", "token": "private-token", "topic": "movies",
	}, "title", "message", "", "")
	if err != nil || sent || requests != 1 {
		t.Fatalf("redirect followed or considered success: sent=%t err=%v requests=%d", sent, err, requests)
	}
	sent, err = service.sendNativeNotification(context.Background(), "serverchan", map[string]any{
		"sckey": "key/../steal",
	}, "title", "message", "", "")
	if err == nil || sent || requests != 1 || strings.Contains(err.Error(), "key/../steal") {
		t.Fatalf("unsafe key was accepted or disclosed: sent=%t err=%v requests=%d", sent, err, requests)
	}
}

func TestNativeBarkAndChanifyDelivery(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodPost || request.URL.Host == "legacy:3000" {
			t.Errorf("unexpected notification destination: %s %s", request.Method, request.URL.Host)
			return nil, nil
		}
		switch request.URL.Host {
		case "bark.example":
			if !strings.HasPrefix(request.URL.EscapedPath(), "/BARK-KEY/") || request.URL.Query().Get("sound") != "bell" {
				t.Errorf("unexpected Bark request: %s", request.URL.String())
			}
			return jsonResponse(request, `{"code":200,"message":"success"}`), nil
		case "chanify.example":
			payload, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			form, err := url.ParseQuery(string(payload))
			if err != nil || request.URL.Path != "/v1/sender/CHANIFY-TOKEN" || form.Get("sound") != "0" || form.Get("title") == "" || form.Get("text") == "" {
				t.Errorf("unexpected Chanify request: path=%s form=%v err=%v", request.URL.Path, form, err)
			}
			return jsonResponse(request, `{}`), nil
		default:
			t.Errorf("unexpected notification host: %s", request.URL.Host)
			return nil, nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, item := range []struct {
		id                        int
		name, channelType, config string
	}{
		{30, "Bark", "bark", `{"server":"https://bark.example/ignored","apikey":"BARK-KEY","params":"sound=bell"}`},
		{31, "Chanify", "chanify", `{"server":"https://chanify.example/ignored","token":"CHANIFY-TOKEN","params":"sound=0"}`},
	} {
		if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (?, ?, ?, ?, '[]', 0, 1)`, item.id, item.name, item.channelType, item.config); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/notifications/30/test", ""},
		{"/api/v1/notifications/31/test", ""},
		{"/api/v1/notifications/custom-message", `{"title":"Hello world","text":"Body text","channelIds":["30","31"]}`},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "BARK-KEY") || strings.Contains(response.Body.String(), "CHANIFY-TOKEN") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	if requests != 4 {
		t.Fatalf("expected four native sends, got %d", requests)
	}
}

func TestNativeIyuuAndPushPlusDelivery(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Scheme != "http" {
			t.Errorf("unexpected notification request: %s %s", request.Method, request.URL.String())
			return nil, nil
		}
		switch request.URL.Host {
		case "iyuu.cn":
			if request.URL.Path != "/IYUU-TOKEN.send" || request.URL.Query().Get("text") == "" {
				t.Errorf("unexpected Iyuu request: %s", request.URL.String())
			}
			return jsonResponse(request, `{"errcode":0,"errmsg":"ok"}`), nil
		case "www.pushplus.plus":
			query := request.URL.Query()
			if request.URL.Path != "/send" || query.Get("token") != "PUSHPLUS-TOKEN" || query.Get("channel") != "wechat" || query.Get("title") == "" || query.Get("content") == "" || query.Get("timestamp") == "" {
				t.Errorf("unexpected PushPlus request: %s", request.URL.String())
			}
			return jsonResponse(request, `{"code":200,"msg":"ok"}`), nil
		default:
			t.Errorf("unexpected notification host: %s", request.URL.Host)
			return nil, nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, item := range []struct {
		id                        int
		name, channelType, config string
	}{
		{40, "爱语飞飞", "iyuu", `{"token":"IYUU-TOKEN"}`},
		{41, "PushPlus", "pushplus", `{"token":"PUSHPLUS-TOKEN","channel":"wechat"}`},
	} {
		if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (?, ?, ?, ?, '[]', 0, 1)`, item.id, item.name, item.channelType, item.config); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/notifications/40/test", ""},
		{"/api/v1/notifications/41/test", ""},
		{"/api/v1/notifications/custom-message", `{"title":"今晚入库","text":"两部影片已完成","channelIds":["40","41"]}`},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "IYUU-TOKEN") || strings.Contains(response.Body.String(), "PUSHPLUS-TOKEN") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	if requests != 4 {
		t.Fatalf("expected four native sends, got %d", requests)
	}
}

func TestNativePushDeerDelivery(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil || request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Host != "pushdeer.example" || request.URL.Path != "/message/push" || form.Get("pushkey") != "PUSHDEER-KEY" || form.Get("type") != "markdown" || form.Get("text") == "" || form.Get("desp") == "" {
			t.Errorf("unexpected PushDeer request: %s %s form=%v err=%v", request.Method, request.URL.String(), form, err)
		}
		if requests == 3 {
			return jsonResponse(request, `{"code":1,"error":"rejected"}`), nil
		}
		return jsonResponse(request, `{"code":0,"content":{"result":1}}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (50, 'PushDeer', 'pushdeer', ?, '[]', 0, 1)`, `{"server":"https://pushdeer.example/ignored","apikey":"PUSHDEER-KEY"}`); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/notifications/50/test", ""},
		{"/api/v1/notifications/custom-message", `{"title":"今晚入库","text":"两部影片已完成","channelIds":["50"]}`},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "PUSHDEER-KEY") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	failureRequest := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/50/test", nil)
	failureRequest.Header.Set("Authorization", token)
	failureResponse := httptest.NewRecorder()
	handler.ServeHTTP(failureResponse, failureRequest)
	if failureResponse.Code != http.StatusOK || !strings.Contains(failureResponse.Body.String(), `"ok":false`) || strings.Contains(failureResponse.Body.String(), "PUSHDEER-KEY") {
		t.Fatalf("PushDeer failure: status = %d: %s", failureResponse.Code, failureResponse.Body.String())
	}
	if requests != 3 {
		t.Fatalf("expected three native sends, got %d", requests)
	}
}

func TestNativeTelegramTextAndImageURLDelivery(t *testing.T) {
	t.Parallel()
	nativeRequests, legacyRequests := 0, 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "legacy:3000" {
			legacyRequests++
			if request.URL.Path != "/api/v1/message/custom/send" {
				t.Errorf("unexpected legacy notification call: %s", request.URL.Path)
			}
			return jsonResponse(request, `{"code":0}`), nil
		}
		nativeRequests++
		expectedPath := "/custom/bot123:ABC_SECRET/sendMessage"
		if nativeRequests == 3 {
			expectedPath = "/custom/bot123:ABC_SECRET/sendPhoto"
		}
		if request.Method != http.MethodGet || request.URL.Host != "telegram.example" || request.URL.Path != expectedPath || request.URL.Query().Get("chat_id") != "-100" || request.URL.Query().Get("parse_mode") != "Markdown" || request.URL.Query().Get("message_thread_id") != "42" {
			t.Errorf("unexpected Telegram request: %s %s", request.Method, request.URL.String())
		}
		if nativeRequests == 2 && request.URL.Query().Get("text") != "*今晚入库*\n两部\\_\\[x]" {
			t.Errorf("unexpected Telegram caption: %q", request.URL.Query().Get("text"))
		}
		if nativeRequests == 3 && request.URL.Query().Get("photo") != "https://img.example/poster.jpg" {
			t.Errorf("unexpected Telegram photo: %s", request.URL.String())
		}
		return jsonResponse(request, `{"ok":true,"result":{"message_id":1}}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "laboratory:\n  telegram_domain: https://telegram.example/custom\n", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (60, 'Telegram', 'telegram', ?, '[]', 0, 1)`, `{"token":"123:ABC_SECRET","chat_id":"-100","thread_id":"42"}`); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/notifications/60/test", ""},
		{"/api/v1/notifications/custom-message", `{"title":"今晚入库","text":"两部_[x]","channelIds":["60"]}`},
		{"/api/v1/notifications/custom-message", `{"title":"图片通知","text":"内容","image":"https://img.example/poster.jpg","channelIds":["60"]}`},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "ABC_SECRET") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	if nativeRequests != 3 || legacyRequests != 0 {
		t.Fatalf("unexpected Telegram routing: native=%d legacy=%d", nativeRequests, legacyRequests)
	}
}

func TestNativeTelegramImageUploadAndTextFallback(t *testing.T) {
	t.Parallel()
	config := map[string]any{"token": "123:ABC_SECRET", "chat_id": "-100"}
	for _, testCase := range []struct {
		name, imageStatus string
		wantUpload        bool
	}{
		{"upload image after Telegram rejects URL", "ok", true},
		{"send text when image cannot be fetched", "missing", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			requests := []string{}
			service := notificationService{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests = append(requests, request.Method+" "+request.URL.Host+request.URL.Path)
				if request.URL.Host == "img.example" {
					if testCase.imageStatus == "missing" {
						return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: request}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("photo-bytes")), Header: make(http.Header), Request: request}, nil
				}
				if request.URL.Host != "api.telegram.org" {
					t.Errorf("unexpected host: %s", request.URL.Host)
				}
				switch request.URL.Path {
				case "/bot123:ABC_SECRET/sendPhoto":
					if request.Method == http.MethodPost {
						body, err := io.ReadAll(request.Body)
						if err != nil || !strings.HasPrefix(request.Header.Get("Content-Type"), "multipart/form-data;") || !strings.Contains(string(body), "photo-bytes") {
							t.Errorf("invalid Telegram photo upload: %v", err)
						}
						return jsonResponse(request, `{"ok":true}`), nil
					}
					return jsonResponse(request, `{"ok":false}`), nil
				case "/bot123:ABC_SECRET/sendMessage":
					return jsonResponse(request, `{"ok":true}`), nil
				default:
					t.Errorf("unexpected Telegram path: %s", request.URL.Path)
					return nil, nil
				}
			})}}
			sent, err := service.sendNativeNotification(context.Background(), "telegram", config, "Title", "Body", "https://img.example/photo.jpg", "")
			if err != nil || !sent || len(requests) != 3 {
				t.Fatalf("Telegram image fallback failed: sent=%t err=%v requests=%v", sent, err, requests)
			}
			last := requests[2]
			if testCase.wantUpload && !strings.HasPrefix(last, "POST api.telegram.org/bot123:ABC_SECRET/sendPhoto") || !testCase.wantUpload && !strings.HasPrefix(last, "GET api.telegram.org/bot123:ABC_SECRET/sendMessage") {
				t.Fatalf("unexpected final Telegram request: %s", last)
			}
		})
	}
}

func TestNativeSynologyChatBroadcast(t *testing.T) {
	t.Parallel()
	listRequests, sendRequests := 0, 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "syno.example" || request.URL.Path != "/webapi/entry.cgi" {
			t.Errorf("unexpected Synology destination: %s", request.URL.String())
			return nil, nil
		}
		if request.Method == http.MethodGet {
			listRequests++
			if request.URL.Query().Get("method") != "user_list" || request.URL.Query().Get("token") != "syno-secret" {
				t.Errorf("unexpected user list query: %s", request.URL.RawQuery)
			}
			return jsonResponse(request, `{"data":{"users":[{"user_id":1},{"user_id":2}]}}`), nil
		}
		sendRequests++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		form, err := url.ParseQuery(string(body))
		var payload struct {
			Text    string  `json:"text"`
			UserIDs []int64 `json:"user_ids"`
		}
		if err != nil || json.Unmarshal([]byte(form.Get("payload")), &payload) != nil || request.URL.Query().Get("method") != "incoming" || len(payload.UserIDs) != 1 || payload.Text == "" || !strings.Contains(payload.Text, "%") {
			t.Errorf("unexpected Synology payload: %s form=%v", request.URL.String(), form)
		}
		return jsonResponse(request, `{"success":true}`), nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (70, 'Synology Chat', 'synologychat', ?, '[]', 0, 1)`, `{"webhook_url":"https://syno.example/webapi/entry.cgi?api=SYNO.Chat.External&method=incoming&token=syno-secret","token":"syno-secret"}`); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/notifications/70/test", ""},
		{"/api/v1/notifications/custom-message", `{"title":"今晚入库","text":"两部影片已完成","channelIds":["70"]}`},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "syno-secret") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	if listRequests != 2 || sendRequests != 4 {
		t.Fatalf("unexpected Synology requests: list=%d send=%d", listRequests, sendRequests)
	}
}

func TestNativeSynologyChatStopsOnRecipientFailure(t *testing.T) {
	t.Parallel()
	sends := 0
	service := notificationService{client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			return jsonResponse(request, `{"data":{"users":[{"user_id":1},{"user_id":2}]}}`), nil
		}
		sends++
		return jsonResponse(request, `{"error":{"code":123,"errors":"rejected"}}`), nil
	})}}
	sent, err := service.sendNativeNotification(context.Background(), "synologychat", map[string]any{
		"webhook_url": "https://syno.example/webapi/entry.cgi?token=secret", "token": "secret",
	}, "Title", "Body", "", "")
	if err != nil || sent || sends != 1 {
		t.Fatalf("Synology failure did not stop broadcast: sent=%t err=%v sends=%d", sent, err, sends)
	}
}

func TestNativeWebhookDeliveryAndExplicitTemplateGap(t *testing.T) {
	t.Parallel()
	nativeRequests, legacyRequests := 0, 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "legacy:3000" {
			legacyRequests++
			if request.URL.Path != "/api/v1/message/custom/send" {
				t.Errorf("unexpected legacy request: %s", request.URL.Path)
			}
			return jsonResponse(request, `{"code":0}`), nil
		}
		nativeRequests++
		if request.Method != http.MethodPatch || request.URL.Host != "hook.example" || request.URL.Path != "/notify" || request.URL.Query().Get("existing") != "yes" || request.URL.Query().Get("env") != "prod" || len(request.URL.Query()["multi"]) != 2 || request.Header.Get("Authorization") != "hook-secret" {
			t.Errorf("unexpected Webhook request: %s %s", request.Method, request.URL.String())
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["title"] == "" || body["user_id"] != "" {
			t.Errorf("unexpected Webhook body: %#v err=%v", body, err)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: request}, nil
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, item := range []struct {
		id     int
		config string
	}{
		{80, `{"url":"https://hook.example/notify?existing=yes","method":"PATCH","token":"hook-secret","query_params":"{\"env\":\"prod\",\"multi\":[\"a\",\"b\"]}"}`},
		{81, `{"url":"https://hook.example/notify","method":"POST","json_tpl":"{\"message\": {{ title|tojson }} }"}`},
	} {
		if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (?, 'Webhook', 'webhook', ?, '[]', 0, 1)`, item.id, item.config); err != nil {
			t.Fatal(err)
		}
	}
	for _, testCase := range []struct {
		path, body string
		status     int
	}{
		{"/api/v1/notifications/80/test", "", http.StatusOK},
		{"/api/v1/notifications/custom-message", `{"title":"普通通知","text":"内容","channelIds":["80"]}`, http.StatusOK},
		{"/api/v1/notifications/81/test", "", http.StatusNotImplemented},
		{"/api/v1/notifications/custom-message", `{"title":"模板通知","text":"内容","channelIds":["81"]}`, http.StatusNotImplemented},
		{"/api/v1/notifications/custom-message", `{"title":"混合通知","text":"内容","channelIds":["80","81"]}`, http.StatusNotImplemented},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != testCase.status || strings.Contains(response.Body.String(), "hook-secret") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
	}
	if nativeRequests != 2 || legacyRequests != 0 {
		t.Fatalf("unexpected Webhook routing: native=%d legacy=%d", nativeRequests, legacyRequests)
	}
}

func TestNativeWeChatTextAndImageDelivery(t *testing.T) {
	t.Parallel()
	tokenRequests, sendRequests := 0, 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "proxy.example" {
			t.Errorf("unexpected WeChat destination: %s", request.URL.String())
			return nil, nil
		}
		switch request.URL.Path {
		case "/relay/cgi-bin/gettoken":
			tokenRequests++
			if request.Method != http.MethodGet || request.URL.Query().Get("corpid") != "corp-id" || request.URL.Query().Get("corpsecret") != "corp-secret" {
				t.Errorf("unexpected WeChat token request: %s", request.URL.String())
			}
			return jsonResponse(request, `{"errcode":0,"access_token":"wechat-access"}`), nil
		case "/relay/cgi-bin/message/send":
			sendRequests++
			if request.Method != http.MethodPost || request.URL.Query().Get("access_token") != "wechat-access" || request.Header.Get("Content-Type") != "application/json" {
				t.Errorf("unexpected WeChat send request: %s", request.URL.String())
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["touser"] != "@all" || body["agentid"] != "agent-id" {
				t.Errorf("unexpected WeChat body: %#v err=%v", body, err)
			}
			if sendRequests == 1 && (body["msgtype"] != "text" || !strings.Contains(text(objectValue(body["text"])["content"]), "这是一条测试消息")) {
				t.Errorf("unexpected WeChat text: %#v", body)
			}
			if sendRequests == 2 && (body["msgtype"] != "news" || len(slice(objectValue(body["news"])["articles"])) != 1) {
				t.Errorf("unexpected WeChat news: %#v", body)
			}
			if sendRequests == 3 {
				return jsonResponse(request, `{"errcode":42001,"errmsg":"expired"}`), nil
			}
			return jsonResponse(request, `{"errcode":0,"errmsg":"ok"}`), nil
		default:
			t.Errorf("unexpected WeChat path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (90, '微信', 'wechat', ?, '[]', 0, 1)`, `{"corpid":"corp-id","corpsecret":"corp-secret","agentid":"agent-id","default_proxy":"https://proxy.example/relay"}`); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/notifications/90/test", ""},
		{"/api/v1/notifications/custom-message", `{"title":"今晚入库","text":"两部影片已完成","image":"https://img.example/poster.jpg","channelIds":["90"]}`},
		{"/api/v1/notifications/90/test", ""},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "corp-secret") || strings.Contains(response.Body.String(), "wechat-access") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
		if tokenRequests == 3 && !strings.Contains(response.Body.String(), `"ok":false`) {
			t.Fatalf("WeChat error was not surfaced: %s", response.Body.String())
		}
	}
	if tokenRequests != 3 || sendRequests != 3 {
		t.Fatalf("unexpected WeChat request count: token=%d send=%d", tokenRequests, sendRequests)
	}
}

func TestNativeSlackDeliveryFindsPaginatedChannelAndChecksAPIResult(t *testing.T) {
	t.Parallel()
	listRequests, sendRequests := 0, 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "slack.com" || request.Header.Get("Authorization") != "Bearer xoxb-secret" {
			t.Errorf("unexpected Slack request: %s", request.URL.String())
			return nil, nil
		}
		switch request.URL.Path {
		case "/api/conversations.list":
			listRequests++
			if request.Method != http.MethodGet || request.URL.Query().Get("limit") != "200" {
				t.Errorf("unexpected Slack channel lookup: %s", request.URL.String())
			}
			if request.URL.Query().Get("cursor") == "" {
				return jsonResponse(request, `{"ok":true,"channels":[{"id":"C1","name":"other"}],"response_metadata":{"next_cursor":"next-page"}}`), nil
			}
			if request.URL.Query().Get("cursor") != "next-page" {
				t.Errorf("unexpected Slack cursor: %s", request.URL.String())
			}
			return jsonResponse(request, `{"ok":true,"channels":[{"id":"C2","name":"全体"}],"response_metadata":{"next_cursor":""}}`), nil
		case "/api/chat.postMessage":
			sendRequests++
			if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" {
				t.Errorf("unexpected Slack send request: %s", request.URL.String())
			}
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["channel"] != "C2" {
				t.Errorf("unexpected Slack body: %#v err=%v", body, err)
			}
			blocks := slice(body["blocks"])
			expectedTitle := "测试"
			if sendRequests == 2 {
				expectedTitle = "今晚入库"
			} else if sendRequests == 4 {
				expectedTitle = "带图消息"
			}
			if len(blocks) == 0 || !strings.Contains(text(objectValue(objectValue(blocks[0])["text"])["text"]), expectedTitle) {
				t.Errorf("unexpected Slack blocks: %#v", blocks)
			}
			if sendRequests == 2 && (len(blocks) != 1 || objectValue(objectValue(blocks[0])["accessory"])["image_url"] != "https://img.example/poster.jpg") {
				t.Errorf("Slack image missing: %#v", blocks)
			}
			if sendRequests == 4 && (len(blocks) != 2 || objectValue(blocks[1])["type"] != "actions") {
				t.Errorf("Slack detail button missing: %#v", blocks)
			}
			if sendRequests == 3 {
				return jsonResponse(request, `{"ok":false,"error":"channel_not_found"}`), nil
			}
			return jsonResponse(request, `{"ok":true,"channel":"C2"}`), nil
		default:
			t.Errorf("unexpected Slack path: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, token, databasePath := nativeServicesFixture(t, "", nil, transport)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (91, 'Slack', 'slack', ?, '[]', 0, 1)`, `{"bot_token":"xoxb-secret","channel":"全体"}`); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ path, body string }{
		{"/api/v1/notifications/91/test", ""},
		{"/api/v1/notifications/custom-message", `{"title":"今晚入库","text":"两部影片已完成","image":"https://img.example/poster.jpg","channelIds":["91"]}`},
		{"/api/v1/notifications/91/test", ""},
	} {
		request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "xoxb-secret") {
			t.Fatalf("%s: status = %d: %s", testCase.path, response.Code, response.Body.String())
		}
		if sendRequests == 3 && !strings.Contains(response.Body.String(), `"ok":false`) {
			t.Fatalf("Slack error was not surfaced: %s", response.Body.String())
		}
	}
	service := notificationService{client: &http.Client{Transport: transport}}
	sent, err := service.sendNativeSlack(context.Background(), map[string]any{"bot_token": "xoxb-secret", "channel": "全体"}, "带图消息", "内容", "https://img.example/poster.jpg", "https://detail.example/item")
	if err != nil || !sent {
		t.Fatalf("Slack image and button delivery: sent=%v err=%v", sent, err)
	}
	if listRequests != 8 || sendRequests != 4 {
		t.Fatalf("unexpected Slack request count: list=%d send=%d", listRequests, sendRequests)
	}
}

func TestSplitNotificationTextPreservesUTF8(t *testing.T) {
	t.Parallel()
	input := strings.Repeat("影片", 750)
	chunks := splitNotificationText(input, 2048)
	if len(chunks) < 2 || strings.Join(chunks, "") != input {
		t.Fatalf("message was not split without loss: %d chunks", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) > 2048 || !utf8.ValidString(chunk) {
			t.Fatalf("invalid notification chunk: %d bytes", len(chunk))
		}
	}
}

func TestNotificationListNeverReturnsCredentials(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, item := range []struct {
		id                                   int
		name, ctype, configuration, switches string
		interactive, enabled                 int
	}{
		{12, "家庭通知", "telegram", `{"token":"bot-secret","chat_id":"123456"}`, `["download_start","transfer_finished"]`, 1, 1},
		{15, "备用推送", "bark", `{"server":"https://push.example","apikey":"device-secret"}`, `["download_fail"]`, 0, 0},
	} {
		if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (?, ?, ?, ?, ?, ?, ?)`, item.id, item.name, item.ctype, item.configuration, item.switches, item.interactive, item.enabled); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/notifications", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"bot-secret", "123456", "device-secret", "push.example"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("credential %q leaked: %s", secret, response.Body.String())
		}
	}
	var result struct {
		Data notificationsData `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Data.Items) != 2 || result.Data.Items[0].ID != "12" || !result.Data.Items[0].Configured || !result.Data.Items[0].CanInteract || result.Data.Items[0].SwitchLabels[1] != "入库完成" {
		t.Fatalf("unexpected notification data: %#v", result.Data)
	}
}

func TestNativeNotificationMutationsPreserveIDAndCredentials(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		call := httptest.NewRequest(method, path, strings.NewReader(body))
		call.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, call)
		return response
	}
	created := request(http.MethodPost, "/api/v1/notifications", `{"name":"首个渠道","type":"telegram","enabled":true,"interactive":true,"events":["download_start"],"config":{"token":"123:secret","chat_id":"100"}}`)
	if created.Code != http.StatusOK || strings.Contains(created.Body.String(), "secret") {
		t.Fatalf("create = %d: %s", created.Code, created.Body.String())
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var firstID int64
	if err := database.QueryRow("SELECT ID FROM MESSAGE_CLIENT WHERE NAME='首个渠道'").Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	second := request(http.MethodPost, "/api/v1/notifications", `{"name":"第二渠道","type":"telegram","enabled":true,"interactive":true,"events":[],"config":{"token":"456:secret","chat_id":"200"}}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second create = %d: %s", second.Code, second.Body.String())
	}
	var secondID int64
	if err := database.QueryRow("SELECT ID FROM MESSAGE_CLIENT WHERE NAME='第二渠道'").Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	var firstInteractive, secondInteractive int
	if err := database.QueryRow("SELECT INTERACTIVE FROM MESSAGE_CLIENT WHERE ID=?", firstID).Scan(&firstInteractive); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT INTERACTIVE FROM MESSAGE_CLIENT WHERE ID=?", secondID).Scan(&secondInteractive); err != nil {
		t.Fatal(err)
	}
	if firstInteractive != 0 || secondInteractive != 1 {
		t.Fatalf("interactive flags = %d, %d", firstInteractive, secondInteractive)
	}
	updated := request(http.MethodPut, fmt.Sprintf("/api/v1/notifications/%d", firstID), `{"name":"已重命名","type":"telegram","enabled":true,"interactive":false,"events":["download_fail"],"config":{}}`)
	if updated.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", updated.Code, updated.Body.String())
	}
	var name, configJSON, switches string
	if err := database.QueryRow("SELECT NAME, CONFIG, SWITCHS FROM MESSAGE_CLIENT WHERE ID=?", firstID).Scan(&name, &configJSON, &switches); err != nil {
		t.Fatal(err)
	}
	if name != "已重命名" || !strings.Contains(configJSON, "123:secret") || switches != `["download_fail"]` {
		t.Fatalf("updated channel = %q %q %q", name, configJSON, switches)
	}
	status := request(http.MethodPut, fmt.Sprintf("/api/v1/notifications/%d/status", firstID), `{"interactive":true}`)
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", status.Code, status.Body.String())
	}
	if err := database.QueryRow("SELECT INTERACTIVE FROM MESSAGE_CLIENT WHERE ID=?", secondID).Scan(&secondInteractive); err != nil || secondInteractive != 0 {
		t.Fatalf("other channel interactive = %d: %v", secondInteractive, err)
	}
	status = request(http.MethodPut, fmt.Sprintf("/api/v1/notifications/%d/status", secondID), `{"interactive":false}`)
	if status.Code != http.StatusOK {
		t.Fatalf("disable other interactive = %d: %s", status.Code, status.Body.String())
	}
	if err := database.QueryRow("SELECT INTERACTIVE FROM MESSAGE_CLIENT WHERE ID=?", firstID).Scan(&firstInteractive); err != nil || firstInteractive != 1 {
		t.Fatalf("unrelated interactive channel = %d: %v", firstInteractive, err)
	}
	deleted := request(http.MethodDelete, fmt.Sprintf("/api/v1/notifications/%d", firstID), "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", deleted.Code, deleted.Body.String())
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM MESSAGE_CLIENT WHERE ID=?", firstID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted channel count = %d: %v", count, err)
	}
	form := url.Values{
		"cid": {strconv.FormatInt(secondID, 10)}, "name": {"旧版更新"}, "type": {"telegram"},
		"config": {`{"token":"456:secret","chat_id":"200"}`}, "switchs": {`["download_start"]`},
		"interactive": {"1"}, "enabled": {"1"},
	}
	legacyRequest := httptest.NewRequest(http.MethodPost, "/api/v1/message/client/update", strings.NewReader(form.Encode()))
	legacyRequest.Header.Set("Authorization", token)
	legacyRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	legacyResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyResponse, legacyRequest)
	if legacyResponse.Code != http.StatusOK {
		t.Fatalf("legacy update = %d: %s", legacyResponse.Code, legacyResponse.Body.String())
	}
	if err := database.QueryRow("SELECT NAME FROM MESSAGE_CLIENT WHERE ID=?", secondID).Scan(&name); err != nil || name != "旧版更新" {
		t.Fatalf("legacy updated channel = %q: %v", name, err)
	}
	legacyStatus := httptest.NewRequest(http.MethodPost, "/api/v1/message/client/status", strings.NewReader(url.Values{"cid": {strconv.FormatInt(secondID, 10)}, "flag": {"enable"}, "checked": {"0"}}.Encode()))
	legacyStatus.Header.Set("Authorization", token)
	legacyStatus.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	legacyStatusResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyStatusResponse, legacyStatus)
	if legacyStatusResponse.Code != http.StatusOK {
		t.Fatalf("legacy status = %d: %s", legacyStatusResponse.Code, legacyStatusResponse.Body.String())
	}
	var enabled int
	if err := database.QueryRow("SELECT ENABLED FROM MESSAGE_CLIENT WHERE ID=?", secondID).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("legacy enabled = %d: %v", enabled, err)
	}
	legacyDelete := httptest.NewRequest(http.MethodPost, "/api/v1/message/client/delete", strings.NewReader(url.Values{"cid": {strconv.FormatInt(secondID, 10)}}.Encode()))
	legacyDelete.Header.Set("Authorization", token)
	legacyDelete.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	legacyDeleteResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyDeleteResponse, legacyDelete)
	if legacyDeleteResponse.Code != http.StatusOK {
		t.Fatalf("legacy delete = %d: %s", legacyDeleteResponse.Code, legacyDeleteResponse.Body.String())
	}
}

func TestNotificationTestWithoutNativeStorageDoesNotUseLegacy(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		t.Errorf("unexpected legacy request: %s", request.URL.Path)
		return nil, nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/12/test", nil)
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || requests != 0 {
		t.Fatalf("status = %d, requests = %d: %s", response.Code, requests, response.Body.String())
	}
}

func TestNotificationStatusUpdatesNativeStorage(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (12, 'Telegram', 'telegram', '{"token":"secret"}', '[]', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/notifications/12/status", strings.NewReader(`{"interactive":true}`))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var interactive int
	if err := database.QueryRow("SELECT INTERACTIVE FROM MESSAGE_CLIENT WHERE ID=12").Scan(&interactive); err != nil || interactive != 1 {
		t.Fatalf("interactive = %d: %v", interactive, err)
	}
}

func TestNotificationDeleteUsesValidatedID(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (15, 'Bark', 'bark', '{}', '[]', 0, 1)`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/notifications/15", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM MESSAGE_CLIENT WHERE ID=15").Scan(&count); err != nil || count != 0 {
		t.Fatalf("remaining rows = %d: %v", count, err)
	}
}

func TestNotificationOptionsAndDetailKeepTextFieldsWriteOnly(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (20, '自动化', 'webhook', ?, '["download_start"]', 0, 1)`, `{"url":"https://secret.example/hook","method":"POST","token":"hook-secret"}`); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/notifications/20", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"secret.example", "hook-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("write-only value %q leaked: %s", secret, response.Body.String())
		}
	}
	var result struct {
		Data notificationDetail `json:"data"`
	}
	_ = json.NewDecoder(response.Body).Decode(&result)
	if result.Data.Config["method"] != "POST" || len(result.Data.ConfiguredFields) != 3 {
		t.Fatalf("unexpected safe detail: %#v", result.Data)
	}
}

func TestNotificationUpdatePreservesWriteOnlyValues(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (20, '自动化', 'webhook', ?, '["download_start"]', 0, 1)`, `{"url":"https://secret.example/hook","method":"POST","token":"hook-secret"}`); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"自动化通知","type":"webhook","enabled":true,"interactive":false,"events":["download_start","download_fail"],"config":{"method":"GET"},"clearConfig":[]}`
	request := httptest.NewRequest(http.MethodPut, "/api/v1/notifications/20", strings.NewReader(body))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var configuration, switches string
	if err := database.QueryRow("SELECT CONFIG, SWITCHS FROM MESSAGE_CLIENT WHERE ID=20").Scan(&configuration, &switches); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(configuration), &saved); err != nil {
		t.Fatal(err)
	}
	if saved["url"] != "https://secret.example/hook" || saved["token"] != "hook-secret" || saved["method"] != "GET" || switches != `["download_start","download_fail"]` {
		t.Fatalf("configuration was not merged safely: %#v, switches=%s", saved, switches)
	}
}

func TestNotificationCreateUsesDefaultsAndValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	body := `{"name":"新渠道","type":"webhook","enabled":true,"interactive":false,"events":[],"config":{"url":"https://new.example/hook"},"clearConfig":[]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(body))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var configuration string
	if err := database.QueryRow("SELECT CONFIG FROM MESSAGE_CLIENT WHERE NAME='新渠道'").Scan(&configuration); err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(configuration), &saved); err != nil || saved["url"] != "https://new.example/hook" || saved["method"] != "POST" {
		t.Fatalf("defaults were not applied: %#v err=%v", saved, err)
	}

	invalidBody := `{"name":"缺少地址","type":"webhook","enabled":true,"interactive":false,"events":[],"config":{},"clearConfig":["url"]}`
	invalidRequest := httptest.NewRequest(http.MethodPost, "/api/v1/notifications", strings.NewReader(invalidBody))
	invalidRequest.Header.Set("Authorization", token)
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest || !strings.Contains(invalidResponse.Body.String(), "URL is required") {
		t.Fatalf("required field validation status = %d: %s", invalidResponse.Code, invalidResponse.Body.String())
	}
}

func TestCustomMessageWithoutNativeStorageDoesNotUseLegacy(t *testing.T) {
	t.Parallel()
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		t.Errorf("unexpected legacy request: %s", request.URL.Path)
		return nil, nil
	})
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, transport)
	body := `{"title":" 今晚入库 ","text":"两部影片已完成","image":"https://img.example/poster.jpg","channelIds":["12","12"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/custom-message", strings.NewReader(body))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || requests != 0 {
		t.Fatalf("status = %d, requests = %d: %s", response.Code, requests, response.Body.String())
	}
}

func TestCustomMessageRejectsUnavailableChannel(t *testing.T) {
	t.Parallel()
	handler, token, databasePath := nativeServicesFixture(t, "", nil, nil)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`INSERT INTO MESSAGE_CLIENT (ID, NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED) VALUES (15, '禁用渠道', 'serverchan', '{"sckey":"SCT_SECRET_123"}', '[]', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	body := `{"title":"测试","text":"","image":"","channelIds":["15"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/custom-message", strings.NewReader(body))
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unavailable") {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}

func TestCustomMessageRejectsInvalidImageURL(t *testing.T) {
	t.Parallel()
	handler, _ := newHandler(config.Config{LegacyBackendURL: "http://legacy:3000"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		t.Fatalf("invalid request should not reach legacy service")
		return nil, nil
	}))
	body := `{"title":"测试","text":"","image":"javascript:alert(1)","channelIds":["12"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/custom-message", strings.NewReader(body))
	request.Header.Set("Authorization", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
