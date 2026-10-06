package transmission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAddMagnetNegotiatesBothRPCVersions(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, modern := range []bool{false, true} {
		client, err := New("tr.example", "9091", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("X-Transmission-Session-Id") == "" {
				response := reply(409, "")
				response.Header.Set("X-Transmission-Session-Id", "session")
				if modern {
					response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
				}
				return response, nil
			}
			var payload struct {
				Method    string            `json:"method"`
				Arguments map[string]string `json:"arguments"`
				Params    map[string]string `json:"params"`
			}
			if json.NewDecoder(request.Body).Decode(&payload) != nil {
				t.Fatal("invalid request JSON")
			}
			if modern {
				if payload.Method != "torrent_add" || payload.Params["filename"] != magnet {
					t.Fatalf("modern payload: %#v", payload)
				}
				return reply(200, `{"jsonrpc":"2.0","result":{"torrent_added":{"hash_string":"`+hash+`"}},"id":1}`), nil
			}
			if payload.Method != "torrent-add" || payload.Arguments["filename"] != magnet {
				t.Fatalf("legacy payload: %#v", payload)
			}
			return reply(200, `{"result":"success","arguments":{"torrent-added":{"hashString":"`+hash+`"}}}`), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := client.AddMagnet(context.Background(), magnet); err != nil || got != hash {
			t.Fatalf("add = %q, %v", got, err)
		}
	}
}

func TestAddMagnetWithOptionsUsesProtocolFieldNames(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, modern := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "modern"}[modern], func(t *testing.T) {
			client, err := New("tr.example", "9091", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
				if request.Header.Get("X-Transmission-Session-Id") == "" {
					response := reply(409, "")
					response.Header.Set("X-Transmission-Session-Id", "session")
					if modern {
						response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
					}
					return response, nil
				}
				var payload struct {
					Method    string         `json:"method"`
					Arguments map[string]any `json:"arguments"`
					Params    map[string]any `json:"params"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				args, dirKey := payload.Arguments, "download-dir"
				if modern {
					args, dirKey = payload.Params, "download_dir"
				}
				if args["filename"] != magnet || args[dirKey] != "/downloads/tv" || args["paused"] != true {
					t.Fatalf("settings=%v", args)
				}
				labels, ok := args["labels"].([]any)
				if !ok || len(labels) != 2 || labels[0] != "rss" || labels[1] != "tv" {
					t.Fatalf("labels=%v", args["labels"])
				}
				if modern {
					return reply(200, `{"jsonrpc":"2.0","result":{"torrent_added":{"hash_string":"`+hash+`"}},"id":1}`), nil
				}
				return reply(200, `{"result":"success","arguments":{"torrent-added":{"hashString":"`+hash+`"}}}`), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.AddMagnetWithOptions(context.Background(), magnet, AddOptions{DownloadDir: "/downloads/tv", Paused: true, Labels: []string{"rss", " tv "}})
			if err != nil || got != hash {
				t.Fatalf("add=%q err=%v", got, err)
			}
		})
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (transport transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
func reply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestCheckNegotiatesLegacySessionAndUsesBasicAuth(t *testing.T) {
	calls := 0
	client, err := New("https://tr.example/prefix/", "9091", "admin", "p&=secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://tr.example:9091/prefix/transmission/rpc" || request.Method != http.MethodPost {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:p&=secret"))
		if request.Header.Get("Authorization") != wantAuth || request.Header.Get("Origin") != "https://tr.example:9091" {
			t.Fatal("missing protected request metadata")
		}
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"method":"session-get"`) {
			t.Fatalf("legacy payload = %s", body)
		}
		if calls == 1 {
			response := reply(http.StatusConflict, "session required")
			response.Header.Set("X-Transmission-Session-Id", "private-session")
			return response, nil
		}
		if request.Header.Get("X-Transmission-Session-Id") != "private-session" {
			t.Fatal("session ID not echoed")
		}
		return reply(http.StatusOK, `{"result":"success","arguments":{"version":"4.0.6"}}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil || calls != 2 {
		t.Fatalf("Check = %v calls=%d", err, calls)
	}
}

func TestCheckNegotiatesModernJSONRPC(t *testing.T) {
	calls := 0
	client, err := New("tr.example", "9091", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			response := reply(409, "")
			response.Header.Set("X-Transmission-Session-Id", "modern-session")
			response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
			return response, nil
		}
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"jsonrpc":"2.0"`) || !strings.Contains(string(body), `"method":"session_get"`) {
			t.Fatalf("modern payload = %s", body)
		}
		return reply(200, `{"jsonrpc":"2.0","result":{"version":"4.1.0"},"id":1}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil || calls != 2 {
		t.Fatalf("Check = %v calls=%d", err, calls)
	}
}

func TestCheckRejectsProtocolFailuresAndRedirects(t *testing.T) {
	fixtures := []struct {
		name        string
		firstCode   int
		firstHeader string
		secondCode  int
		secondBody  string
		want        error
	}{
		{"unauthorized", 401, "", 0, "", ErrAuthentication},
		{"redirect", 302, "", 0, "", ErrResponse},
		{"missing session", 409, "", 0, "", ErrResponse},
		{"repeated conflict", 409, "session", 409, "", ErrResponse},
		{"failed result", 409, "session", 200, `{"result":"failure: secret-password","arguments":{}}`, ErrResponse},
		{"missing version", 409, "session", 200, `{"result":"success","arguments":{}}`, ErrResponse},
		{"invalid JSON", 409, "session", 200, `<html>secret-password</html>`, ErrResponse},
		{"oversized", 409, "session", 200, strings.Repeat("x", (1<<20)+1), ErrResponse},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			calls := 0
			client, err := New("tr.example", "9091", "admin", "secret-password", transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Hostname() != "tr.example" {
					t.Fatal("credentials sent across redirect")
				}
				if calls == 1 {
					response := reply(fixture.firstCode, "secret-password")
					if fixture.firstHeader != "" {
						response.Header.Set("X-Transmission-Session-Id", fixture.firstHeader)
					}
					response.Header.Set("Location", "https://evil.example/collect")
					return response, nil
				}
				return reply(fixture.secondCode, fixture.secondBody), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = client.Check(context.Background())
			if !errors.Is(err, fixture.want) || strings.Contains(err.Error(), "secret-password") {
				t.Fatalf("Check = %v", err)
			}
		})
	}
}

func TestConfigurationAndCancellation(t *testing.T) {
	for _, host := range []string{"", "file:///etc/passwd", "http://user:password@tr.example", "http://tr.example?secret=1", "http://tr.example/#fragment"} {
		if _, err := New(host, "9091", "", "", nil); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("accepted invalid host %q", host)
		}
	}
	client, err := New("http://[::1]/prefix", "9091", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	if err != nil || client.endpoint.Host != "[::1]:9091" {
		t.Fatalf("IPv6 = %v %s", err, client.endpoint.Host)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Check(ctx); !errors.Is(err, ErrConnection) {
		t.Fatalf("cancelled Check = %v", err)
	}
}

func TestLegacyTasksAndControls(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	calls := 0
	client, err := New("tr.example", "9091", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(request.Body)
		if calls%2 == 1 {
			response := reply(http.StatusConflict, "")
			response.Header.Set("X-Transmission-Session-Id", "legacy-session")
			return response, nil
		}
		if request.Header.Get("X-Transmission-Session-Id") != "legacy-session" {
			t.Fatal("session ID not echoed")
		}
		payload := string(body)
		switch calls {
		case 2:
			if !strings.Contains(payload, `"method":"torrent-get"`) || !strings.Contains(payload, `"hashString"`) {
				t.Fatalf("task payload = %s", payload)
			}
			return reply(200, `{"result":"success","arguments":{"torrents":[{"hashString":"`+hash+`","name":"Legacy Movie","percentDone":0.42,"rateDownload":1048576,"rateUpload":1024,"eta":65,"status":4},{"hashString":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","name":"Done","percentDone":1,"rateDownload":0,"rateUpload":0,"eta":-1,"status":6}]}}`), nil
		case 4:
			if !strings.Contains(payload, `"method":"torrent-start"`) {
				t.Fatalf("start payload = %s", payload)
			}
		case 6:
			if !strings.Contains(payload, `"method":"torrent-stop"`) {
				t.Fatalf("stop payload = %s", payload)
			}
		case 8:
			if !strings.Contains(payload, `"method":"torrent-remove"`) || !strings.Contains(payload, `"delete-local-data":true`) {
				t.Fatalf("remove payload = %s", payload)
			}
		}
		if !strings.Contains(payload, hash) {
			t.Fatalf("missing hash: %s", payload)
		}
		return reply(200, `{"result":"success","arguments":{}}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.Tasks(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].Hash != hash || tasks[0].Progress != 0.42 || tasks[0].DownloadSpeed != 1048576 {
		t.Fatalf("Tasks = %#v, %v", tasks, err)
	}
	for _, action := range []string{"start", "stop", "remove"} {
		if err := client.Control(context.Background(), hash, action); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	before := calls
	for _, invalid := range []struct{ hash, action string }{{"all", "stop"}, {hash, "erase"}} {
		if err := client.Control(context.Background(), invalid.hash, invalid.action); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("invalid control = %v", err)
		}
	}
	if calls != before {
		t.Fatal("invalid control reached Transmission")
	}
}

func TestModernTasksAndRemove(t *testing.T) {
	const hash = "abcdef0123456789abcdef0123456789abcdef01"
	calls := 0
	client, err := New("tr.example", "9091", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(request.Body)
		if calls%2 == 1 {
			response := reply(http.StatusConflict, "")
			response.Header.Set("X-Transmission-Session-Id", "modern-session")
			response.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
			return response, nil
		}
		payload := string(body)
		if calls == 2 {
			if !strings.Contains(payload, `"method":"torrent_get"`) || !strings.Contains(payload, `"hash_string"`) {
				t.Fatalf("modern task payload = %s", payload)
			}
			return reply(200, `{"jsonrpc":"2.0","result":{"torrents":[{"hash_string":"`+hash+`","name":"Paused","percent_done":0.25,"rate_download":0,"rate_upload":0,"eta":-1,"status":0}]},"id":1}`), nil
		}
		if !strings.Contains(payload, `"method":"torrent_remove"`) || !strings.Contains(payload, `"delete_local_data":true`) || !strings.Contains(payload, hash) {
			t.Fatalf("modern remove payload = %s", payload)
		}
		return reply(200, `{"jsonrpc":"2.0","result":{},"id":1}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.Tasks(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].Status != 0 {
		t.Fatalf("Tasks = %#v, %v", tasks, err)
	}
	if err := client.Control(context.Background(), hash, "remove"); err != nil {
		t.Fatal(err)
	}
}

func TestTasksRejectMalformedResponses(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, body := range []string{
		`{"result":"success","arguments":{}}`,
		`{"result":"success","arguments":{"torrents":[{"hashString":"bad","name":"Movie","percentDone":0.5,"rateDownload":0,"rateUpload":0,"eta":0,"status":4}]}}`,
		`{"result":"success","arguments":{"torrents":[{"hashString":"` + hash + `","name":"Movie","percentDone":1.5,"rateDownload":0,"rateUpload":0,"eta":0,"status":4}]}}`,
	} {
		client, err := New("tr.example", "9091", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
			return reply(200, body), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Tasks(context.Background()); !errors.Is(err, ErrResponse) {
			t.Fatalf("accepted malformed response: %v", err)
		}
	}
}
