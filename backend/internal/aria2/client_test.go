package aria2

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (transport transportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
func reply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestAddMagnetUsesTokenAndValidatesGID(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	client, err := New("aria.example", "6800", "private-token", transportFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Method != "aria2.addUri" || len(payload.Params) != 3 {
			t.Fatalf("payload: %#v", payload)
		}
		var token string
		var uris []string
		if json.Unmarshal(payload.Params[0], &token) != nil || json.Unmarshal(payload.Params[1], &uris) != nil || token != "token:private-token" || len(uris) != 1 || uris[0] != magnet {
			t.Fatalf("params: %#v", payload.Params)
		}
		return reply(200, `{"jsonrpc":"2.0","id":"nastool-add","result":"0123456789abcdef"}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := client.AddMagnet(context.Background(), magnet); err != nil || got != "0123456789abcdef" {
		t.Fatalf("add = %q, %v", got, err)
	}
}

func TestAddMagnetWithOptionsSendsPerDownloadSettings(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	client, err := New("aria.example", "6800", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Method != "aria2.addUri" || len(payload.Params) != 3 {
			t.Fatalf("payload=%+v", payload)
		}
		var options map[string]string
		if err := json.Unmarshal(payload.Params[2], &options); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"dir": "/downloads/tv", "pause": "true", "max-upload-limit": "102400", "max-download-limit": "204800"} {
			if options[key] != want {
				t.Fatalf("%s=%q want %q", key, options[key], want)
			}
		}
		return reply(200, `{"jsonrpc":"2.0","id":"nastool-add","result":"0123456789abcdef"}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.AddMagnetWithOptions(context.Background(), magnet, AddOptions{DownloadDir: "/downloads/tv", Paused: true, UploadLimitKB: 100, DownloadLimitKB: 200})
	if err != nil || got != "0123456789abcdef" {
		t.Fatalf("add=%q err=%v", got, err)
	}
	if _, err := client.AddMagnetWithOptions(context.Background(), magnet, AddOptions{UploadLimitKB: -1}); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("invalid limit=%v", err)
	}
}

func TestCheckUsesMethodTokenAndReadsVersion(t *testing.T) {
	client, err := New("https://aria.example/prefix/", "6800", "p&=secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://aria.example:6800/prefix/jsonrpc" || request.Header.Get("Origin") != "https://aria.example:6800" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("secret duplicated into authorization header")
		}
		var payload struct {
			JSONRPC, ID, Method string
			Params              []string
		}
		if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.JSONRPC != "2.0" || payload.ID != "nastool-check" || payload.Method != "aria2.getVersion" || len(payload.Params) != 1 || payload.Params[0] != "token:p&=secret" {
			t.Fatalf("payload = %+v", payload)
		}
		return reply(200, `{"jsonrpc":"2.0","id":"nastool-check","result":{"version":"1.37.0","enabledFeatures":[]}}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRejectsFailuresWithoutFollowingRedirects(t *testing.T) {
	fixtures := []struct {
		name string
		code int
		body string
		want error
	}{
		{"unauthorized", 401, "secret-token", ErrAuthentication},
		{"redirect", 307, "secret-token", ErrResponse},
		{"rpc error", 200, `{"jsonrpc":"2.0","id":"nastool-check","error":{"code":1,"message":"secret-token"}}`, ErrResponse},
		{"wrong ID", 200, `{"jsonrpc":"2.0","id":"other","result":{"version":"1.37.0"}}`, ErrResponse},
		{"empty version", 200, `{"jsonrpc":"2.0","id":"nastool-check","result":{"version":""}}`, ErrResponse},
		{"invalid JSON", 200, "<html>secret-token</html>", ErrResponse},
		{"oversized", 200, strings.Repeat("x", (1<<20)+1), ErrResponse},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			calls := 0
			client, err := New("aria.example", "6800", "secret-token", transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Hostname() != "aria.example" {
					t.Fatal("followed redirect with token")
				}
				response := reply(fixture.code, fixture.body)
				response.Header.Set("Location", "https://evil.example/collect")
				return response, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = client.Check(context.Background())
			if !errors.Is(err, fixture.want) || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("Check = %v", err)
			}
			if calls != 1 {
				t.Fatalf("calls = %d", calls)
			}
		})
	}
}

func TestConfigurationAndCancellation(t *testing.T) {
	for _, host := range []string{"", "file:///etc/passwd", "http://user:password@aria.example", "http://aria.example?secret=1", "http://aria.example/#fragment"} {
		if _, err := New(host, "6800", "secret", nil); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("accepted host %q", host)
		}
	}
	if _, err := New("aria.example", "0", "secret", nil); !errors.Is(err, ErrConfiguration) {
		t.Fatal("accepted port zero")
	}
	if _, err := New("aria.example", "6800", "", nil); !errors.Is(err, ErrConfiguration) {
		t.Fatal("accepted empty secret")
	}
	client, err := New("http://[::1]/prefix", "6800", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	if err != nil || client.endpoint.Host != "[::1]:6800" {
		t.Fatalf("IPv6 = %v %s", err, client.endpoint.Host)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Check(ctx); !errors.Is(err, ErrConnection) {
		t.Fatalf("cancelled Check = %v", err)
	}
}

func TestTasksAndControls(t *testing.T) {
	const activeGID = "0123456789abcdef"
	const pausedGID = "abcdef0123456789"
	calls := 0
	client, err := New("aria.example", "6800", "server-secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			JSONRPC, ID, Method string
			Params              []json.RawMessage
		}
		if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.JSONRPC != "2.0" || len(payload.Params) == 0 || string(payload.Params[0]) != `"token:server-secret"` {
			t.Fatalf("payload = %+v", payload)
		}
		switch payload.Method {
		case "aria2.tellActive":
			if payload.ID != "nastool-active" || len(payload.Params) != 2 || !strings.Contains(string(payload.Params[1]), `"bittorrent"`) {
				t.Fatalf("active payload = %+v", payload)
			}
			return reply(200, `{"jsonrpc":"2.0","id":"nastool-active","result":[{"gid":"`+activeGID+`","status":"active","totalLength":"1000","completedLength":"425","downloadSpeed":"1048576","uploadSpeed":"1024","bittorrent":{"info":{"name":"Aria Movie"}},"files":[]}]}`), nil
		case "aria2.tellWaiting":
			if payload.ID != "nastool-waiting" || len(payload.Params) != 4 || string(payload.Params[1]) != "-1" || string(payload.Params[2]) != "100" {
				t.Fatalf("waiting payload = %+v", payload)
			}
			return reply(200, `{"jsonrpc":"2.0","id":"nastool-waiting","result":[{"gid":"`+pausedGID+`","status":"paused","totalLength":"2000","completedLength":"1000","downloadSpeed":"0","uploadSpeed":"0","files":[{"path":"/downloads/Paused.File.mkv"}]}]}`), nil
		case "aria2.unpause", "aria2.pause", "aria2.remove":
			if payload.ID != "nastool-control" || len(payload.Params) != 2 || string(payload.Params[1]) != `"`+activeGID+`"` {
				t.Fatalf("control payload = %+v", payload)
			}
			return reply(200, `{"jsonrpc":"2.0","id":"nastool-control","result":"`+activeGID+`"}`), nil
		default:
			t.Fatalf("unexpected method: %s", payload.Method)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.Tasks(context.Background())
	if err != nil || len(tasks) != 2 || tasks[0].Name != "Aria Movie" || tasks[0].Progress != 0.425 || tasks[1].Name != "Paused.File.mkv" || tasks[1].Status != "paused" {
		t.Fatalf("Tasks = %#v, %v", tasks, err)
	}
	for _, action := range []string{"start", "stop", "remove"} {
		if err := client.Control(context.Background(), activeGID, action); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	before := calls
	for _, invalid := range []struct{ gid, action string }{{"all", "stop"}, {activeGID, "erase"}} {
		if err := client.Control(context.Background(), invalid.gid, invalid.action); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("invalid control = %v", err)
		}
	}
	if calls != before {
		t.Fatal("invalid control reached Aria2")
	}
}

func TestTasksRejectMalformedResponses(t *testing.T) {
	fixtures := []struct {
		active, waiting string
	}{
		{`null`, `[]`},
		{`[{"gid":"bad","status":"active","totalLength":"1","completedLength":"0","downloadSpeed":"0","uploadSpeed":"0"}]`, `[]`},
		{`[{"gid":"0123456789abcdef","status":"active","totalLength":"1","completedLength":"2","downloadSpeed":"0","uploadSpeed":"0"}]`, `[]`},
		{`[]`, `[{"gid":"0123456789abcdef","status":"unknown","totalLength":"1","completedLength":"0","downloadSpeed":"0","uploadSpeed":"0"}]`},
	}
	for _, fixture := range fixtures {
		client, err := New("aria.example", "6800", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
			var payload struct{ ID string }
			_ = json.NewDecoder(request.Body).Decode(&payload)
			body := fixture.active
			if payload.ID == "nastool-waiting" {
				body = fixture.waiting
			}
			return reply(200, `{"jsonrpc":"2.0","id":"`+payload.ID+`","result":`+body+`}`), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Tasks(context.Background()); !errors.Is(err, ErrResponse) {
			t.Fatalf("accepted malformed response: %v", err)
		}
	}
}
