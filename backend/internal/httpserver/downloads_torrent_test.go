package httpserver

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

var testTorrent = []byte("d4:infod4:name4:Testee")

func torrentUpload(handler http.Handler, token, name string, contents []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreateFormFile("torrent", name)
	_, _ = part.Write(contents)
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/downloads/torrent", &body)
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestNativeTorrentUploadDoesNotCallPython(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"qbittorrent", "transmission", "aria2"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host == "legacy:3000" {
					t.Fatalf("upload called Python: %s", request.URL)
				}
				switch kind {
				case "qbittorrent":
					if strings.HasSuffix(request.URL.Path, "/auth/login") {
						return jsonResponse(request, "Ok."), nil
					}
					if request.URL.Path != "/api/v2/torrents/add" || request.ParseMultipartForm(1<<20) != nil {
						t.Fatalf("bad qB upload: %s", request.URL)
					}
					file, header, err := request.FormFile("torrents")
					if err != nil || header.Filename != "upload.torrent" {
						t.Fatalf("bad qB file: %#v %v", header, err)
					}
					contents, _ := io.ReadAll(file)
					_ = file.Close()
					if !bytes.Equal(contents, testTorrent) {
						t.Fatalf("qB torrent changed: %q", contents)
					}
					return jsonResponse(request, "Ok."), nil
				case "transmission":
					if request.URL.Path != "/transmission/rpc" {
						t.Fatalf("bad Transmission path: %s", request.URL)
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
							Metainfo string `json:"metainfo"`
						} `json:"params"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Method != "torrent_add" {
						t.Fatalf("bad Transmission payload: %#v", payload)
					}
					contents, err := base64.StdEncoding.DecodeString(payload.Params.Metainfo)
					if err != nil || !bytes.Equal(contents, testTorrent) {
						t.Fatalf("Transmission torrent changed: %q %v", contents, err)
					}
					return jsonResponse(request, `{"jsonrpc":"2.0","result":{"torrent_added":{"hash_string":"0123456789abcdef0123456789abcdef01234567"}},"id":1}`), nil
				case "aria2":
					if request.URL.Path != "/jsonrpc" {
						t.Fatalf("bad Aria2 path: %s", request.URL)
					}
					var payload struct {
						Method string            `json:"method"`
						Params []json.RawMessage `json:"params"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Method != "aria2.addTorrent" || len(payload.Params) != 4 {
						t.Fatalf("bad Aria2 payload: %#v", payload)
					}
					var encoded string
					if json.Unmarshal(payload.Params[1], &encoded) != nil {
						t.Fatal("bad Aria2 metainfo")
					}
					contents, err := base64.StdEncoding.DecodeString(encoded)
					if err != nil || !bytes.Equal(contents, testTorrent) {
						t.Fatalf("Aria2 torrent changed: %q %v", contents, err)
					}
					return jsonResponse(request, `{"jsonrpc":"2.0","id":"nastool-add-torrent","result":"0123456789abcdef"}`), nil
				}
				return nil, nil
			})
			downloader := downloaderconfig.Downloader{Name: "Default", Type: kind, Enabled: 1, Transfer: 1, Config: `{"host":"client.local","port":8080,"username":"admin","password":"secret","secret":"secret"}`}
			handler, token, _ := nativeServicesFixture(t, "", &downloader, transport)
			response := torrentUpload(handler, token, "movie.torrent", testTorrent)
			if response.Code != 200 || !strings.Contains(response.Body.String(), `"success":true`) || calls == 0 {
				t.Fatalf("upload = %d %s calls=%d", response.Code, response.Body.String(), calls)
			}
			if kind == "qbittorrent" {
				hash := sha1.Sum([]byte("d4:name4:Teste"))
				if !strings.Contains(response.Body.String(), hex.EncodeToString(hash[:])) {
					t.Fatalf("qB info hash missing: %s", response.Body.String())
				}
			}
		})
	}
}

func TestNativeTorrentUploadRejectsInvalidFiles(t *testing.T) {
	t.Parallel()
	called := false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		t.Fatalf("invalid upload reached %s", request.URL)
		return nil, nil
	})
	downloader := downloaderconfig.Downloader{Name: "Default", Type: "qbittorrent", Enabled: 1, Config: `{"host":"client.local","port":8080}`}
	handler, token, _ := nativeServicesFixture(t, "", &downloader, transport)
	for _, input := range []struct {
		name     string
		contents []byte
	}{
		{"movie.txt", testTorrent}, {"movie.torrent", nil}, {"movie.torrent", []byte("not bencode")}, {"movie.torrent", bytes.Repeat([]byte("d"), (8<<20)+1)},
		{"movie.torrent", []byte("d4:infolee")},
	} {
		response := torrentUpload(handler, token, input.name, input.contents)
		if response.Code != 400 {
			t.Fatalf("accepted %s (%d bytes): %d %s", input.name, len(input.contents), response.Code, response.Body.String())
		}
	}
	if response := torrentUpload(handler, "", "movie.torrent", testTorrent); response.Code != 401 {
		t.Fatalf("unauthenticated upload = %d", response.Code)
	}
	if called {
		t.Fatal("invalid upload reached downloader")
	}
}
