package builtinindexer

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMTeamFileCommandSignedLinksAndTemporaryFiles(t *testing.T) {
	const raw = "https://api.m-team.info/api/rss/dl/42?ticket=a%2Fb+c"
	const torrent = "d4:infod4:name4:Testee"
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
	body, disposition := torrent, `attachment; filename*=UTF-8''%E5%BD%B1%E7%89%87.torrent`
	status := 200
	foreignRedirect := false
	calls := 0
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = fakeTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "api.m-team.info" || r.Method != "GET" || r.UserAgent() != "Fixture" || r.Header.Get("Cookie") != "" || r.Header.Get("x-api-key") != "" || r.Header.Get("Referer") != "" {
			t.Fatal("unexpected recipient or credentials")
		}
		h := http.Header{}
		code := status
		if r.URL.Path == "/api/rss/dl/42" {
			if r.URL.RawQuery != "ticket=a%2Fb+c" {
				t.Fatal("signed query changed")
			}
			code = 302
			if foreignRedirect {
				h.Set("Location", "https://outside.example/download")
			} else {
				h.Set("Location", "/file")
			}
		} else if r.URL.Path == "/file" {
			h.Set("Content-Disposition", disposition)
		} else {
			t.Fatal(r.URL.Path)
		}
		return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	dir := t.TempDir()
	run := func() (map[string]any, error) {
		b, _ := json.Marshal(map[string]string{"url": raw, "ua": "Fixture", "key": "must-not-send", "directory": dir})
		var out bytes.Buffer
		err := MTeamDownloadCommand(t.Context(), bytes.NewReader(b), &out)
		if err != nil {
			return nil, err
		}
		var result map[string]any
		err = json.Unmarshal(out.Bytes(), &result)
		return result, err
	}
	first, e := run()
	if e != nil || first["path"] != filepath.Join(dir, "影片.torrent") {
		t.Fatal(first, e)
	}
	second, e := run()
	if e != nil || second["path"] == first["path"] {
		t.Fatal(second, e)
	}
	for _, f := range []map[string]any{first, second} {
		data, err := os.ReadFile(f["path"].(string))
		if err != nil || string(data) != torrent {
			t.Fatal(err)
		}
	}
	disposition = `attachment; filename="..%2Foutside.torrent"`
	third, e := run()
	if e != nil || third["path"] != filepath.Join(dir, "Test.torrent") {
		t.Fatal(third, e)
	}
	body = magnet
	if reply, e := run(); e != nil || reply["magnet"] != magnet {
		t.Fatal(reply, e)
	}
	body = "<html>login</html>"
	if _, e := run(); e == nil {
		t.Fatal("invalid torrent accepted")
	}
	status = 429
	if reply, e := run(); e != nil || reply["status"] != float64(429) {
		t.Fatal(reply, e)
	}
	foreignRedirect = true
	before := calls
	if reply, e := run(); e != nil || reply["status"] != float64(302) || calls != before+1 {
		t.Fatal(reply, e, calls)
	}
	files, e := os.ReadDir(dir)
	if e != nil || len(files) != 3 {
		t.Fatal(files, e)
	}
}

func TestMTeamDownloadOriginScope(t *testing.T) {
	const front = "https://kp.m-team.cc/signin"
	for _, raw := range []string{"https://kp.m-team.cc/detail/42?hit=1", "https://api.m-team.cc/api/rss/dl?token=A", "https://kp.m-team.cc/api/rss/dl/42"} {
		if !MTeamDownloadMatches(front, raw) {
			t.Error(raw)
		}
	}
	for _, raw := range []string{"https://api.m-team.io/api/rss/dl?token=A", "https://xp.m-team.cc/detail/42", "http://kp.m-team.cc/detail/42", "https://api.m-team.cc.example/api/rss/dl", "https://user@api.m-team.cc/api/rss/dl", "https://api.m-team.cc:9000/api/rss/dl", "https://api.m-team.cc/api/rss/dl/../member", "https://kp.m-team.cc/detail/00", "https://kp.m-team.cc/detail/42#tag"} {
		if MTeamDownloadMatches(front, raw) {
			t.Error(raw)
		}
	}
	if MTeamDownloadMatches("https://kp.m-team.info/signin", "https://api.m-team.cc/api/rss/dl") || !MTeamDownloadMatches("https://kp.m-team.info/signin", "https://api.m-team.info/api/rss/dl") {
		t.Fatal("catalog info origin mixed with cc")
	}
	// Absolute token URLs in an RSS query never become the chosen destination.
	u, _ := url.Parse("https://example.org/article?link=https://api.m-team.cc/api/rss/dl")
	if IsMTeamDownload(u.String()) {
		t.Fatal("foreign origin classified as M-Team")
	}
}
