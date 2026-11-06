package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type markFixture struct {
	f                *loginFixture
	api              *torrentMarkAPI
	item             downloaderconfig.Downloader
	mu               sync.Mutex
	labels           []string
	writes, requests int
	lost             bool
	before           func(*http.Request)
}

func newMarkFixture(t *testing.T) *markFixture {
	f := newLoginFixture(t, nil)
	system, err := systemconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { system.Close() })
	ds, err := downloaderconfig.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })
	item, err := ds.Upsert(t.Context(), downloaderconfig.Downloader{Name: "Fixture downloader", Enabled: 1, Type: "qbittorrent", Config: `{"host":"http://qb.fixture.test:8080","username":"fixture","password":"fixture-private-password"}`, DownloadDir: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	m := &markFixture{f: f, api: &torrentMarkAPI{login: f.api, system: system, downloaders: ds}, item: item, labels: []string{"BT", "keep"}}
	if err := system.Set(t.Context(), "UserInstalledPlugins", `["TorrentMark"]`); err != nil {
		t.Fatal(err)
	}
	if err := system.Set(t.Context(), markConfigKey, `{"enable":true,"onlyonce":false,"cron":"","downloaders":["`+strconv.FormatInt(item.ID, 10)+`"],"unknown":9007199254740993}`); err != nil {
		t.Fatal(err)
	}
	f.api.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.requests++
		if m.before != nil {
			m.before(r)
		}
		if r.URL.Host != "qb.fixture.test:8080" {
			t.Fatal("unexpected destination", r.URL)
		}
		switch r.URL.Path {
		case "/api/v2/auth/login":
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("Ok."))}, nil
		case "/api/v2/torrents/info":
			if r.URL.Query().Get("hashes") == "" {
				return jsonResponse(r, `[{"hash":"`+strings.Repeat("a", 40)+`"}]`), nil
			}
			b, _ := json.Marshal([]any{map[string]any{"hash": strings.Repeat("a", 40), "added_on": 1, "state": "uploading", "tags": strings.Join(m.labels, ", "), "progress": 1, "amount_left": 0, "isPrivate": true}})
			return jsonResponse(r, string(b)), nil
		case "/api/v2/torrents/trackers":
			return jsonResponse(r, `[{"url":"https://tracker.fixture.test/announce?passkey=tracker-secret-fixture"}]`), nil
		case "/api/v2/torrents/addTags", "/api/v2/torrents/removeTags":
			_ = r.ParseForm()
			if r.Form.Get("hashes") != strings.Repeat("a", 40) {
				t.Fatal(r.Form)
			}
			m.writes++
			if strings.HasSuffix(r.URL.Path, "addTags") {
				if r.Form.Get("tags") != "PT" {
					t.Fatal(r.Form)
				}
				m.labels = append(m.labels, "PT")
			} else {
				if r.Form.Get("tags") != "BT" {
					t.Fatal(r.Form)
				}
				m.labels = slices.DeleteFunc(m.labels, func(s string) bool { return s == "BT" })
			}
			if m.lost {
				return nil, errors.New("lost response after write")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		t.Fatal(r.URL)
		return nil, errors.New("unexpected fixture request")
	})
	return m
}
func (m *markFixture) scope(t *testing.T) markScope {
	t.Helper()
	s, err := m.api.scope(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (m *markFixture) action(t *testing.T, action string, in markInput) *httptest.ResponseRecorder {
	t.Helper()
	return m.f.request(t, "POST", "/api/v1/torrent-mark/"+action, in)
}
func (m *markFixture) input(t *testing.T) markInput {
	t.Helper()
	st, err := m.api.state(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return markInput{Fingerprint: m.scope(t).fingerprint(), Epoch: st.Control.Epoch, Confirm: true, RequestID: strings.Repeat("a", 64)}
}
func TestTorrentMarkNativeRoutesPreserveConfigAndDedupeAfterReopen(t *testing.T) {
	m := newMarkFixture(t)
	response := m.f.request(t, "GET", "/api/v1/torrent-mark", nil)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	for _, secret := range []string{"fixture-private-password", "tracker-secret-fixture"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("secret leaked")
		}
	}
	response = m.action(t, "run", m.input(t))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"changed":1`) {
		t.Fatal(response.Body.String())
	}
	if m.writes != 2 || !slices.Equal(m.labels, []string{"keep", "PT"}) {
		t.Fatal(m.writes, m.labels)
	}
	st, _ := m.api.state(t.Context())
	if len(st.Pending) != 0 || st.Runs[0].State != "complete" {
		t.Fatal(st)
	}
	// Reopen the same database through a fresh API and evict the UI's recent list.
	if err := m.api.update(t.Context(), func(_ map[string]string, st *markState) error { st.Runs = []markRun{}; return nil }); err != nil {
		t.Fatal(err)
	}
	reopened, err := systemconfig.Open(m.f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	api := *m.api
	api.system = reopened
	req := httptest.NewRequest("POST", "/api/v1/torrent-mark/run", strings.NewReader(`{"requestId":"`+strings.Repeat("a", 64)+`"}`))
	req.SetPathValue("action", "run")
	req.Header.Set("Authorization", m.f.token)
	out := httptest.NewRecorder()
	api.serve(out, req)
	if out.Code != 200 || !strings.Contains(out.Body.String(), "archived") || m.writes != 2 {
		t.Fatal(out.Body.String(), m.writes)
	}
	raw, _ := m.api.system.Get(t.Context(), markConfigKey)
	if !strings.Contains(raw, "9007199254740993") {
		t.Fatal(raw)
	}
}
func TestTorrentMarkLostWriteIsNeverReplayedAndReadOnlyReconciles(t *testing.T) {
	m := newMarkFixture(t)
	m.labels = []string{"keep"}
	m.lost = true
	response := m.action(t, "run", m.input(t))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"unknown":1`) {
		t.Fatal(response.Body.String())
	}
	if m.writes != 1 {
		t.Fatal(m.writes)
	}
	in := m.input(t)
	in.RequestID = strings.Repeat("b", 64)
	response = m.action(t, "run", in)
	if response.Code != 200 || m.writes != 1 {
		t.Fatal(response.Body.String(), m.writes)
	}
	st, _ := m.api.state(t.Context())
	if len(st.Pending) != 1 {
		t.Fatal(st)
	}
	raw, _ := m.api.system.Get(t.Context(), markStateKey)
	if strings.Contains(raw, "tracker-secret") || strings.Contains(raw, "fixture-private-password") {
		t.Fatal("private evidence persisted")
	}
	m.lost = false
	response = m.action(t, "reconcile", m.input(t))
	if response.Code != 200 || m.writes != 1 {
		t.Fatal(response.Body.String(), m.writes)
	}
	st, _ = m.api.state(t.Context())
	if len(st.Pending) != 0 {
		t.Fatal(st)
	}
}
func TestTorrentMarkConsumedOnceSurvivesRestartAndStopRevokesCron(t *testing.T) {
	m := newMarkFixture(t)
	ctx := t.Context()
	raw, _ := m.api.system.Get(ctx, markConfigKey)
	updated, err := mergeMark(raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("true"), "cron": json.RawMessage(`"0 */6 * * *"`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.api.system.Set(ctx, markConfigKey, updated); err != nil {
		t.Fatal(err)
	}
	response := m.action(t, "activate", m.input(t))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	st, _ := m.api.state(ctx)
	once := st.Control.Once
	raw, _ = m.api.system.Get(ctx, markConfigKey)
	if strings.Contains(raw, `"onlyonce":true`) {
		t.Fatal(raw)
	}
	now, _ := time.Parse(time.RFC3339Nano, once)
	m.api.tick(ctx, now.Add(time.Second))
	if m.writes != 2 {
		t.Fatal(m.writes)
	}
	reopened, err := systemconfig.Open(m.f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	other := *m.api
	other.system = reopened
	other.tick(ctx, now.Add(2*time.Second))
	if m.writes != 2 {
		t.Fatal("once replayed")
	}
	st, _ = other.state(ctx)
	if st.Control.Once != "" || st.Control.Next == "" {
		t.Fatal(st)
	}
	response = m.action(t, "stop", m.input(t))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	other.tick(ctx, now.Add(24*time.Hour))
	if m.writes != 2 {
		t.Fatal("stopped schedule wrote")
	}
}
func TestTorrentMarkSaveAndLegacyRoutesStayNative(t *testing.T) {
	m := newMarkFixture(t)
	for _, route := range []string{"/api/v1/plugins/TorrentMark", "/api/v1/plugins/TorrentMark/page", "/api/v1/plugins"} {
		r := m.f.request(t, "GET", route, nil)
		if r.Code != 200 {
			t.Fatal(route, r.Code, r.Body.String())
		}
	}
	d, _, _, _, err := m.api.detail(t.Context(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	r := m.f.request(t, "PUT", "/api/v1/plugins/TorrentMark", map[string]any{"fingerprint": d.Meta.Fingerprint, "values": map[string]any{"cron": "0 6 * * *"}})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	raw, _ := m.api.system.Get(t.Context(), markConfigKey)
	if !strings.Contains(raw, "9007199254740993") {
		t.Fatal(raw)
	}
	in := m.input(t)
	m.action(t, "activate", in)
	r = m.f.request(t, "DELETE", "/api/v1/plugins/TorrentMark", nil)
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	st, _ := m.api.state(t.Context())
	if st.Control.Next != "" {
		t.Fatal(st)
	}
	if m.requests != 0 {
		t.Fatal("configuration contacted downloader", m.requests)
	}
	for _, key := range []string{markConfigKey, markStateKey, markReceiptKey(strings.Repeat("a", 64))} {
		request := httptest.NewRequest("POST", "/api/v1/config/set", strings.NewReader(url.Values{"key": {key}, "value": {"{}"}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Authorization", m.f.token)
		out := httptest.NewRecorder()
		m.f.handler.ServeHTTP(out, request)
		if out.Code != 400 {
			t.Fatal(key, out.Code, out.Body.String())
		}
	}
}
func TestTorrentMarkScopeRevocationStopsNextNetworkRequest(t *testing.T) {
	m := newMarkFixture(t)
	m.before = func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "trackers") {
			m.before = nil
			if err := m.api.update(r.Context(), func(_ map[string]string, st *markState) error { st.Control.Epoch = "revoked"; return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := m.action(t, "run", m.input(t))
	if r.Code != 200 || m.writes != 0 {
		t.Fatal(r.Body.String(), m.writes)
	}
}
func TestTorrentMarkTransmissionRequiresExclusiveAndSharesOrganizationLock(t *testing.T) {
	m := newMarkFixture(t)
	m.item.Type = "transmission"
	if _, err := m.api.downloaders.Upsert(t.Context(), m.item); err != nil {
		t.Fatal(err)
	}
	r := m.action(t, "run", m.input(t))
	if r.Code != 409 || m.requests != 0 {
		t.Fatal(r.Body.String())
	}
	guard, err := m.api.acquire(t.Context(), torrentLabelGuard(m.item.ID, strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if other, err := (*m.f.api.guards).AcquireJob(t.Context(), torrentLabelGuard(m.item.ID, strings.Repeat("a", 40))); err == nil {
		other.Close()
		t.Fatal("label writer guard not shared")
	}
}
func TestTorrentMarkParsesExactIdentifiersAndKeepsUnknownFields(t *testing.T) {
	raw := `{"downloaders":[9007199254740993],"opaque":{"value":9007199254740993}}`
	cfg, err := parseMark(raw)
	if err != nil || cfg.Downloaders[0] != "9007199254740993" {
		t.Fatal(cfg, err)
	}
	updated, err := mergeMark(raw, map[string]json.RawMessage{"enable": json.RawMessage("true")})
	if err != nil || !strings.Contains(updated, `"value":9007199254740993`) {
		t.Fatal(updated, err)
	}
	for _, bad := range []string{`{"downloaders":[1.0]}`, `{"downloaders":["1","1"]}`, `{"downloaders":[null]}`, `{"enable":"true"}`} {
		if _, err := parseMark(bad); err == nil {
			t.Fatal(bad)
		}
	}
}

func TestTorrentMarkLabelLimitNeverCreatesAnUnverifiableWrite(t *testing.T) {
	m := newMarkFixture(t)
	m.labels = []string{}
	for i := 0; i < 64; i++ {
		m.labels = append(m.labels, "custom-"+strconv.Itoa(i))
	}
	r := m.action(t, "run", m.input(t))
	st, err := m.api.state(t.Context())
	if r.Code != 200 || err != nil || m.writes != 0 || len(st.Pending) != 0 || len(st.Runs) != 1 || st.Runs[0].Skipped != 1 {
		t.Fatal(r.Body.String(), err, m.writes, st)
	}
}

func TestTorrentMarkRuntimeAndAuthenticationPreventUnapprovedExecution(t *testing.T) {
	m := newMarkFixture(t)
	for _, path := range []string{"/api/v1/torrent-mark", "/api/v1/torrent-mark/run", "/api/v1/plugins/TorrentMark"} {
		req := httptest.NewRequest("GET", path, nil)
		if strings.HasSuffix(path, "/run") {
			req.Method = "POST"
		}
		out := httptest.NewRecorder()
		m.f.handler.ServeHTTP(out, req)
		if out.Code != 401 {
			t.Fatal(path, out.Code, out.Body.String())
		}
	}
	m.f.api.pureGo = false
	r := m.action(t, "run", m.input(t))
	if r.Code != 501 || m.requests != 0 {
		t.Fatal(r.Code, r.Body.String())
	}
	r = m.f.request(t, "GET", "/api/v1/torrent-mark", nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"goOnly":false`) {
		t.Fatal(r.Body.String())
	}
}

func TestTorrentMarkRecoveryNeedsTheOriginalProcessLockAndNeverReplays(t *testing.T) {
	m := newMarkFixture(t)
	id := strings.Repeat("f", 64)
	if err := m.api.update(t.Context(), func(_ map[string]string, st *markState) error {
		st.Runs = append(st.Runs, markRun{ID: id, Kind: "manual", State: "running"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	guard, err := m.api.acquire(t.Context(), "native-torrent-mark")
	if err != nil {
		t.Fatal(err)
	}
	m.api.tick(t.Context(), time.Now())
	st, _ := m.api.state(t.Context())
	if st.Runs[0].State != "running" {
		t.Fatal(st)
	}
	guard.Close()
	m.api.tick(t.Context(), time.Now())
	st, _ = m.api.state(t.Context())
	if st.Runs[0].State != "unknown" || m.requests != 0 {
		t.Fatal(st, m.requests)
	}
}

func TestTorrentMarkReviewShowsTheConfiguredPortOverride(t *testing.T) {
	m := newMarkFixture(t)
	m.item.Config = `{"host":"http://qb.fixture.test:8080","port":9090}`
	if _, err := m.api.downloaders.Upsert(t.Context(), m.item); err != nil {
		t.Fatal(err)
	}
	r := m.f.request(t, "GET", "/api/v1/torrent-mark", nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"receiver":"http://qb.fixture.test:9090"`) || m.requests != 0 {
		t.Fatal(r.Body.String(), m.requests)
	}
}
