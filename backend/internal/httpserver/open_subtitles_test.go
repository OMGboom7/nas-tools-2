package httpserver

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/opensubtitles"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

type openHTTPFixture struct {
	subtitle                 *subtitleHTTPFixture
	calls, logins, downloads atomic.Int32
	provider                 string
	lost, deny               bool
}

func openZIP(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for _, file := range []struct{ name, data string }{{"nested/original.srt", "original Chinese subtitle\n"}, {"nested/second.srt", "second release must not replace first\n"}, {"other.ass", "[Script Info]\nTitle: 原字幕\n"}} {
		w, e := z.Create(file.name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write([]byte(file.data)); e != nil {
			t.Fatal(e)
		}
	}
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return out.Bytes()
}
func openHTTPGrant(t *testing.T, f *subscriptionRunFixture, provider string, remote bool) {
	t.Helper()
	if w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/OpenSubtitles/install", f.token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	detail := subtitleRequestData[pluginConfigDetail](t, nativeJSONRequest(f.handler, "GET", "/api/v1/plugins/OpenSubtitles", f.token, ""))
	values := map[string]any{"enable": true, "provider": provider, "user_agent": "fixture-open/1", "go_enable": true, "go_confirm": true, "go_write": true, "go_old_stopped": true, "go_remote": remote}
	if provider == opensubtitles.REST {
		values["api_key"] = "private-open-key"
		values["username"] = "private-open-user"
		values["password"] = "private-open-password"
	} else {
		values["cookie"] = "fixture-cookie=owned"
	}
	body, _ := json.Marshal(pluginConfigRequest{Fingerprint: detail.Meta.Fingerprint, Values: values, ClearConfig: []string{}})
	if w := nativeJSONRequest(f.handler, "PUT", "/api/v1/plugins/OpenSubtitles", f.token, string(body)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func newOpenHTTPFixture(t *testing.T, provider string, csf bool) *openHTTPFixture {
	t.Helper()
	x := &openHTTPFixture{subtitle: newSubtitleHTTPFixture(t), provider: provider}
	f := x.subtitle.library.f
	x.bind(t, f)
	monitorRebuild(t, f)
	if !csf {
		if w := nativeJSONRequest(f.handler, "DELETE", "/api/v1/plugins/ChineseSubFinder", f.token, ""); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	openHTTPGrant(t, f, provider, false)
	return x
}
func (x *openHTTPFixture) bind(t *testing.T, f *subscriptionRunFixture) {
	t.Helper()
	provider := x.provider
	base := f.transport
	archive := openZIP(t)
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/3/movie/100" {
			return jsonResponse(r, `{"id":100,"title":"Movie","original_title":"Movie","release_date":"2026-01-01","imdb_id":"tt0000100"}`), nil
		}
		if !strings.HasSuffix(r.URL.Hostname(), "opensubtitles.org") && !strings.HasSuffix(r.URL.Hostname(), "opensubtitles.com") {
			return base.RoundTrip(r)
		}
		x.calls.Add(1)
		if r.Header.Get("User-Agent") != "fixture-open/1" {
			t.Error("wrong frozen application identity")
		}
		var intents int
		if e := f.db.QueryRow(`SELECT COUNT(*) FROM GO_OPEN_SUBTITLE_REQUESTS r JOIN json_each(r.OPERATIONS) o WHERE json_extract(o.value,'$.State')='intent'`).Scan(&intents); e != nil || intents != 1 {
			t.Error("network preceded original persistent intent", intents, e)
		}
		response := func(status int, data []byte) *http.Response {
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data)), Request: r}
		}
		if provider == opensubtitles.Legacy {
			if r.URL.Host != "www.opensubtitles.org" || r.Header.Get("Cookie") != "fixture-cookie=owned" || r.Header.Get("Api-Key") != "" {
				t.Error("wrong legacy source or cookie scope")
			}
			if strings.Contains(r.URL.Path, "/search/") {
				return response(200, []byte(`<table id="search_results"><tbody><tr><td><strong><a class="bnone">Movie (2026)</a></strong></td><td></td><td></td><td></td><td><a href="/download/1">download</a></td></tr></tbody></table>`)), nil
			}
			x.downloads.Add(1)
			if x.lost {
				return nil, errors.New("private-open-cookie response lost")
			}
			return response(200, archive), nil
		}
		switch r.URL.Path {
		case "/api/v1/login":
			x.logins.Add(1)
			if x.deny {
				return response(401, []byte(`{"message":"private-open-password denied"}`)), nil
			}
			if r.Header.Get("Api-Key") != "private-open-key" || r.Header.Get("Authorization") != "" {
				t.Error("wrong login credential scope")
			}
			return response(200, []byte(`{"status":200,"base_url":"api.opensubtitles.com","token":"private-open-token"}`)), nil
		case "/api/v1/subtitles":
			if r.Header.Get("Authorization") != "Bearer private-open-token" || r.URL.Query().Get("imdb_id") != "100" || r.URL.Query().Get("languages") != "zh-cn,zh-tw" {
				t.Error("wrong original media search scope", r.URL)
			}
			return response(200, []byte(`{"data":[{"attributes":{"language":"zh-cn","release":"original","feature_details":{"feature_type":"Movie","imdb_id":100,"title":"Movie","year":2026},"files":[{"file_id":123,"file_name":"subtitle.srt"}]}}]}`)), nil
		case "/api/v1/download":
			x.downloads.Add(1)
			if x.lost {
				return nil, errors.New("private-open-key grant acknowledgement lost")
			}
			if r.Header.Get("Authorization") != "Bearer private-open-token" || r.Header.Get("Api-Key") != "private-open-key" {
				t.Error("wrong download grant authority")
			}
			return response(200, []byte(`{"link":"https://www.opensubtitles.com/download/private-link/subfile/subtitle.srt","file_name":"subtitle.srt","remaining":5}`)), nil
		default:
			if r.Header.Get("Authorization") != "" || r.Header.Get("Api-Key") != "" || r.Header.Get("Cookie") != "" {
				t.Error("credentials leaked to file download host")
			}
			return response(200, []byte("1\n00:00:01,000 --> 00:00:02,000\n中文字幕\n")), nil
		}
	})
}
func (x *openHTTPFixture) queue(t *testing.T, relative string) organization.SubtitleRequest {
	t.Helper()
	x.subtitle.video(t, relative)
	p := x.subtitle.review(t, relative)
	in := subtitleQueueInput(p)
	in.ConfirmOpenSubtitles = true
	return subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.subtitle.library.f, "POST", subtitleRequestsBase+"/requests", in))
}
func TestOpenSubtitleManualHTTPBothProtocolsUseOnlyOriginalVideoAndRealOutputReceipts(t *testing.T) {
	for _, provider := range []string{opensubtitles.Legacy, opensubtitles.REST} {
		t.Run(provider, func(t *testing.T) {
			x := newOpenHTTPFixture(t, provider, true)
			f := x.subtitle.library.f
			x.subtitle.video(t, "Films/Movie.2026.mkv")
			existing := filepath.Join(x.subtitle.library.root, "Films/Movie.2026.chi.zh-cn.srt")
			if e := os.WriteFile(existing, []byte("original subtitle must remain\n"), 0640); e != nil {
				t.Fatal(e)
			}
			p := x.subtitle.review(t, "Films/Movie.2026.mkv")
			if p.OpenSubtitles == nil || p.OpenSubtitles.State != "not_queued" || x.calls.Load() != 0 {
				t.Fatal(p, x.calls.Load())
			}
			in := subtitleQueueInput(p)
			denied := subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", in)
			if denied.Code < 400 || x.calls.Load() != 0 {
				t.Fatal("missing separate provider consent sent a request", denied.Code, x.calls.Load())
			}
			in.ConfirmOpenSubtitles = true
			p = subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", in))
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			if e := f.runner.subtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			got := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if got.OpenSubtitles == nil || got.OpenSubtitles.State != "completed" || got.ServiceState != "submitted" || got.ExternalID == "" {
				t.Fatal(got)
			}
			before := x.calls.Load()
			monitorRebuild(t, f)
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil || x.calls.Load() != before {
				t.Fatal("completed request replayed after restart", e, x.calls.Load())
			}
			actual, e := os.ReadFile(existing)
			if e != nil || string(actual) != "original subtitle must remain\n" {
				t.Fatal("existing subtitle replaced", e)
			}
			var count int
			if e = f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); e != nil || count != 0 {
				t.Fatal("manual subtitle fabricated intake", count, e)
			}
			var historyTable bool
			if e = f.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='TRANSFER_HISTORY')`).Scan(&historyTable); e != nil {
				t.Fatal(e)
			}
			if historyTable {
				if e = f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&count); e != nil || count != 0 {
					t.Fatal("manual subtitle fabricated history", count, e)
				}
			}
			response := nativeJSONRequest(f.handler, "GET", "/api/v1/open-subtitles/requests/"+p.OpenSubtitles.ID, f.token, "")
			for _, secret := range []string{"private-open-key", "private-open-password", "private-open-token", "private-link", "fixture-cookie=owned"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatal("secret escaped receipt", secret)
				}
			}
		})
	}
}
func TestOpenSubtitleHTTPUncertainGrantAndRestartNeverCreateAnotherDownloadOrFile(t *testing.T) {
	for _, provider := range []string{opensubtitles.Legacy, opensubtitles.REST} {
		t.Run(provider, func(t *testing.T) {
			x := newOpenHTTPFixture(t, provider, false)
			x.lost = true
			f := x.subtitle.library.f
			p := x.queue(t, "Movie.2026.mkv")
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			got := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if got.ServiceState != "uncertain" || got.OpenSubtitles == nil || got.OpenSubtitles.State != "uncertain" || x.downloads.Load() != 1 {
				t.Fatal(got, x.downloads.Load())
			}
			before := x.calls.Load()
			monitorRebuild(t, f)
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil || x.calls.Load() != before {
				t.Fatal("unknown download repeated", e, x.calls.Load())
			}
			original := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if original.ID != got.ID || original.ServiceUpdated != got.ServiceUpdated || original.OpenSubtitles.ID != got.OpenSubtitles.ID {
				t.Fatal(original, got)
			}
			if _, e := os.Stat(filepath.Join(x.subtitle.library.root, "Movie.2026.chi.zh-cn.srt")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("unknown service response wrote subtitle", e)
			}
			stop := nativeJSONRequest(f.handler, "POST", "/api/v1/open-subtitles/requests/"+p.OpenSubtitles.ID+"/stop", f.token, `{"confirm":true,"reason":"cannot cancel remote operation"}`)
			if stop.Code < 400 {
				t.Fatal("unknown request cancelled as unsent", stop.Code)
			}
		})
	}
}
func TestOpenSubtitleHTTPLoginDenialStopsOtherRequestsUntilANewExplicitGrant(t *testing.T) {
	x := newOpenHTTPFixture(t, opensubtitles.REST, false)
	x.deny = true
	f := x.subtitle.library.f
	first := x.queue(t, "Movie.2026.mkv")
	second := x.queue(t, "Second/Movie.2026.mkv")
	if e := f.runner.openSubtitles.tick(t.Context()); e != nil {
		t.Fatal(e)
	}
	if x.logins.Load() != 1 || x.calls.Load() != 1 {
		t.Fatal("same denied credentials retried", x.logins.Load(), x.calls.Load())
	}
	for _, p := range []organization.SubtitleRequest{first, second} {
		got := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
		if got.OpenSubtitles == nil || got.OpenSubtitles.State != "blocked" {
			t.Fatal(got)
		}
	}
	monitorRebuild(t, f)
	if e := f.runner.openSubtitles.tick(t.Context()); e != nil || x.calls.Load() != 1 {
		t.Fatal("denial retried after restart", e, x.calls.Load())
	}
	status := subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase, nil))
	if status.Ready {
		t.Fatal("denied provider advertised ready")
	}
}
func TestOpenSubtitleHTTPChangesAndNativeLifecycleNeverRetargetOriginalRequests(t *testing.T) {
	for _, change := range []string{"content", "app", "provider", "stop", "uninstall"} {
		t.Run(change, func(t *testing.T) {
			x := newOpenHTTPFixture(t, opensubtitles.Legacy, false)
			f := x.subtitle.library.f
			p := x.queue(t, "Movie.2026.mkv")
			switch change {
			case "content":
				if e := os.WriteFile(filepath.Join(x.subtitle.library.root, "Movie.2026.mkv"), []byte("different video"), 0640); e != nil {
					t.Fatal(e)
				}
			case "app":
				if e := config.NewStore(f.path).Update(map[string]any{"app.web_port": 39999}); e != nil {
					t.Fatal(e)
				}
			case "provider":
				if _, e := f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=REPLACE(VALUE,'"org"','"com"') WHERE KEY='plugin.OpenSubtitles'`); e != nil {
					t.Fatal(e)
				}
			case "stop":
				detail := subtitleRequestData[pluginConfigDetail](t, nativeJSONRequest(f.handler, "GET", "/api/v1/plugins/OpenSubtitles", f.token, ""))
				body, _ := json.Marshal(pluginConfigRequest{Fingerprint: detail.Meta.Fingerprint, Values: map[string]any{"go_enable": false}})
				if w := nativeJSONRequest(f.handler, "PUT", "/api/v1/plugins/OpenSubtitles", f.token, string(body)); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "uninstall":
				if w := nativeJSONRequest(f.handler, "DELETE", "/api/v1/plugins/OpenSubtitles", f.token, ""); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			original := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if x.calls.Load() != 0 || original.OpenSubtitles == nil || original.OpenSubtitles.State != "blocked" {
				t.Fatal(original, x.calls.Load())
			}
		})
	}
}
func TestOpenSubtitleNativeRoutesRejectUnknownActionsAndProtectRawSettings(t *testing.T) {
	x := newOpenHTTPFixture(t, opensubtitles.REST, false)
	f := x.subtitle.library.f
	for _, path := range []string{"/api/v1/plugins/OpenSubtitles", "/api/v1/plugins/OpenSubtitles/page", "/api/v1/open-subtitles", "/api/v1/open-subtitles/requests?page=1"} {
		w := nativeJSONRequest(f.handler, "GET", path, f.token, "")
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		for _, secret := range []string{"private-open-key", "private-open-user", "private-open-password", "private-open-token"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("raw secret exposed", path, secret)
			}
		}
		if w = nativeJSONRequest(f.handler, "GET", path, "", ""); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	for _, path := range []string{"/api/v1/plugins/OpenSubtitles/arbitrary", "/api/v1/plugins/OpenSubtitles/page/records", "/api/v1/open-subtitles/arbitrary"} {
		if w := nativeJSONRequest(f.handler, "DELETE", path, f.token, ""); w.Code != 405 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	for _, endpoint := range []string{"/api/v1/plugin/list", "/api/v1/plugin/status", "/api/v1/plugin/page"} {
		w := performFormRequest(f.handler, endpoint, f.token, url.Values{"id": {"OpenSubtitles"}})
		if w.Code != 200 {
			t.Fatal(endpoint, w.Code, w.Body.String())
		}
		for _, secret := range []string{"private-open-key", "private-open-user", "private-open-password", "private-open-token"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("legacy secret exposure", endpoint, secret)
			}
		}
	}
	w := performFormRequest(f.handler, "/api/v1/plugin/config", f.token, url.Values{"id": {"OpenSubtitles"}, "config": {`{"enable":true,"provider":"com","unknown":9007199254740993}`}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var raw string
	if e := f.db.QueryRow(`SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.OpenSubtitles' ORDER BY ID DESC LIMIT 1`).Scan(&raw); e != nil || !strings.Contains(raw, "9007199254740993") || !strings.Contains(raw, "private-open-key") {
		t.Fatal("legacy merge changed unknown values or blanked credentials", raw, e)
	}
	status := subtitleRequestData[struct{ Ready bool }](t, nativeJSONRequest(f.handler, "GET", "/api/v1/open-subtitles", f.token, ""))
	if status.Ready {
		t.Fatal("legacy save silently retained native grant")
	}
	for _, action := range []string{"uninstall", "install"} {
		w = performFormRequest(f.handler, "/api/v1/plugin/"+action, f.token, url.Values{"id": {"OpenSubtitles"}})
		if w.Code != 200 {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	if w = nativeJSONRequest(f.handler, "GET", "/api/v1/plugins/OpenSubtitles", f.token, ""); w.Code != 200 || strings.Contains(w.Body.String(), "private-open-key") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, e := f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='["OpenSubtitles","UnmigratedPrivatePlugin"]' WHERE KEY='UserInstalledPlugins'`); e != nil {
		t.Fatal(e)
	}
	w = performFormRequest(f.handler, "/api/v1/plugin/list", f.token, url.Values{})
	if w.Code != 501 || strings.Contains(w.Body.String(), "private-open-key") {
		t.Fatal("mixed config exposed a native secret", w.Code, w.Body.String())
	}
}
func TestOpenSubtitleHybridRoutesNeverForwardNativeCredentialsOrMutations(t *testing.T) {
	x := newOpenHTTPFixture(t, opensubtitles.REST, false)
	f := x.subtitle.library.f
	var forwarded atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		http.Error(w, "unexpected old request", 500)
	}))
	defer old.Close()
	h, e := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: old.URL}, f.transport)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/api/v1/open-subtitles", "/api/v1/open-subtitles/requests"} {
		if w := nativeJSONRequest(h, "GET", path, f.token, ""); w.Code != 501 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/plugins/OpenSubtitles", "/api/v1/plugins/OpenSubtitles/install"} {
		method := "PUT"
		if strings.HasSuffix(path, "install") {
			method = "POST"
		}
		if w := nativeJSONRequest(h, method, path, f.token, `{"values":{"go_enable":true}}`); w.Code != 501 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	w := performFormRequest(h, "/api/v1/plugin/config", f.token, url.Values{"id": {"OpenSubtitles"}, "config": {`{"go_enable":true}`}})
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
	if forwarded.Load() != 0 {
		t.Fatal("native OpenSubtitles fell back to Python", forwarded.Load())
	}
}
func TestOpenSubtitleIntakeAllLocalModesAndWholeDiscKeepOriginalPublicationAndHistory(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move", "disc"} {
		t.Run(mode, func(t *testing.T) {
			var f *subscriptionRunFixture
			var source, target string
			var input organizationPlanInput
			if mode == "disc" {
				f, source, target, input = organizationDiscFixture(t, "copy")
			} else {
				f, source, target, input, _ = organizationLinkFixture(t, mode)
			}
			_ = source
			if e := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": false}); e != nil {
				t.Fatal(e)
			}
			x := &openHTTPFixture{provider: opensubtitles.Legacy}
			x.bind(t, f)
			monitorRebuild(t, f)
			openHTTPGrant(t, f, opensubtitles.Legacy, false)
			status, plan, body := organizationPreview(t, f, input)
			if status != 200 {
				t.Fatal(status, body)
			}
			job := organizationCreateJob(t, f, input, plan)
			if rows, e := f.runner.organizer.jobs.OpenSubtitles(t.Context(), false, 1); e != nil || len(rows) != 0 {
				t.Fatal("draft emitted subtitles", rows, e)
			}
			code, done, body := organizationMoveRequest(t, f, job.ID, "execute")
			if code != 200 || done.State != "completed" {
				t.Fatal(code, body)
			}
			before, e := f.runner.organizer.jobs.Get(t.Context(), job.ID)
			if e != nil {
				t.Fatal(e)
			}
			historyBefore := historyList(t, f)
			if e = f.runner.openSubtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			rows, e := f.runner.organizer.jobs.OpenSubtitles(t.Context(), false, 1)
			if e != nil || len(rows) != 1 || rows[0].State != "completed" || len(rows[0].Outputs) != 2 {
				t.Fatal(rows, e)
			}
			for _, out := range rows[0].Outputs {
				if out.State != "created" {
					t.Fatal(out)
				}
				if _, e = os.Stat(filepath.Join(target, out.Target)); e != nil {
					t.Fatal(e)
				}
			}
			after, e := f.runner.organizer.jobs.Get(t.Context(), job.ID)
			if e != nil || organization.Digest(before) != organization.Digest(after) || organization.Digest(historyBefore) != organization.Digest(historyList(t, f)) {
				t.Fatal("subtitle changed original transfer/history", e)
			}
			calls := x.calls.Load()
			monitorRebuild(t, f)
			if e = f.runner.openSubtitles.tick(t.Context()); e != nil || calls != x.calls.Load() {
				t.Fatal("restart replayed original intake", e, x.calls.Load())
			}
		})
	}
}
func TestOpenSubtitleRemoteSingleFileRequiresOnceOnlyWriteConsentAndOriginalObject(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			remote := newRemoteLibraryHTTP(t, driver, false)
			f := remote.base.f
			remote.put(t, "Movie.2026.mkv", []byte("original remote video"))
			remote.put(t, "Movie.2026.chi.zh-cn.srt", []byte("keep original remote subtitle"))
			x := &openHTTPFixture{provider: opensubtitles.Legacy}
			x.bind(t, f)
			remote.restart(t)
			openHTTPGrant(t, f, opensubtitles.Legacy, false)
			status := subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase, nil))
			in := subtitleRequestInput{RootID: remote.base.input.RootID, Driver: driver, Path: "Movie.2026.mkv", Fingerprint: status.Fingerprint, ConfirmRead: true, ConfirmRemoteRead: true}
			p := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/review", in))
			grants := subtitleQueueInput(p)
			grants.ConfirmOpenSubtitles = true
			if w := subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", grants); w.Code == 200 || x.calls.Load() != 0 {
				t.Fatal("remote write borrowed read-only or future-intake permission", w.Code)
			}
			grants.ConfirmRemoteSubtitleWrite = true
			subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", grants))
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			got := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if got.OpenSubtitles == nil || got.OpenSubtitles.State != "completed" || len(got.OpenSubtitles.Outputs) != 2 {
				t.Fatal(got)
			}
			if data, yes := remote.read(t, "Movie.2026.chi.zh-cn.srt"); !yes || string(data) != "keep original remote subtitle" {
				t.Fatal("remote existing subtitle overwritten")
			}
			if data, yes := remote.read(t, "Movie.2026.chi.zh-cn.ass"); !yes || !bytes.Contains(data, []byte("原字幕")) {
				t.Fatal("remote subtitle not published")
			}
			calls := x.calls.Load()
			remote.restart(t)
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil || x.calls.Load() != calls {
				t.Fatal("remote replay", e)
			}
			in.Fingerprint = subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase, nil)).Fingerprint
			p = subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/review", in))
			grants = subtitleQueueInput(p)
			grants.ConfirmOpenSubtitles = true
			grants.ConfirmRemoteSubtitleWrite = true
			subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", grants))
			remote.put(t, "Movie.2026.mkv", []byte("different remote object"))
			if e := f.runner.openSubtitles.tick(t.Context()); e != nil || x.calls.Load() != calls {
				t.Fatal("changed remote source was adopted", e)
			}
			got = subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if got.OpenSubtitles == nil || got.OpenSubtitles.State != "blocked" {
				t.Fatal(got)
			}
			remote.assertNoHistory(t)
		})
	}
}
func TestOpenSubtitleRemoteIntakeWaitsForOriginalNFOAndRequiresItsOwnFutureWriteGrant(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		for _, allow := range []bool{false, true} {
			t.Run(driver+map[bool]string{false: "/no-write", true: "/approved"}[allow], func(t *testing.T) {
				var f *subscriptionRunFixture
				var input organizationPlanInput
				var cfg config.Config
				var read func(string) ([]byte, bool)
				if driver == "minio" {
					fixture, cloud, _, selection, settings := minioManual(t, "minio", false)
					f, input, cfg = fixture, selection, settings
					read = func(name string) ([]byte, bool) {
						return cloud.Get(strings.TrimPrefix(filepath.ToSlash(filepath.Join("/bucket/library", name)), "/"))
					}
				} else {
					fixture, _, target, selection, account := remoteHTTPFixture(t, "rclonecopy", false)
					f, input = fixture, selection
					cfg = config.Config{ApplicationConfigPath: f.path, DisableLegacy: true, RcloneConfigFile: account, RcloneBinary: os.Getenv("NASTOOL_TEST_RCLONE_BINARY")}
					read = func(name string) ([]byte, bool) {
						data, e := os.ReadFile(filepath.Join(target, name))
						return data, e == nil
					}
				}
				if e := config.NewStore(f.path).Update(map[string]any{"media.nfo_poster": allow}); e != nil {
					t.Fatal(e)
				}
				x := &openHTTPFixture{provider: opensubtitles.Legacy}
				x.bind(t, f)
				var e error
				if driver == "rclone" {
					// The existing alias fixture binds the same empty child
					// environment into its value-receiver intake handlers and
					// workers before constructing them.
					f.runner.scraper.transport = f.transport
					f.runner.openSubtitles.transport = f.transport
				} else {
					f.handler, e = buildHandler(cfg, f.transport, &f.runner)
					if e != nil {
						t.Fatal(e)
					}
				}
				openHTTPGrant(t, f, opensubtitles.Legacy, allow)
				if allow {
					if e = f.runner.scraper.system.Set(t.Context(), scraper.ConfigKey, `{"scraper_nfo":{"movie":{"basic":true}}}`); e != nil {
						t.Fatal(e)
					}
				}
				code, plan, body := organizationPreview(t, f, input)
				if code != 200 {
					t.Fatal(code, body)
				}
				job := organizationCreateJob(t, f, input, plan)
				code, done, body := remoteHTTPAction(t, f, job.ID, "execute")
				if code != 200 || done.State != "completed" {
					t.Fatal(code, body)
				}
				history := historyList(t, f)
				if e = f.runner.openSubtitles.tick(t.Context()); e != nil || x.calls.Load() != 0 {
					t.Fatal("remote subtitle preceded original metadata/grant", e, x.calls.Load())
				}
				if allow {
					if e = f.runner.scraper.tick(t.Context()); e != nil {
						t.Fatal(e)
					}
					scrapes, e := f.runner.organizer.jobs.Scrapes(t.Context(), false)
					if e != nil || len(scrapes) != 1 || scrapes[0].State != "completed" {
						t.Fatal(scrapes, e)
					}
					if e = f.runner.openSubtitles.tick(t.Context()); e != nil {
						t.Fatal(e)
					}
				}
				rows, e := f.runner.organizer.jobs.OpenSubtitles(t.Context(), false, 1)
				if e != nil || len(rows) != 1 {
					t.Fatal(rows, e)
				}
				if allow {
					if rows[0].State != "completed" || len(rows[0].Outputs) != 2 {
						t.Fatal(rows)
					}
					for _, output := range rows[0].Outputs {
						if data, yes := read(output.Target); !yes || len(data) == 0 || output.State != "created" {
							t.Fatal("remote original output missing", output)
						}
					}
				} else if rows[0].State != "blocked" || rows[0].Requests != 0 {
					t.Fatal("unapproved remote output began", rows)
				}
				if organization.Digest(history) != organization.Digest(historyList(t, f)) {
					t.Fatal("remote subtitles changed intake history")
				}
				calls := x.calls.Load()
				if e = f.runner.openSubtitles.tick(t.Context()); e != nil || x.calls.Load() != calls {
					t.Fatal("remote enrichment replay", e)
				}
			})
		}
	}
}
