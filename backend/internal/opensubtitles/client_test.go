package opensubtitles

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	_ "modernc.org/sqlite"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func testResponse(r *http.Request, status int, body []byte, headers http.Header) *http.Response {
	if headers == nil {
		headers = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(bytes.NewReader(body)), Request: r}
}
func zipBody(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for name, data := range files {
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return out.Bytes()
}
func testSteps(t *testing.T, ops *[]OpenTestStep) Step {
	t.Helper()
	return func(_ context.Context, kind, sig string) (func(string, string) error, error) {
		if !Hex(sig, 64) {
			t.Error("request intent must hold only a stable signature")
		}
		index := len(*ops)
		*ops = append(*ops, OpenTestStep{Kind: kind, State: "intent", Signature: sig})
		return func(state, digest string) error {
			(*ops)[index].State = state
			(*ops)[index].Digest = digest
			return nil
		}, nil
	}
}

type OpenTestStep struct{ Kind, State, Signature, Digest string }

func TestOriginalOpenSubtitlesConfigurationNeedsIndependentGrantAndPreservesRawIntegers(t *testing.T) {
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(`CREATE TABLE SYSTEM_DICT(ID INTEGER PRIMARY KEY,TYPE TEXT,KEY TEXT,VALUE TEXT);INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','UserInstalledPlugins','["OpenSubtitles","unknown-plugin"]')`); e != nil {
		t.Fatal(e)
	}
	raw := `{"enable":true,"unknown":9007199254740993}`
	if _, e = db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, ConfigKey, raw); e != nil {
		t.Fatal(e)
	}
	_, _, wanted, ready, e := Current(t.Context(), db)
	if e != nil || !wanted || ready {
		t.Fatal(wanted, ready, e)
	}
	control := Control{Version: 1, Epoch: strings.Repeat("e", 32), Actor: "root", Authority: bus.Hash("root-authority"), Config: bus.Hash(raw), Authorized: true, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	encoded, _ := json.Marshal(control)
	if _, e = db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, ControlKey, string(encoded)); e != nil {
		t.Fatal(e)
	}
	c, scope, wanted, ready, e := Current(t.Context(), db)
	if e != nil || !wanted || !ready || c.Provider != Legacy || !scope.Valid() {
		t.Fatal(c.Provider, scope, wanted, ready, e)
	}
	public, _ := json.Marshal(scope)
	if bytes.Contains(public, []byte("9007199254740993")) {
		t.Fatal("raw configuration leaked into definition")
	}
	if _, e = db.Exec(`UPDATE SYSTEM_DICT SET VALUE=REPLACE(VALUE,'9007199254740993','9007199254740992')WHERE KEY=?`, ConfigKey); e != nil {
		t.Fatal(e)
	}
	_, _, wanted, ready, e = Current(t.Context(), db)
	if e != nil || !wanted || ready {
		t.Fatal("raw integer change adopted old grant", wanted, ready, e)
	}
	for _, bad := range []string{`{"enable":true,"enable":false}`, `{"enable":"Y"}`, `{"enable":true,"provider":"foreign"}`, `{"enable":true,"provider":"com","api_key":"key"}`, `{"cookie":"bad\r\nvalue"}`} {
		if _, e := Parse(bad); e == nil {
			t.Fatal("ambiguous or incomplete configuration accepted", bad)
		}
	}
}
func TestOriginalHTMLMovieAndTVSelectionKeepsExactTitleYearAndSeasonEpisode(t *testing.T) {
	movie := `<table id="search_results"><tbody><tr><td><strong><a class="bnone">Movie (2026)</a></strong><br>original release</td><td></td><td></td><td></td><td><a href="/download/1">zip</a></td></tr><tr style="display:none"><td>hidden</td></tr><tr><td><strong><a class="bnone">Movie (2025)</a></strong></td><td></td><td></td><td></td><td><a href="/download/2">zip</a></td></tr></tbody></table>`
	rows, e := ParseLegacy(movie, Query{Movie: true, Name: "Movie", Year: "2026"})
	if e != nil || len(rows) != 1 || rows[0].Link != "https://www.opensubtitles.org/download/1" || rows[0].Description != "original release" {
		t.Fatal(rows, e)
	}
	rows, e = ParseLegacy(movie, Query{Movie: true, Name: "Different", Year: "2026", IMDb: "tt123"})
	if e != nil || len(rows) != 2 {
		t.Fatal("IMDb search lost original title-independent behavior", rows, e)
	}
	tv := `<table id="search_results"><tbody><tr><td><span id="season-2"><a><b>Season 2</b></a></span></td></tr><tr><td><span itemprop="episodeNumber">3</span><span itemprop="name">Episode release</span><a href="/download/3">download</a></td></tr><tr><td><span itemprop="episodeNumber">4</span><a href="/download/4">download</a></td></tr></tbody></table>`
	rows, e = ParseLegacy(tv, Query{Name: "Show", IMDb: "tt123", Seasons: []int{2}, Episodes: []int{3}})
	if e != nil || len(rows) != 1 || rows[0].Season != 2 || rows[0].Episode != 3 {
		t.Fatal(rows, e)
	}
	if _, e = ParseLegacy("<html>verify you are human</html>", Query{Movie: true, Name: "Movie"}); !errors.Is(e, ErrChallenge) {
		t.Fatal(e)
	}
	if _, e = ParseLegacy(tv, Query{Name: "Show"}); e == nil {
		t.Fatal("TV without actual IMDb adopted keyword search")
	}
}
func TestZIPSelectionNeverExtractsMemberPathsOrChangesSubtitleBytes(t *testing.T) {
	original := []byte{0xff, 0xfe, '1', 0, '\n', 0}
	files, e := Archive(zipBody(t, map[string][]byte{"nested/字幕.srt": original, "Subs\\other.ass": []byte("[Script Info]\n"), "readme.txt": []byte("not a subtitle")}))
	if e != nil || len(files) != 2 {
		t.Fatal(files, e)
	}
	found := false
	for _, file := range files {
		if file.Extension == ".srt" {
			found = bytes.Equal(file.Data, original)
		}
	}
	if !found {
		t.Fatal("subtitle bytes were converted or lost")
	}
	for _, name := range []string{"../escape.srt", "/absolute.srt", "C:/foreign.srt", "Subs/../../escape.ass", "bad\x00.ssa"} {
		if _, e = Archive(zipBody(t, map[string][]byte{name: []byte("1\n00:00:01,000 --> 00:00:02,000\n中文字幕\n")})); e == nil {
			t.Fatal("unsafe archive member accepted", name)
		}
	}
	var payload bytes.Buffer
	z := zip.NewWriter(&payload)
	header := &zip.FileHeader{Name: "link.srt", Method: zip.Store}
	header.SetMode(os.ModeSymlink | 0777)
	w, e := z.CreateHeader(header)
	if e != nil {
		t.Fatal(e)
	}
	w.Write([]byte("/foreign"))
	z.Close()
	if _, e = Archive(payload.Bytes()); e == nil {
		t.Fatal("symlink archive member accepted")
	}
	if _, e = Archive([]byte("<html>captcha</html>")); e == nil {
		t.Fatal("verification page became an archive")
	}
	if SubtitleBytes([]byte("<!DOCTYPE html><title>quota</title>")) {
		t.Fatal("HTML became a raw subtitle")
	}
}
func TestRESTUsesOfficialIdentityQueriesAndNeverForwardsCredentialsToDownloadHost(t *testing.T) {
	ops := []OpenTestStep{}
	calls := 0
	client := &Client{Config: Config{Enabled: true, Provider: REST, APIKey: "private-key", Username: "private-user", Password: "private-password", UserAgent: "fixture-app/1"}, Step: testSteps(t, &ops)}
	client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if len(ops) != calls || ops[calls-1].State != "intent" {
			t.Fatal("network preceded its persistent intent")
		}
		if r.URL.Scheme != "https" || r.Header.Get("User-Agent") != "fixture-app/1" {
			t.Fatal("wrong protocol or application")
		}
		switch r.URL.Path {
		case "/api/v1/login":
			if r.URL.Host != "api.opensubtitles.com" || r.Header.Get("Api-Key") != "private-key" || r.Header.Get("Authorization") != "" {
				t.Fatal("wrong login scope")
			}
			return testResponse(r, 200, []byte(`{"status":200,"base_url":"vip-api.opensubtitles.com","token":"private-token"}`), nil), nil
		case "/api/v1/subtitles":
			if r.URL.Host != "vip-api.opensubtitles.com" || r.Header.Get("Authorization") != "Bearer private-token" || r.URL.Query().Get("parent_imdb_id") != "123" || r.URL.Query().Get("season_number") != "2" || r.URL.Query().Get("episode_number") != "3" || r.URL.Query().Get("imdb_id") != "" || r.URL.Query().Get("languages") != "zh-cn,zh-tw" {
				t.Fatal("wrong TV identity request", r.URL)
			}
			return testResponse(r, 200, []byte(`{"data":[{"attributes":{"language":"zh-cn","release":"original","feature_details":{"feature_type":"Episode","season_number":2,"episode_number":3},"files":[{"file_id":1234,"file_name":"subtitle.ass"}]}}]}`), nil), nil
		case "/api/v1/download":
			var body map[string]int64
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["file_id"] != 1234 {
				t.Fatal("wrong original download grant")
			}
			return testResponse(r, 200, []byte(`{"link":"https://www.opensubtitles.com/download/private-signed/subfile/subtitle.ass","file_name":"subtitle.ass","remaining":4}`), nil), nil
		default:
			if r.URL.Host != "www.opensubtitles.com" || r.Header.Get("Authorization") != "" || r.Header.Get("Api-Key") != "" || r.Header.Get("Cookie") != "" {
				t.Fatal("API credentials forwarded to file host")
			}
			return testResponse(r, 200, []byte("[Script Info]\nTitle: 中文字幕\n"), nil), nil
		}
	})
	rows, e := client.Search(t.Context(), Query{Name: "Show", IMDb: "tt000123", Seasons: []int{2}, Episodes: []int{3}})
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	files, e := client.Download(t.Context(), rows[0])
	if e != nil || len(files) != 1 || files[0].Extension != ".ass" || calls != 4 {
		t.Fatal(files, calls, e)
	}
	for _, op := range ops {
		if op.State != "received" || !Hex(op.Digest, 64) {
			t.Fatal(op)
		}
	}
	raw, _ := json.Marshal(ops)
	for _, secret := range []string{"private-key", "private-password", "private-token", "private-signed"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("secret or signed URL persisted in operation", secret)
		}
	}
}
func TestRESTDoesNotFollowForeignLoginOrDownloadTargetsAndDoesNotRetryQuotaOrLostGrant(t *testing.T) {
	for _, scenario := range []string{"foreign-login", "foreign-download", "denied", "lost-grant", "bad-body"} {
		t.Run(scenario, func(t *testing.T) {
			ops := []OpenTestStep{}
			calls := 0
			client := &Client{Config: Config{Provider: REST, APIKey: "key", Username: "user", Password: "password", UserAgent: "fixture/1"}, Step: testSteps(t, &ops)}
			client.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "api.opensubtitles.com" {
					t.Fatal("unapproved target", r.URL.Host)
				}
				if r.URL.Path == "/api/v1/login" {
					if scenario == "foreign-login" {
						return testResponse(r, 200, []byte(`{"status":200,"base_url":"foreign.test","token":"secret"}`), nil), nil
					}
					return testResponse(r, 200, []byte(`{"status":200,"base_url":"api.opensubtitles.com","token":"secret"}`), nil), nil
				}
				if r.URL.Path != "/api/v1/download" {
					t.Fatal("unexpected operation", r.URL.Path)
				}
				switch scenario {
				case "denied":
					return testResponse(r, 429, []byte("limit"), nil), nil
				case "lost-grant":
					return nil, errors.New("acknowledgement lost")
				case "bad-body":
					return testResponse(r, 200, []byte(`{"link":"bad","link":"https://www.opensubtitles.com/download/a","file_name":"s.srt"}`), nil), nil
				default:
					return testResponse(r, 200, []byte(`{"link":"https://foreign.test/download/key","file_name":"s.srt"}`), nil), nil
				}
			})
			if _, e := client.Login(t.Context()); e != nil {
				if scenario != "foreign-login" {
					t.Fatal(e)
				}
				if calls != 1 {
					t.Fatal(calls)
				}
				return
			}
			if _, e := client.Download(t.Context(), Candidate{ID: "123"}); e == nil {
				t.Fatal("unapproved response accepted")
			}
			if calls != 2 {
				t.Fatal("operation retried or foreign file fetched", calls)
			}
			if scenario == "lost-grant" && ops[1].State != "uncertain" {
				t.Fatal(ops)
			}
			if scenario == "denied" && ops[1].State != "denied" {
				t.Fatal(ops)
			}
		})
	}
}
func TestLegacyHTTPSCookiesAndLostGETResponseUseOneVerifiedConnectionPerOperation(t *testing.T) {
	var requests atomic.Int32
	archive := zipBody(t, map[string][]byte{"movie.srt": []byte("1\n00:00:01,000 --> 00:00:02,000\n中文字幕\n")})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Cookie") == "" || r.Header.Get("Api-Key") != "" || r.Header.Get("Authorization") != "" {
			t.Error("wrong original web cookie scope")
		}
		if strings.Contains(r.URL.Path, "/search/") {
			http.SetCookie(w, &http.Cookie{Name: "page-session", Value: "owned-page", Path: "/", Secure: true})
			io.WriteString(w, `<table id="search_results"><tbody><tr><td><strong><a class="bnone">Movie (2026)</a></strong></td><td></td><td></td><td></td><td><a href="/download/1">download</a></td></tr></tbody></table>`)
			return
		}
		cookie, e := r.Cookie("page-session")
		if e != nil || cookie.Value != "owned-page" {
			t.Error("original page cookie not retained")
		}
		if r.URL.Query().Get("complete") == "1" {
			w.Write(archive)
			return
		}
		connection, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			t.Error(e)
			return
		}
		connection.Close()
	}))
	defer server.Close()
	tr := server.Client().Transport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	tr.TLSClientConfig.ServerName = server.Certificate().DNSNames[0]
	tr.TLSClientConfig.MinVersion = tls.VersionTLS12
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	ops := []OpenTestStep{}
	client := &Client{Config: Config{Provider: Legacy, Cookie: "original-user=fixture", UserAgent: "fixture/1"}, Transport: tr, Step: testSteps(t, &ops)}
	rows, e := client.Search(t.Context(), Query{Movie: true, Name: "Movie", Year: "2026"})
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	if _, e = client.Download(t.Context(), rows[0]); !errors.Is(e, ErrResponse) {
		t.Fatal(e)
	}
	if requests.Load() != 2 || len(ops) != 2 || ops[1].State != "uncertain" {
		t.Fatal("lost GET was repeated", requests.Load(), ops)
	}
}
