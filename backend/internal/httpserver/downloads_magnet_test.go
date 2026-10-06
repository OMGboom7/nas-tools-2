package httpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

const testMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Movie"

func TestNativePan115MagnetAddDoesNotCallPython(t *testing.T) {
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host == "legacy:3000" || request.Header.Get("Cookie") != "UID=server-secret" {
			t.Fatalf("unexpected magnet destination: %s %v", request.URL, request.Header)
		}
		switch request.URL.Host {
		case "webapi.115.com":
			if request.Method != http.MethodGet || request.URL.Path != "/files/getid" || request.URL.Query().Get("path") != "/" {
				t.Fatalf("unexpected directory request: %s", request.URL)
			}
			return jsonResponse(request, `{"state":true,"id":"123"}`), nil
		case "115.com":
			if request.Method != http.MethodPost || request.URL.Query().Get("ac") != "add_task_urls" {
				t.Fatalf("unexpected task request: %s", request.URL)
			}
			body, _ := io.ReadAll(request.Body)
			values, err := url.ParseQuery(string(body))
			if err != nil || values.Get("url[0]") != testMagnet || values.Get("wp_path_id") != "123" || len(values) != 3 {
				t.Fatalf("unexpected task form: %s err=%v", body, err)
			}
			return jsonResponse(request, `{"state":true,"result":[{"info_hash":"0123456789abcdef0123456789abcdef01234567"}]}`), nil
		default:
			t.Fatalf("unexpected host: %s", request.URL)
			return nil, nil
		}
	})
	downloader := downloaderconfig.Downloader{Name: "115", Type: "pan115", Enabled: 1, Config: `{"cookie":"UID=server-secret"}`}
	handler, token, _ := nativeServicesFixture(t, "", &downloader, transport)
	result := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/magnet", token, `{"magnet":"`+testMagnet+`"}`)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"id":"0123456789abcdef0123456789abcdef01234567"`) || calls != 2 || strings.Contains(result.Body.String(), "server-secret") {
		t.Fatalf("add=%d %s calls=%d", result.Code, result.Body.String(), calls)
	}
}

func TestNativeMagnetAddDoesNotCallPython(t *testing.T) {
	t.Parallel()
	for _, downloaderType := range []string{"qbittorrent", "transmission", "aria2"} {
		t.Run(downloaderType, func(t *testing.T) {
			calls := 0
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host == "legacy:3000" {
					t.Fatalf("magnet add called Python: %s", request.URL)
				}
				switch downloaderType {
				case "qbittorrent":
					if request.URL.Host != "client.local:8080" {
						t.Fatalf("unexpected qB host: %s", request.URL)
					}
					if strings.HasSuffix(request.URL.Path, "/auth/login") {
						return jsonResponse(request, "Ok."), nil
					}
					if request.URL.Path != "/api/v2/torrents/add" || request.ParseForm() != nil || request.Form.Get("urls") != testMagnet {
						t.Fatalf("unexpected qB request: %s %v", request.URL, request.Form)
					}
					return jsonResponse(request, "Ok."), nil
				case "transmission":
					if request.URL.Path != "/transmission/rpc" {
						t.Fatalf("unexpected Transmission path: %s", request.URL)
					}
					if request.Header.Get("X-Transmission-Session-Id") == "" {
						response := jsonResponse(request, "")
						response.StatusCode = http.StatusConflict
						response.Header.Set("X-Transmission-Session-Id", "session")
						response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
						return response, nil
					}
					var payload struct {
						Method string `json:"method"`
						Params struct {
							Filename string `json:"filename"`
						} `json:"params"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Method != "torrent_add" || payload.Params.Filename != testMagnet {
						t.Fatalf("unexpected Transmission payload: %#v", payload)
					}
					return jsonResponse(request, `{"jsonrpc":"2.0","result":{"torrent_added":{"hash_string":"0123456789abcdef0123456789abcdef01234567"}},"id":1}`), nil
				case "aria2":
					if request.URL.Path != "/jsonrpc" {
						t.Fatalf("unexpected Aria2 path: %s", request.URL)
					}
					var payload struct {
						Method string            `json:"method"`
						Params []json.RawMessage `json:"params"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Method != "aria2.addUri" || len(payload.Params) != 3 {
						t.Fatalf("unexpected Aria2 payload: %#v", payload)
					}
					var uris []string
					if json.Unmarshal(payload.Params[1], &uris) != nil || len(uris) != 1 || uris[0] != testMagnet {
						t.Fatalf("unexpected Aria2 URI: %s", payload.Params[1])
					}
					return jsonResponse(request, `{"jsonrpc":"2.0","id":"nastool-add","result":"0123456789abcdef"}`), nil
				}
				return nil, nil
			})
			downloader := downloaderconfig.Downloader{Name: "Default", Type: downloaderType, Enabled: 1, Transfer: 1, Config: `{"host":"client.local","port":8080,"username":"admin","password":"secret","secret":"secret"}`}
			handler, token, _ := nativeServicesFixture(t, "", &downloader, transport)
			response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/magnet", token, `{"magnet":"`+testMagnet+`"}`)
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"success":true`) || calls == 0 {
				t.Fatalf("add = %d %s calls=%d", response.Code, response.Body.String(), calls)
			}
			if downloaderType == "qbittorrent" && !strings.Contains(response.Body.String(), `"id":"0123456789abcdef0123456789abcdef01234567"`) {
				t.Fatalf("qB torrent ID missing: %s", response.Body.String())
			}
		})
	}
}

func TestNativeMagnetRejectsInvalidAndMissingAuth(t *testing.T) {
	t.Parallel()
	called := false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		t.Fatalf("invalid magnet reached %s", request.URL)
		return nil, nil
	})
	downloader := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"client.local","port":8080}`}
	handler, token, _ := nativeServicesFixture(t, "", &downloader, transport)
	for _, raw := range []string{`{"magnet":"https://example.com/a.torrent"}`, `{"magnet":"magnet:?xt=urn:btih:bad"}`, `{"magnet":"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567%0d"}`, `{"magnet":"` + testMagnet + `","extra":true}`} {
		response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/magnet", token, raw)
		if response.Code != 400 {
			t.Fatalf("invalid input = %d %s", response.Code, response.Body.String())
		}
	}
	if response := nativeJSONRequest(handler, http.MethodPost, "/api/v1/downloads/magnet", "", `{"magnet":"`+testMagnet+`"}`); response.Code != 401 {
		t.Fatalf("missing auth = %d", response.Code)
	}
	if called {
		t.Fatal("invalid input reached downloader")
	}
}
