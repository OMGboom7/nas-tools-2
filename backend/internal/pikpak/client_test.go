package pikpak

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"

func mockClient(t *testing.T, operation func(*http.Request) (*http.Response, error)) (*Client, *int) {
	t.Helper()
	calls := 0
	c, err := New("user@example.com", "private-password", "", roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Scheme != "https" || r.Header.Get("X-Device-Id") == "" {
			t.Fatal("unsafe destination/header", r.URL)
		}
		var data map[string]any
		if r.URL.Host == "user.mypikpak.com" {
			if r.Header.Get("Authorization") != "" || r.Method != "POST" {
				t.Fatal("authorization leaked to signin")
			}
			if json.NewDecoder(r.Body).Decode(&data) != nil {
				t.Fatal("invalid auth request")
			}
			switch r.URL.Path {
			case "/v1/shield/captcha/init":
				if data["action"] != "POST:"+accountURL+"/v1/auth/signin" || data["client_id"] != clientID || data["device_id"] != r.Header.Get("X-Device-Id") || data["meta"].(map[string]any)["email"] != "user@example.com" {
					t.Fatal("invalid challenge scope")
				}
				return reply(r, `{"captcha_token":"challenge-secret"}`), nil
			case "/v1/auth/signin":
				if data["password"] != "private-password" || data["captcha_token"] != "challenge-secret" || data["client_secret"] != clientSecret {
					t.Fatal("invalid signin scope")
				}
				return reply(r, `{"access_token":"access-secret","sub":"User_1","token_type":"Bearer"}`), nil
			}
			t.Fatal("unexpected auth destination", r.URL)
		}
		if r.URL.Host != "api-drive.mypikpak.com" || r.Header.Get("Authorization") != "Bearer access-secret" {
			t.Fatal("wrong drive authorization")
		}
		return operation(r)
	}))
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestAccountTasksAndSubmissionPreserveCaseAndPagination(t *testing.T) {
	page, adds := 0, 0
	c, calls := mockClient(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			if r.URL.Path != "/drive/v1/tasks" || r.URL.Query().Get("type") != "offline" || r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("filters") != `{"phase":{"in":"PHASE_TYPE_RUNNING,PHASE_TYPE_ERROR,PHASE_TYPE_PENDING"}}` {
				t.Fatal("incorrect list scope")
			}
			page++
			if page == 1 {
				return reply(r, `{"tasks":[{"id":"Task_A","file_name":"Movie.mkv","phase":"PHASE_TYPE_RUNNING"}],"next_page_token":"opaque+/=?token"}`), nil
			}
			if r.URL.Query().Get("page_token") != "opaque+/=?token" {
				t.Fatal("changed page token")
			}
			return reply(r, `{"tasks":[{"id":"Task_B","file_name":"Pending.mkv","phase":"PHASE_TYPE_PENDING"},{"id":"Task_C","file_name":"Failed.mkv","phase":"PHASE_TYPE_ERROR"}]}`), nil
		}
		adds++
		if r.Method != "POST" || r.URL.Path != "/drive/v1/files" {
			t.Fatal("unexpected mutation", r.URL)
		}
		var data map[string]any
		if json.NewDecoder(r.Body).Decode(&data) != nil {
			t.Fatal("bad add")
		}
		if data["kind"] != "drive#file" || data["upload_type"] != "UPLOAD_TYPE_URL" || data["parent_id"] != "Folder_A" || data["folder_type"] != "" || data["url"].(map[string]any)["url"] != magnet {
			t.Fatal("changed add options")
		}
		return reply(r, `{"task":{"id":"Task_CaseSensitive"}}`), nil
	})
	if err := c.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	tasks, err := c.Tasks(t.Context())
	if err != nil || len(tasks) != 3 || tasks[0].ID != "Task_A" || tasks[2].Phase != "PHASE_TYPE_ERROR" || *calls != 4 {
		t.Fatal(tasks, err, *calls)
	}
	id, err := c.AddMagnet(t.Context(), magnet, "Folder_A")
	if err != nil || id != "Task_CaseSensitive" || adds != 1 || *calls != 5 {
		t.Fatal(id, err, adds, *calls)
	}
}

func TestTasksRejectMalformedPartialAndLoopingResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"tasks":null}`, `{"tasks":[{"id":"bad/id","file_name":"a","phase":"PHASE_TYPE_RUNNING"}]}`, `{"tasks":[{"id":"Task","file_name":"a","phase":"unknown"}]}`, `{"tasks":[{"id":"Task","file_name":"","phase":"PHASE_TYPE_RUNNING"}]}`, `{"tasks":[],"next_page_token":"repeat"}`, `{"error":"denied","error_description":"private-password"}`, `[]`, strings.Repeat("x", (4<<20)+1)} {
		t.Run(body[:min(len(body), 40)], func(t *testing.T) {
			c, calls := mockClient(t, func(r *http.Request) (*http.Response, error) { return reply(r, body), nil })
			rows, err := c.Tasks(t.Context())
			if err == nil || rows != nil || *calls > 4 || strings.Contains(err.Error(), "private-password") {
				t.Fatal("unsafe partial or unbounded response", len(rows), err, *calls)
			}
		})
	}
}

func TestSubmissionNeverRetriesAmbiguousErrors(t *testing.T) {
	for _, mode := range []string{"network", "401", "redirect", "invalid", "missing", "upstream", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			adds := 0
			c, calls := mockClient(t, func(r *http.Request) (*http.Response, error) {
				adds++
				switch mode {
				case "network":
					return nil, errors.New("private-password in network diagnostic")
				case "401":
					resp := reply(r, `{"error":"private-password"}`)
					resp.StatusCode = 401
					return resp, nil
				case "redirect":
					resp := reply(r, "")
					resp.StatusCode = 302
					resp.Header.Set("Location", "https://other.test/")
					return resp, nil
				case "invalid":
					return reply(r, `{"task":{"id":"bad/id"}}`), nil
				case "missing":
					return reply(r, `{}`), nil
				case "upstream":
					return reply(r, `{"error":"secret","error_description":"private-password"}`), nil
				default:
					return reply(r, strings.Repeat("x", (4<<20)+1)), nil
				}
			})
			id, err := c.AddMagnet(t.Context(), magnet, "")
			if err == nil || errors.Is(err, ErrNotSubmitted) || id != "" || adds != 1 || *calls != 3 || strings.Contains(err.Error(), "private-password") {
				t.Fatal(id, err, adds, *calls)
			}
		})
	}
}

func TestAccountVerificationIsNotBypassed(t *testing.T) {
	for _, body := range []string{`{}`, `{"captcha_token":"token","url":"https://challenge.test/"}`, `{"error":"invalid_account_or_password","error_description":"private-password"}`, `{"captcha_token":"bad\r\ntoken"}`} {
		calls := 0
		c, _ := New("user", "private-password", "", roundTrip(func(r *http.Request) (*http.Response, error) { calls++; return reply(r, body), nil }))
		if err := c.Check(t.Context()); err == nil || calls != 1 || strings.Contains(err.Error(), "private-password") {
			t.Fatal("challenge bypass", err, calls)
		}
	}
}

func TestInvalidConfigurationAndOptionsDoNotUseNetwork(t *testing.T) {
	transport := roundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })
	for _, args := range [][3]string{{"", "password", ""}, {"user", "", ""}, {"user\n", "password", ""}, {"user", "password", "ftp://proxy.test"}, {"user", "password", "proxy.test/path"}, {"user", "password", "http://proxy.test?secret=x"}, {"user", "password", "proxy.test"}} {
		if _, err := New(args[0], args[1], args[2], transport); err == nil {
			t.Fatal("accepted config")
		}
	}
	c, _ := New("user", "password", "", transport)
	for _, args := range [][2]string{{magnet, "/NAS/path"}, {"https://tracker.test/secret", ""}, {magnet + "\r\n", ""}} {
		if _, err := c.AddMagnet(t.Context(), args[0], args[1]); !errors.Is(err, ErrConfiguration) {
			t.Fatal(err)
		}
	}
}

func TestProxyRoutingAndRedirectContainment(t *testing.T) {
	requests := make(chan *http.Request, 1)
	base := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "proxy.test:7890" {
			return nil, errors.New("proxy bypassed")
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			r, err := http.ReadRequest(bufio.NewReader(server))
			if err == nil {
				requests <- r
			}
			_, _ = io.WriteString(server, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		}()
		return client, nil
	}}
	c, err := New("user", "password", "http://proxy.test:7890", base)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Check(t.Context()); !errors.Is(err, ErrConnection) {
		t.Fatal(err)
	}
	select {
	case r := <-requests:
		if r.Method != "CONNECT" || r.Host != "user.mypikpak.com:443" {
			t.Fatal("wrong proxy scope", r.Method, r.Host)
		}
	default:
		t.Fatal("proxy was not used")
	}
	if base.Proxy != nil {
		t.Fatal("shared transport was mutated")
	}
	for _, address := range []string{"proxy.test:7890", "https://proxy.test:7890", "socks5://proxy.test:1080"} {
		c, err := New("user", "password", address, nil)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse("https://user.mypikpak.com")
		p, err := c.http.Transport.(*http.Transport).Proxy(&http.Request{URL: u})
		if err != nil || p.Hostname() != "proxy.test" {
			t.Fatal("proxy ignored")
		}
	}
}
