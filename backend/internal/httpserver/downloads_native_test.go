package httpserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

func TestNativePan115DownloadListAndCompatNow(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	pages := []string{}
	deletes := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "115.com" || request.URL.Path != "/web/lixian/" || request.Header.Get("Cookie") != "UID=server-secret" {
			t.Fatalf("unexpected destination: %s %v", request.URL, request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		switch request.URL.Query().Get("ac") {
		case "task_lists":
			pages = append(pages, string(body))
			if string(body) != "page=1" {
				t.Fatalf("unexpected page: %s", body)
			}
			return jsonResponse(request, `{"state":true,"count":2,"page_count":1,"tasks":[{"info_hash":"`+hash+`","name":"Cloud.Movie.2026","status":1,"percentDone":48.5},{"info_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"Complete","status":2,"percentDone":100}]}`), nil
		case "task_del":
			values, err := url.ParseQuery(string(body))
			if err != nil || values.Get("hash[0]") != hash || len(values) != 1 {
				t.Fatalf("unexpected delete form: %s err=%v", body, err)
			}
			deletes++
			return jsonResponse(request, `{"state":true}`), nil
		default:
			t.Fatalf("unexpected 115 action: %s", request.URL)
			return nil, nil
		}
	})
	downloader := downloaderconfig.Downloader{Name: "115", Type: "pan115", Enabled: 1, Config: `{"cookie":"UID=server-secret"}`}
	handler, token, _ := nativeServicesFixture(t, "", &downloader, transport)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/downloads", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), hash) || !strings.Contains(response.Body.String(), "Cloud.Movie.2026") || !strings.Contains(response.Body.String(), `"progress":48.5`) || !strings.Contains(response.Body.String(), `"canControl":false`) || !strings.Contains(response.Body.String(), `"canRemove":true`) || strings.Contains(response.Body.String(), "Complete") {
		t.Fatalf("native list: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") {
		t.Fatal("download list leaked cookie")
	}
	compat := performFormRequest(handler, "/api/v1/download/now", token, url.Values{"id": {"1"}, "force_list": {"true"}})
	if compat.Code != 200 || !strings.Contains(compat.Body.String(), hash) || !strings.Contains(compat.Body.String(), `"success":true`) {
		t.Fatalf("compat now: %d %s", compat.Code, compat.Body.String())
	}
	for _, action := range []string{"start", "stop"} {
		modern := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/"+hash+"/"+action, token, "")
		legacy := performFormRequest(handler, "/api/v1/download/"+action, token, url.Values{"id": {hash}})
		if modern.Code != http.StatusNotImplemented || legacy.Code != http.StatusNotImplemented {
			t.Fatalf("unsupported 115 %s: modern=%d legacy=%d", action, modern.Code, legacy.Code)
		}
	}
	for _, endpoint := range []string{"/api/v1/downloads/" + hash + "/remove", "/api/v1/download/remove"} {
		var result *httptest.ResponseRecorder
		if strings.Contains(endpoint, "/downloads/") {
			result = nativeJSONRequest(handler, http.MethodPost, endpoint, token, "")
		} else {
			result = performFormRequest(handler, endpoint, token, url.Values{"id": {hash}})
		}
		if result.Code != 200 || !strings.Contains(result.Body.String(), `"success":true`) {
			t.Fatalf("115 remove: %d %s", result.Code, result.Body.String())
		}
	}
	invalid := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/all/remove", token, "")
	if invalid.Code != http.StatusBadRequest || deletes != 2 {
		t.Fatalf("invalid delete status=%d, upstream calls=%d", invalid.Code, deletes)
	}
	if len(pages) != 2 {
		t.Fatalf("115 task calls=%v", pages)
	}
}

func TestNativeQbittorrentDownloadListAndControls(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	var lock sync.Mutex
	paths := []string{}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		lock.Lock()
		paths = append(paths, request.URL.Host+request.URL.Path)
		lock.Unlock()
		if request.URL.Host == "legacy:3000" {
			if request.URL.Path != "/api/v1/download/history" {
				t.Fatalf("active task unexpectedly used Flask: %s", request.URL.Path)
			}
			return jsonResponse(request, `{"code":0,"data":{"Items":[]}}`), nil
		}
		if request.URL.Host != "qb.local:8080" {
			t.Fatalf("unexpected destination: %s", request.URL)
		}
		switch request.URL.Path {
		case "/api/v2/auth/login":
			response := jsonResponse(request, "Ok.")
			response.Header.Set("Set-Cookie", "SID=session; Path=/; HttpOnly")
			return response, nil
		case "/api/v2/torrents/info":
			if request.URL.Query().Get("filter") != "downloading" {
				t.Fatalf("filter = %q", request.URL.Query().Get("filter"))
			}
			return jsonResponse(request, `[{"hash":"`+hash+`","name":"Native.Movie.2026","progress":0.425,"dlspeed":1048576,"upspeed":1024,"eta":65,"state":"pausedDL"}]`), nil
		case "/api/v2/torrents/resume", "/api/v2/torrents/pause", "/api/v2/torrents/delete":
			payload, _ := io.ReadAll(request.Body)
			form, _ := url.ParseQuery(string(payload))
			if form.Get("hashes") != hash {
				t.Fatalf("task hash = %q", form.Get("hashes"))
			}
			if strings.HasSuffix(request.URL.Path, "/delete") && form.Get("deleteFiles") != "true" {
				t.Fatalf("deleteFiles = %q", form.Get("deleteFiles"))
			}
			return jsonResponse(request, ""), nil
		default:
			t.Fatalf("unexpected qBittorrent endpoint: %s", request.URL.Path)
			return nil, nil
		}
	})
	handler, _, token := nativeSiteFixture(t, transport)
	add := url.Values{"name": {"QB"}, "type": {"qbittorrent"}, "enabled": {"1"}, "transfer": {"1"}, "only_nastool": {"1"}, "config": {`{"host":"qb.local","port":8080,"username":"admin","password":"server-secret"}`}}
	if response := performFormRequest(handler, "/api/v1/download/client/add", token, add); response.Code != 200 {
		t.Fatalf("add downloader: %d %s", response.Code, response.Body.String())
	}
	if response := performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {"DefaultDownloader"}, "value": {"1"}}); response.Code != 200 {
		t.Fatalf("set default: %d %s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/downloads", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), hash) || !strings.Contains(response.Body.String(), "Native.Movie.2026") || !strings.Contains(response.Body.String(), `"progress":42.5`) || !strings.Contains(response.Body.String(), `"state":"Stoped"`) || !strings.Contains(response.Body.String(), "1.0 MB/s") {
		t.Fatalf("native list: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") || strings.Contains(response.Body.String(), "session") {
		t.Fatal("download list leaked credentials")
	}
	compatNow := performFormRequest(handler, "/api/v1/download/now", token, url.Values{"id": {"1"}, "force_list": {"true"}})
	if compatNow.Code != 200 || !strings.Contains(compatNow.Body.String(), hash) || !strings.Contains(compatNow.Body.String(), `"success":true`) {
		t.Fatalf("compat now: %d %s", compatNow.Code, compatNow.Body.String())
	}
	compatHistory := performFormRequest(handler, "/api/v1/download/history", token, url.Values{"page": {"1"}})
	if compatHistory.Code != 200 || !strings.Contains(compatHistory.Body.String(), "Fixture History") || !strings.Contains(compatHistory.Body.String(), `"media_type":"电影"`) {
		t.Fatalf("compat history: %d %s", compatHistory.Code, compatHistory.Body.String())
	}
	for _, action := range []string{"start", "stop", "remove"} {
		response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/"+hash+"/"+action, token, "")
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", action, response.Code, response.Body.String())
		}
		compat := performFormRequest(handler, "/api/v1/download/"+action, token, url.Values{"id": {hash}})
		if compat.Code != 200 || !strings.Contains(compat.Body.String(), hash) {
			t.Fatalf("compat %s: %d %s", action, compat.Code, compat.Body.String())
		}
	}
	lock.Lock()
	before := len(paths)
	lock.Unlock()
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/all/stop", token, "")
	if response.Code != 400 {
		t.Fatalf("bulk task ID accepted: %d %s", response.Code, response.Body.String())
	}
	lock.Lock()
	after := len(paths)
	lock.Unlock()
	if before != after {
		t.Fatal("invalid task ID reached qBittorrent")
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "/api/v1/download/now") || strings.HasSuffix(path, "/api/v1/download/start") || strings.HasSuffix(path, "/api/v1/download/stop") || strings.HasSuffix(path, "/api/v1/download/remove") {
			t.Fatalf("native qBittorrent operation reached Flask: %s", path)
		}
	}
}

func TestNativeTransmissionDownloadListAndControls(t *testing.T) {
	const hash = "abcdef0123456789abcdef0123456789abcdef01"
	var lock sync.Mutex
	legacyPaths := []string{}
	methods := []string{}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "legacy:3000" {
			lock.Lock()
			legacyPaths = append(legacyPaths, request.URL.Path)
			lock.Unlock()
			if request.URL.Path != "/api/v1/download/history" {
				t.Fatalf("Transmission unexpectedly used Flask: %s", request.URL.Path)
			}
			return jsonResponse(request, `{"code":0,"data":{"Items":[]}}`), nil
		}
		if request.URL.Host != "tr.local:9091" || request.URL.Path != "/transmission/rpc" {
			t.Fatalf("unexpected destination: %s", request.URL)
		}
		if request.Header.Get("X-Transmission-Session-Id") == "" {
			response := jsonResponse(request, "")
			response.StatusCode = http.StatusConflict
			response.Header.Set("X-Transmission-Session-Id", "modern-session")
			response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
			return response, nil
		}
		payload, _ := io.ReadAll(request.Body)
		body := string(payload)
		lock.Lock()
		methods = append(methods, body)
		lock.Unlock()
		switch {
		case strings.Contains(body, `"method":"torrent_get"`):
			return jsonResponse(request, `{"jsonrpc":"2.0","result":{"torrents":[{"hash_string":"`+hash+`","name":"Transmission.Movie.2026","percent_done":0.375,"rate_download":2097152,"rate_upload":2048,"eta":125,"status":0}]},"id":1}`), nil
		case strings.Contains(body, `"method":"torrent_start"`), strings.Contains(body, `"method":"torrent_stop"`):
			if !strings.Contains(body, hash) {
				t.Fatalf("missing task hash: %s", body)
			}
			return jsonResponse(request, `{"jsonrpc":"2.0","result":{},"id":1}`), nil
		case strings.Contains(body, `"method":"torrent_remove"`):
			if !strings.Contains(body, hash) || !strings.Contains(body, `"delete_local_data":true`) {
				t.Fatalf("unsafe remove payload: %s", body)
			}
			return jsonResponse(request, `{"jsonrpc":"2.0","result":{},"id":1}`), nil
		default:
			t.Fatalf("unexpected Transmission payload: %s", body)
			return nil, nil
		}
	})
	handler, _, token := nativeSiteFixture(t, transport)
	add := url.Values{"name": {"Transmission"}, "type": {"transmission"}, "enabled": {"1"}, "transfer": {"1"}, "only_nastool": {"1"}, "config": {`{"host":"tr.local","port":9091,"username":"admin","password":"server-secret"}`}}
	if response := performFormRequest(handler, "/api/v1/download/client/add", token, add); response.Code != 200 {
		t.Fatalf("add downloader: %d %s", response.Code, response.Body.String())
	}
	if response := performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {"DefaultDownloader"}, "value": {"1"}}); response.Code != 200 {
		t.Fatalf("set default: %d %s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/downloads", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), hash) || !strings.Contains(response.Body.String(), "Transmission.Movie.2026") || !strings.Contains(response.Body.String(), `"progress":37.5`) || !strings.Contains(response.Body.String(), `"state":"Stoped"`) || !strings.Contains(response.Body.String(), "2.0 MB/s") {
		t.Fatalf("native list: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") || strings.Contains(response.Body.String(), "modern-session") {
		t.Fatal("download list leaked credentials")
	}
	for _, action := range []string{"start", "stop", "remove"} {
		response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/"+hash+"/"+action, token, "")
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", action, response.Code, response.Body.String())
		}
	}
	lock.Lock()
	defer lock.Unlock()
	if len(methods) != 4 {
		t.Fatalf("Transmission methods = %d", len(methods))
	}
	for _, path := range legacyPaths {
		if path != "/api/v1/download/history" {
			t.Fatalf("native Transmission operation reached Flask: %s", path)
		}
	}
}

func TestNativeAria2DownloadListAndControls(t *testing.T) {
	const gid = "0123456789abcdef"
	var lock sync.Mutex
	legacyPaths := []string{}
	methods := []string{}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "legacy:3000" {
			lock.Lock()
			legacyPaths = append(legacyPaths, request.URL.Path)
			lock.Unlock()
			if request.URL.Path != "/api/v1/download/history" {
				t.Fatalf("Aria2 unexpectedly used Flask: %s", request.URL.Path)
			}
			return jsonResponse(request, `{"code":0,"data":{"Items":[]}}`), nil
		}
		if request.URL.Host != "aria.local:6800" || request.URL.Path != "/jsonrpc" {
			t.Fatalf("unexpected destination: %s", request.URL)
		}
		payload, _ := io.ReadAll(request.Body)
		body := string(payload)
		lock.Lock()
		methods = append(methods, body)
		lock.Unlock()
		if !strings.Contains(body, `"token:server-secret"`) {
			t.Fatalf("missing method token: %s", body)
		}
		switch {
		case strings.Contains(body, `"method":"aria2.tellActive"`):
			return jsonResponse(request, `{"jsonrpc":"2.0","id":"nastool-active","result":[{"gid":"`+gid+`","status":"active","totalLength":"1000","completedLength":"625","downloadSpeed":"1048576","uploadSpeed":"1024","bittorrent":{"info":{"name":"Aria.Movie.2026"}},"files":[]}]}`), nil
		case strings.Contains(body, `"method":"aria2.tellWaiting"`):
			return jsonResponse(request, `{"jsonrpc":"2.0","id":"nastool-waiting","result":[]}`), nil
		case strings.Contains(body, `"method":"aria2.unpause"`), strings.Contains(body, `"method":"aria2.pause"`), strings.Contains(body, `"method":"aria2.remove"`):
			if !strings.Contains(body, gid) {
				t.Fatalf("missing task gid: %s", body)
			}
			return jsonResponse(request, `{"jsonrpc":"2.0","id":"nastool-control","result":"`+gid+`"}`), nil
		default:
			t.Fatalf("unexpected Aria2 payload: %s", body)
			return nil, nil
		}
	})
	handler, _, token := nativeSiteFixture(t, transport)
	add := url.Values{"name": {"Aria2"}, "type": {"aria2"}, "enabled": {"1"}, "transfer": {"1"}, "only_nastool": {"1"}, "config": {`{"host":"aria.local","port":6800,"secret":"server-secret"}`}}
	if response := performFormRequest(handler, "/api/v1/download/client/add", token, add); response.Code != 200 {
		t.Fatalf("add downloader: %d %s", response.Code, response.Body.String())
	}
	if response := performFormRequest(handler, "/api/v1/config/set", token, url.Values{"key": {"DefaultDownloader"}, "value": {"1"}}); response.Code != 200 {
		t.Fatalf("set default: %d %s", response.Code, response.Body.String())
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/downloads", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), gid) || !strings.Contains(response.Body.String(), "Aria.Movie.2026") || !strings.Contains(response.Body.String(), `"progress":62.5`) || !strings.Contains(response.Body.String(), "1.0 MB/s") || !strings.Contains(response.Body.String(), "Fixture History") || !strings.Contains(response.Body.String(), "Fixture.Release") {
		t.Fatalf("native list: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "server-secret") {
		t.Fatal("download list leaked credentials")
	}
	for _, action := range []string{"start", "stop", "remove"} {
		response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/"+gid+"/"+action, token, "")
		if response.Code != 200 {
			t.Fatalf("%s: %d %s", action, response.Code, response.Body.String())
		}
	}
	lock.Lock()
	before := len(methods)
	lock.Unlock()
	response = nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/all/stop", token, "")
	if response.Code != 400 {
		t.Fatalf("bulk task ID accepted: %d %s", response.Code, response.Body.String())
	}
	lock.Lock()
	defer lock.Unlock()
	if len(methods) != before || len(methods) != 5 {
		t.Fatalf("Aria2 methods = %d before invalid=%d", len(methods), before)
	}
	if len(legacyPaths) != 0 {
		t.Fatalf("native downloads page reached Flask: %v", legacyPaths)
	}
	for _, path := range legacyPaths {
		if path != "/api/v1/download/history" {
			t.Fatalf("native Aria2 operation reached Flask: %s", path)
		}
	}
}
