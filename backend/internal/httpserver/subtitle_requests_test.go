package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

type subtitleHTTPFixture struct {
	library *libraryHTTPFixture
	calls   atomic.Int32
	lost    bool
	last    subtitles.Payload
}

func installRequestSubtitle(t *testing.T, f *subscriptionRunFixture, local string) {
	t.Helper()
	if w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/ChineseSubFinder/install", f.token, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	values := map[string]any{"host": "https://subtitle.test/prefix", "api_key": "private-subtitle-key", "local_path": local, "remote_path": "/subtitle"}
	raw, _ := json.Marshal(map[string]any{"values": values})
	if w := nativeJSONRequest(f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", f.token, string(raw)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func newSubtitleHTTPFixture(t *testing.T) *subtitleHTTPFixture {
	t.Helper()
	x := &subtitleHTTPFixture{library: newLibraryHTTPFixture(t, false)}
	f := x.library.f
	base := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "subtitle.test" {
			return base.RoundTrip(r)
		}
		x.calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/prefix/api/v1/add-job" || r.Header.Get("Authorization") != "Bearer private-subtitle-key" || r.URL.RawQuery != "" {
			t.Error("wrong original subtitle target", r.Method, r.URL.Path)
		}
		if e := json.NewDecoder(r.Body).Decode(&x.last); e != nil {
			t.Fatal(e)
		}
		if !strings.HasPrefix(x.last.Path, "/subtitle/") || x.last.Priority != 3 || x.last.Bluray {
			t.Error("wrong original payload", x.last)
		}
		var intents int
		if e := f.db.QueryRow(`SELECT COUNT(*) FROM GO_SUBTITLE_JOBS WHERE STATE='sending'`).Scan(&intents); e != nil || intents != 1 {
			t.Error("missing persisted intent", intents, e)
		}
		if x.lost {
			return nil, errors.New("private-subtitle-key acknowledgement lost")
		}
		return jsonResponse(r, `{"job_id":"original-subtitle-task","message":"accepted"}`), nil
	})
	monitorRebuild(t, f)
	canonical, e := filepath.EvalSymlinks(x.library.root)
	if e != nil {
		t.Fatal(e)
	}
	installRequestSubtitle(t, f, canonical)
	return x
}
func subtitleRequestHTTP(t *testing.T, f *subscriptionRunFixture, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if value != nil {
		data, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		raw = string(data)
	}
	return nativeJSONRequest(f.handler, method, path, f.token, raw)
}
func subtitleRequestData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return recoveryData[T](t, w)
}
func (x *subtitleHTTPFixture) video(t *testing.T, relative string) {
	t.Helper()
	file := filepath.Join(x.library.root, relative)
	if e := os.MkdirAll(filepath.Dir(file), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(file, []byte("agent-owned subtitle request video; no transfer or personal data"), 0640); e != nil {
		t.Fatal(e)
	}
}
func (x *subtitleHTTPFixture) review(t *testing.T, relative string, names ...string) organization.SubtitleRequest {
	t.Helper()
	s := subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase, nil))
	in := subtitleRequestInput{RootID: x.library.input.RootID, Path: relative, Fingerprint: s.Fingerprint, ConfirmRead: true}
	if len(names) == 1 {
		in.Name = names[0]
	}
	return subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/review", in))
}
func subtitleQueueInput(p organization.SubtitleRequest) subtitleRequestInput {
	return subtitleRequestInput{ReviewID: p.ID, Fingerprint: p.Fingerprint, Confirm: true, ConfirmPath: true, ConfirmExternalWrite: true, ConfirmOldExecutorsStopped: true, HTTP: true, ConfirmRemoteRead: true}
}
func TestManualSubtitleHTTPWholeWorkflowUsesExistingFileAndOneOriginalReceipt(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{true: "lost_ack", false: "accepted"}[lost], func(t *testing.T) {
			x := newSubtitleHTTPFixture(t)
			x.lost = lost
			x.video(t, "Movies/Movie.2026.mkv")
			p := x.review(t, "Movies/Movie.2026.mkv")
			if p.Queued || p.ServiceState != "not_queued" || x.calls.Load() != 0 {
				t.Fatal(p, x.calls.Load())
			}
			in := subtitleQueueInput(p)
			p = subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", in))
			if p.State != "queued" || p.ServiceState != "pending" {
				t.Fatal(p)
			}
			if e := x.library.f.runner.subtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			p = subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			state := "submitted"
			if lost {
				state = "uncertain"
			}
			if p.ServiceState != state || p.ServiceUpdated == "" || x.calls.Load() != 1 {
				t.Fatal(p, x.calls.Load())
			}
			if !lost && p.ExternalID != "original-subtitle-task" {
				t.Fatal(p)
			}
			if strings.Contains(p.Reason, "private-subtitle-key") {
				t.Fatal("private error escaped", p)
			}
			if e := os.Remove(filepath.Join(x.library.root, "Movies/Movie.2026.mkv")); e != nil {
				t.Fatal(e)
			}
			monitorRebuild(t, x.library.f)
			original := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", in))
			if original.ID != p.ID || original.ServiceState != state {
				t.Fatal(original)
			}
			if e := x.library.f.runner.subtitles.tick(t.Context()); e != nil || x.calls.Load() != 1 {
				t.Fatal("restart or duplicate request replayed", x.calls.Load(), e)
			}
			var count int
			if e := x.library.f.db.QueryRow(`SELECT COUNT(*) FROM GO_ORGANIZATION_JOBS`).Scan(&count); e != nil || count != 0 {
				t.Fatal("manual request manufactured intake", count, e)
			}
		})
	}
}
func TestManualSubtitleHTTPRequiresIndependentConsentAndKeepsForeignPathsOut(t *testing.T) {
	x := newSubtitleHTTPFixture(t)
	x.video(t, "Movie.2026.mkv")
	p := x.review(t, "Movie.2026.mkv")
	for _, field := range []string{"confirm", "confirmPath", "confirmExternalWrite", "confirmOldExecutorsStopped"} {
		in := subtitleQueueInput(p)
		raw, _ := json.Marshal(in)
		var values map[string]any
		json.Unmarshal(raw, &values)
		values[field] = false
		w := subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", values)
		if w.Code != 400 && w.Code != 409 {
			t.Fatal(field, w.Code, w.Body.String())
		}
	}
	for _, change := range []string{"path", "rootId", "name", "identity"} {
		raw, _ := json.Marshal(subtitleQueueInput(p))
		var in map[string]any
		json.Unmarshal(raw, &in)
		switch change {
		case "path":
			in[change] = "Other.mkv"
		case "rootId":
			in[change] = "foreign-root"
		case "name":
			in[change] = "Another Movie"
		case "identity":
			in[change] = map[string]any{"kind": "movie", "tmdbId": "101"}
		}
		if w := subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", in); w.Code != 409 {
			t.Fatal(change, w.Code, w.Body.String())
		}
	}
	if x.calls.Load() != 0 {
		t.Fatal("unreviewed mutation contacted subtitle service")
	}
	if e := os.Symlink(filepath.Join(t.TempDir(), "foreign.mkv"), filepath.Join(x.library.root, "foreign.mkv")); e != nil {
		t.Fatal(e)
	}
	status := subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase, nil))
	for _, file := range []string{"../Movie.2026.mkv", "/foreign/Movie.2026.mkv", "foreign.mkv", "Movie.2026.mkv/../Movie.2026.mkv", "folder", "Movie.2026.nfo"} {
		w := subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/review", subtitleRequestInput{RootID: x.library.input.RootID, Path: file, Fingerprint: status.Fingerprint, ConfirmRead: true})
		if w.Code == 200 {
			t.Fatal("foreign or non-file scope accepted", file, w.Body.String())
		}
	}
}
func TestManualSubtitleHTTPUsesOriginalConfiguredLinksAndSelectedMediaIdentity(t *testing.T) {
	x := newSubtitleHTTPFixture(t)
	x.video(t, "Movie.2026.mkv")
	if e := os.Symlink("Movie.2026.mkv", filepath.Join(x.library.root, "Alias.2026.mkv")); e != nil {
		t.Fatal(e)
	}
	p := x.review(t, "Alias.2026.mkv", "Movie.2026.mkv")
	if p.Bytes <= 0 || p.Path != "Alias.2026.mkv" {
		t.Fatal(p)
	}
	if e := os.Remove(filepath.Join(x.library.root, "Alias.2026.mkv")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("missing.mkv", filepath.Join(x.library.root, "Alias.2026.mkv")); e != nil {
		t.Fatal(e)
	}
	if w := subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", subtitleQueueInput(p)); w.Code == 200 {
		t.Fatal("replaced link adopted", w.Body.String())
	}
	status := subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase, nil))
	in := subtitleRequestInput{RootID: x.library.input.RootID, Path: "Movie.2026.mkv", Name: "unrecognized original title", Fingerprint: status.Fingerprint, ConfirmRead: true, Identity: &subtitleRequestIdentity{Kind: "movie", TMDBID: "101"}}
	selected := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/review", in))
	if selected.TMDBID != "101" || selected.Title != "Original NFO identity" {
		t.Fatal(selected)
	}
}
func TestManualSubtitleHTTPStopsOnOriginalConfigContentAndUninstallChanges(t *testing.T) {
	for _, change := range []string{"content", "plugin", "app", "uninstall", "open-subtitles"} {
		t.Run(change, func(t *testing.T) {
			x := newSubtitleHTTPFixture(t)
			x.video(t, "Movie.2026.mkv")
			p := x.review(t, "Movie.2026.mkv")
			subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", subtitleQueueInput(p)))
			switch change {
			case "content":
				if e := os.WriteFile(filepath.Join(x.library.root, "Movie.2026.mkv"), []byte("different bytes"), 0640); e != nil {
					t.Fatal(e)
				}
			case "plugin":
				if _, e := x.library.f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=REPLACE(VALUE,'/subtitle','/other') WHERE KEY='plugin.ChineseSubFinder'`); e != nil {
					t.Fatal(e)
				}
			case "app":
				if e := config.NewStore(x.library.f.path).Update(map[string]any{"app.web_port": 39999}); e != nil {
					t.Fatal(e)
				}
			case "uninstall":
				if w := nativeJSONRequest(x.library.f.handler, "DELETE", "/api/v1/plugins/ChineseSubFinder", x.library.f.token, ""); w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "open-subtitles":
				if _, e := x.library.f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='["ChineseSubFinder","OpenSubtitles"]' WHERE KEY='UserInstalledPlugins';INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig','plugin.OpenSubtitles','{"enable":true}')`); e != nil {
					t.Fatal(e)
				}
			}
			if e := x.library.f.runner.subtitles.tick(t.Context()); e != nil {
				t.Fatal(e)
			}
			got := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if x.calls.Load() != 0 || got.ServiceState != "blocked" {
				t.Fatal("changed scope submitted", got, x.calls.Load())
			}
		})
	}
}
func TestManualSubtitleHTTPLegacyFormAndAjaxUseTheSameReviewedOriginal(t *testing.T) {
	for _, ajax := range []bool{false, true} {
		t.Run(strconvBool(ajax), func(t *testing.T) {
			x := newSubtitleHTTPFixture(t)
			x.video(t, "Movie.2026.mkv")
			status := subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase, nil))
			in := subtitleRequestInput{Path: filepath.Join(x.library.root, "Movie.2026.mkv"), Name: "Movie.2026.mkv", Review: true, ConfirmRead: true, Fingerprint: status.Fingerprint}
			send := func(in subtitleRequestInput) *httptest.ResponseRecorder {
				if ajax {
					return subtitleRequestHTTP(t, x.library.f, "POST", "/do", map[string]any{"cmd": "download_subtitle", "data": in})
				}
				form := url.Values{"path": {in.Path}, "name": {in.Name}, "fingerprint": {in.Fingerprint}, "reviewId": {in.ReviewID}, "review": {strconvBool(in.Review)}, "confirmRead": {strconvBool(in.ConfirmRead)}, "confirm": {strconvBool(in.Confirm)}, "confirmPath": {strconvBool(in.ConfirmPath)}, "confirmExternalWrite": {strconvBool(in.ConfirmExternalWrite)}, "confirmOldExecutorsStopped": {strconvBool(in.ConfirmOldExecutorsStopped)}}
				return performFormRequest(x.library.f.handler, "/api/v1/media/subtitle/download", x.library.f.token, form)
			}
			p := subtitleRequestData[organization.SubtitleRequest](t, send(in))
			if x.calls.Load() != 0 {
				t.Fatal("legacy review submitted")
			}
			queued := subtitleRequestData[organization.SubtitleRequest](t, send(subtitleQueueInput(p)))
			if queued.ID != p.ID || !queued.Queued {
				t.Fatal(queued)
			}
			if e := x.library.f.runner.subtitles.tick(t.Context()); e != nil || x.calls.Load() != 1 {
				t.Fatal(x.calls.Load(), e)
			}
		})
	}
}
func strconvBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
func TestManualSubtitleHTTPPlainHTTPNeedsItsOwnLastStepConsent(t *testing.T) {
	x := newSubtitleHTTPFixture(t)
	x.video(t, "Movie.2026.mkv")
	if _, e := x.library.f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=REPLACE(VALUE,'https://subtitle.test','http://subtitle.test') WHERE KEY='plugin.ChineseSubFinder'`); e != nil {
		t.Fatal(e)
	}
	p := x.review(t, "Movie.2026.mkv")
	if !p.HTTP {
		t.Fatal(p)
	}
	in := subtitleQueueInput(p)
	in.HTTP = false
	if w := subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", in); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if x.calls.Load() != 0 {
		t.Fatal("credential transmission preceded explicit HTTP consent")
	}
}
func TestManualSubtitleHTTPDuplicateKeysRoleAndUnknownActionsNeverReachLegacy(t *testing.T) {
	x := newSubtitleHTTPFixture(t)
	for _, body := range []string{`{"confirmRead":true,"confirmRead":false}`, `{"identity":{"tmdbId":"100","tmdbId":"101"}}`, `{"confirmRead":true} {}`, `{"unknown":true}`} {
		if w := nativeJSONRequest(x.library.f.handler, "POST", subtitleRequestsBase+"/review", x.library.f.token, body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, endpoint := range []string{subtitleRequestsBase + "/retry", subtitleRequestsBase + "/requests/abc/execute", "/api/v1/media/subtitle/download/retry"} {
		if w := nativeJSONRequest(x.library.f.handler, "POST", endpoint, x.library.f.token, "{}"); w.Code != 405 {
			t.Fatal(endpoint, w.Code, w.Body.String())
		}
	}
	if w := nativeJSONRequest(x.library.f.handler, "GET", subtitleRequestsBase, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := performFormRequest(x.library.f.handler, "/api/v1/user/manage", x.library.f.token, url.Values{"oper": {"add"}, "name": {"subtitle-viewer"}, "password": {"fixture-password"}, "pris": {""}}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	user := loginForTest(t, x.library.f.handler, "subtitle-viewer", "fixture-password")
	if w := nativeJSONRequest(x.library.f.handler, "GET", subtitleRequestsBase, user, ""); w.Code != 403 {
		t.Fatal("non-admin read", w.Code, w.Body.String())
	}
}
func TestManualSubtitleHybridEntryPointsNeverForwardToPython(t *testing.T) {
	x := newSubtitleHTTPFixture(t)
	var forwarded atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		http.Error(w, "unexpected old request", 500)
	}))
	defer old.Close()
	hybrid, e := newHandler(config.Config{ApplicationConfigPath: x.library.f.path, DisableLegacy: false, LegacyBackendURL: old.URL}, x.library.f.transport)
	if e != nil {
		t.Fatal(e)
	}
	for _, endpoint := range []string{subtitleRequestsBase, subtitleRequestsBase + "/requests", "/api/v1/media/subtitle/download"} {
		if w := nativeJSONRequest(hybrid, "GET", endpoint, x.library.f.token, ""); w.Code != 501 {
			t.Fatal(endpoint, w.Code, w.Body.String())
		}
	}
	if w := nativeJSONRequest(hybrid, "POST", "/do", x.library.f.token, `{"cmd":"download_subtitle","data":{}}`); w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
	if forwarded.Load() != 0 {
		t.Fatal("migrated subtitle entry contacted legacy service", forwarded.Load())
	}
}
func TestManualSubtitleRemoteSourcesRequireSeparateReadConsentAndKeepOriginalBinding(t *testing.T) {
	for _, driver := range []string{"minio", "rclone"} {
		t.Run(driver, func(t *testing.T) {
			remote := newRemoteLibraryHTTP(t, driver, false)
			f := remote.base.f
			remote.put(t, "Movie.2026.mkv", []byte("original remote video"))
			base := f.transport
			var calls atomic.Int32
			f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "subtitle.test" {
					return base.RoundTrip(r)
				}
				calls.Add(1)
				var p subtitles.Payload
				if json.NewDecoder(r.Body).Decode(&p) != nil || p.Path != "/subtitle/Movie.2026.mkv" || p.VideoType != 0 || r.Header.Get("Authorization") != "Bearer private-subtitle-key" {
					t.Error("remote request adopted different mapping", p)
				}
				return jsonResponse(r, `{"job_id":"remote-subtitle-task"}`), nil
			})
			remote.restart(t)
			installRequestSubtitle(t, f, remote.logical)
			status := subtitleRequestData[subtitleRequestStatus](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase, nil))
			in := subtitleRequestInput{RootID: remote.base.input.RootID, Driver: driver, Path: "Movie.2026.mkv", Fingerprint: status.Fingerprint, ConfirmRead: true}
			if w := subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/review", in); w.Code == 200 {
				t.Fatal("remote full read lacked separate consent")
			}
			in.ConfirmRemoteRead = true
			p := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/review", in))
			if p.Driver != driver || p.Bytes != int64(len("original remote video")) {
				t.Fatal(p)
			}
			grants := subtitleQueueInput(p)
			grants.ConfirmRemoteRead = false
			if w := subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", grants); w.Code == 200 {
				t.Fatal("submission reused review-only remote consent")
			}
			grants.ConfirmRemoteRead = true
			subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", grants))
			if e := f.runner.subtitles.tick(t.Context()); e != nil || calls.Load() != 1 {
				t.Fatal("remote existing file not submitted", calls.Load(), e)
			}
			got := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if got.ServiceState != "submitted" {
				t.Fatal(got)
			}
			p = subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/review", in))
			subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "POST", subtitleRequestsBase+"/requests", subtitleQueueInput(p)))
			remote.put(t, "Movie.2026.mkv", []byte("replaced remote video"))
			if e := f.runner.subtitles.tick(t.Context()); e != nil || calls.Load() != 1 {
				t.Fatal("changed remote object was adopted", calls.Load(), e)
			}
			got = subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, f, "GET", subtitleRequestsBase+"/requests/"+p.ID, nil))
			if got.ServiceState != "blocked" {
				t.Fatal(got)
			}
		})
	}
}
func TestManualSubtitleHTTPStablePaginationAndPendingCancellationDoNotTouchVideo(t *testing.T) {
	x := newSubtitleHTTPFixture(t)
	x.video(t, "Movie.2026.mkv")
	p := x.review(t, "Movie.2026.mkv")
	subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", subtitleQueueInput(p)))
	stopped := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests/"+p.ID+"/cancel", map[string]any{"confirm": true, "reason": "stop only this not-sent request"}))
	if stopped.State != "cancelled" || stopped.ServiceState != "blocked" {
		t.Fatal(stopped)
	}
	if e := x.library.f.runner.subtitles.tick(t.Context()); e != nil || x.calls.Load() != 0 {
		t.Fatal(e, x.calls.Load())
	}
	if _, e := os.Stat(filepath.Join(x.library.root, "Movie.2026.mkv")); e != nil {
		t.Fatal(e)
	}
	first := p.ID
	for i := 0; i < 31; i++ {
		x.review(t, "Movie.2026.mkv")
	}
	rows := subtitleRequestData[[]organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase+"/requests?page=2", nil))
	if len(rows) != 2 {
		t.Fatal("lost old reviewed originals", len(rows))
	}
	got := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "GET", subtitleRequestsBase+"/requests/"+first, nil))
	if got.State != "cancelled" {
		t.Fatal(got)
	}
}

func grantSubtitleHTTPWebhook(t *testing.T, x *subtitleHTTPFixture) {
	t.Helper()
	f := x.library.f
	snapshot, e := config.NewStore(f.path).Snapshot()
	if e != nil {
		t.Fatal(e)
	}
	actor := text(objectValue(snapshot["app"])["login_user"])
	authority, e := f.runner.eventWebhook.authority(actor)
	if e != nil {
		t.Fatal(e)
	}
	values := map[string]any{"webhook_url": "https://subtitle-events.test/", "method": "post"}
	control := bus.EventWebhookControl{Epoch: strings.Repeat("a", 32), Actor: actor, Authority: authority, ConfigDigest: bus.Hash(values), Created: time.Now().UTC().Format(time.RFC3339Nano), Authorized: true}
	for key, value := range map[string]any{"UserInstalledPlugins": []string{"ChineseSubFinder", "Webhook"}, bus.EventWebhookKey: values, bus.EventWebhookControlKey: control} {
		raw, _ := json.Marshal(value)
		if _, e = f.db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)`, key, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
}
func TestManualSubtitleHTTPPublishesOnlyItsActualQueuedOriginalEvent(t *testing.T) {
	x := newSubtitleHTTPFixture(t)
	grantSubtitleHTTPWebhook(t, x)
	x.video(t, "Movie.2026.mkv")
	p := x.review(t, "Movie.2026.mkv")
	queued := subtitleRequestData[organization.SubtitleRequest](t, subtitleRequestHTTP(t, x.library.f, "POST", subtitleRequestsBase+"/requests", subtitleQueueInput(p)))
	events, e := x.library.f.runner.eventWebhook.queue.EventWebhooks(t.Context(), 1, false)
	if e != nil || len(events) != 1 || events[0].Reference != queued.ID || events[0].Type != "subtitle.download" || events[0].Definition.Source != nil {
		t.Fatal(events, e)
	}
	var payload map[string]any
	if json.Unmarshal(events[0].Definition.Envelope.Data, &payload) != nil || payload["bluray"] != false || payload["file_ext"] != ".mkv" {
		t.Fatal(payload)
	}
}
