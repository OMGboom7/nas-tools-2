package qbittorrent

import (
	"context"
	"errors"
	"io"
	"math"
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

func TestCheckLogsInThenReadsStatusWithIsolatedCookie(t *testing.T) {
	calls := 0
	client, err := New("https://qb.example/prefix/", "8443", "admin", "p&=secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "qb.example:8443" || request.Header.Get("Origin") != "https://qb.example:8443" || request.Header.Get("Authorization") != "" {
			t.Fatalf("incorrect request metadata: %s", request.URL)
		}
		switch request.URL.Path {
		case "/prefix/api/v2/auth/login":
			if request.Method != http.MethodPost {
				t.Fatal("login method")
			}
			if err := request.ParseForm(); err != nil || request.Form.Get("password") != "p&=secret" || request.Form.Get("username") != "admin" {
				t.Fatal("invalid login form")
			}
			response := reply(200, "Ok.")
			response.Header.Set("Set-Cookie", "SID=private-session; Path=/; Secure; HttpOnly")
			return response, nil
		case "/prefix/api/v2/transfer/info":
			cookie, err := request.Cookie("SID")
			if request.Method != http.MethodGet || err != nil || cookie.Value != "private-session" {
				t.Fatal("missing authenticated session")
			}
			return reply(200, `{"dl_info_speed":0,"up_info_speed":10}`), nil
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Check(context.Background()); err != nil || calls != 2 {
		t.Fatalf("Check: %v calls=%d", err, calls)
	}
}

func TestCheckRejectsFailuresWithoutLeakingResponse(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		loginCode  int
		loginBody  string
		statusBody string
		want       error
	}{
		{"bad login", 200, "Fails. secret-password", "", ErrAuthentication},
		{"forbidden", 403, "secret-password", "", ErrAuthentication},
		{"redirect", 307, "secret-password", "", ErrResponse},
		{"HTML status", 200, "Ok.", "<html>secret-password</html>", ErrResponse},
		{"null status", 200, "Ok.", "null", ErrResponse},
		{"empty status", 200, "Ok.", "{}", ErrResponse},
		{"oversized status", 200, "Ok.", strings.Repeat("x", (1<<20)+1), ErrResponse},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			calls := 0
			client, err := New("qb.example", "8080", "admin", "secret-password", transportFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if strings.HasSuffix(request.URL.Path, "auth/login") {
					response := reply(fixture.loginCode, fixture.loginBody)
					response.Header.Set("Location", "https://evil.example/collect")
					return response, nil
				}
				if request.URL.Hostname() != "qb.example" {
					t.Fatal("followed redirect with credentials")
				}
				return reply(200, fixture.statusBody), nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = client.Check(context.Background())
			if !errors.Is(err, fixture.want) || strings.Contains(err.Error(), "secret-password") {
				t.Fatalf("Check = %v", err)
			}
			if fixture.loginCode != 200 && calls != 1 {
				t.Fatalf("retried failure: %d", calls)
			}
		})
	}
}

func TestConfigurationAndCancellation(t *testing.T) {
	for _, host := range []string{"", "file:///etc/passwd", "http://user:password@qb.example", "http://qb.example?secret=1", "http://qb.example/#fragment"} {
		if _, err := New(host, "8080", "", "", nil); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("accepted invalid address %q", host)
		}
	}
	client, err := New("http://[::1]/prefix", "8080", "", "", transportFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}))
	if err != nil || client.base.Host != "[::1]:8080" {
		t.Fatalf("IPv6: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Check(ctx); !errors.Is(err, ErrConnection) {
		t.Fatalf("cancelled check: %v", err)
	}
}

func TestTasksAndControlsUseSingleValidatedHash(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	paths := []string{}
	client, err := New("qb.example", "8080", "admin", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.RequestURI())
		if strings.HasSuffix(request.URL.Path, "/auth/login") {
			response := reply(200, "Ok.")
			response.Header.Set("Set-Cookie", "SID=session; Path=/")
			return response, nil
		}
		if _, err := request.Cookie("SID"); err != nil {
			t.Fatal("authenticated request has no session")
		}
		switch {
		case strings.HasSuffix(request.URL.Path, "/torrents/info"):
			if request.URL.Query().Get("filter") != "downloading" {
				t.Fatalf("filter = %q", request.URL.Query().Get("filter"))
			}
			return reply(200, `[{"hash":"`+hash+`","name":"Movie","progress":0.25,"dlspeed":1024,"upspeed":5,"eta":65,"state":"pausedDL"}]`), nil
		case strings.HasSuffix(request.URL.Path, "/torrents/resume"), strings.HasSuffix(request.URL.Path, "/torrents/pause"):
			if err := request.ParseForm(); err != nil || request.Form.Get("hashes") != hash {
				t.Fatalf("control form = %v", request.Form)
			}
			return reply(200, ""), nil
		case strings.HasSuffix(request.URL.Path, "/torrents/delete"):
			if err := request.ParseForm(); err != nil || request.Form.Get("hashes") != hash || request.Form.Get("deleteFiles") != "true" {
				t.Fatalf("delete form = %v", request.Form)
			}
			return reply(200, ""), nil
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
			return nil, nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := client.Tasks(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].Hash != hash || tasks[0].State != "pausedDL" {
		t.Fatalf("Tasks = %+v, %v", tasks, err)
	}
	for _, action := range []string{"start", "stop", "remove"} {
		if err := client.Control(context.Background(), hash, action); err != nil {
			t.Fatalf("Control(%s) = %v", action, err)
		}
	}
	before := len(paths)
	for _, invalid := range []string{"", "all", hash + "|" + hash, strings.Repeat("a", 39), strings.Repeat("a", 41), strings.Repeat("g", 40)} {
		if err := client.Control(context.Background(), invalid, "stop"); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("accepted hash %q: %v", invalid, err)
		}
	}
	if err := client.Control(context.Background(), hash, "unknown"); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("accepted action: %v", err)
	}
	if len(paths) != before {
		t.Fatal("invalid control reached qBittorrent")
	}
}

func TestTasksRejectMalformedPayload(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	for _, body := range []string{"null", `{}`, `[{"hash":"bad","name":"Movie","progress":0.2}]`, `[{"hash":"` + hash + `","name":"","progress":0.2}]`, `[{"hash":"` + hash + `","name":"Movie","progress":1.1}]`, `[{"hash":"` + hash + `","name":"Movie","progress":0.2,"dlspeed":-1}]`} {
		client, err := New("qb.example", "8080", "admin", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "/auth/login") {
				return reply(200, "Ok."), nil
			}
			return reply(200, body), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Tasks(context.Background()); !errors.Is(err, ErrResponse) {
			t.Fatalf("accepted payload %s: %v", body, err)
		}
	}
}

func TestAddMagnetRequiresAuthenticatedSuccess(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	calls := 0
	client, err := New("qb.example", "8080", "admin", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if strings.HasSuffix(request.URL.Path, "/auth/login") {
			return reply(200, "Ok."), nil
		}
		if request.URL.Path != "/api/v2/torrents/add" || request.ParseForm() != nil || request.Form.Get("urls") != magnet {
			t.Fatalf("bad add request: %s %v", request.URL, request.Form)
		}
		return reply(200, "Ok."), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AddMagnet(context.Background(), magnet); err != nil || calls != 2 {
		t.Fatalf("add = %v, calls = %d", err, calls)
	}
	if err := client.AddMagnet(context.Background(), "https://example.com"); !errors.Is(err, ErrConfiguration) || calls != 2 {
		t.Fatalf("invalid add = %v, calls = %d", err, calls)
	}
}

func TestAddWithOptionsSendsPerTorrentSettings(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	options := AddOptions{SavePath: "/downloads/movies", Category: "films", Tags: []string{"rss", " 4k "}, Paused: true, UploadLimitKB: 100, DownloadLimitKB: 200, RatioLimit: 1.5, SeedingTimeLimit: 60}
	calls := 0
	client, err := New("qb.example", "8080", "admin", "secret", transportFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/auth/login") {
			return reply(200, "Ok."), nil
		}
		calls++
		if calls == 1 {
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.Form.Get("urls") != magnet {
				t.Fatalf("magnet=%q", request.Form.Get("urls"))
			}
		} else {
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			file, _, err := request.FormFile("torrents")
			if err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(file)
			file.Close()
			if err != nil || string(contents) != "torrent-content" {
				t.Fatalf("torrent=%q err=%v", contents, err)
			}
		}
		for key, want := range map[string]string{"savepath": "/downloads/movies", "autoTMM": "false", "category": "films", "tags": "rss,4k", "paused": "true", "upLimit": "102400", "dlLimit": "204800", "ratioLimit": "1.5", "seedingTimeLimit": "60"} {
			if got := request.Form.Get(key); got != want {
				t.Fatalf("%s=%q want %q", key, got, want)
			}
		}
		return reply(200, "Ok."), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AddMagnetWithOptions(context.Background(), magnet, options); err != nil {
		t.Fatal(err)
	}
	if err := client.AddTorrentWithOptions(context.Background(), []byte("torrent-content"), options); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("add calls=%d", calls)
	}
	if err := client.AddMagnetWithOptions(context.Background(), magnet, AddOptions{Tags: []string{"bad,tag"}}); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("invalid tags=%v", err)
	}
	if err := client.AddMagnetWithOptions(context.Background(), magnet, AddOptions{RatioLimit: math.NaN()}); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("invalid ratio=%v", err)
	}
}
