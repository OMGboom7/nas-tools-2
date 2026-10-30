package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type loginFixture struct {
	handler     http.Handler
	api         *siteLoginAPI
	token, path string
	site        siteconfig.Site
	configPath  string
}

func newLoginFixture(t *testing.T, renderer browserruntime.Renderer) *loginFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("app:\n  login_user: admin\n  login_password: password\nsecurity:\n  api_key: native-site-login-fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(dir, "catalog.bin")
	if err := os.WriteFile(catalog, []byte(base64.StdEncoding.EncodeToString([]byte(`{"indexer":[],"conf":{"site.fixture.test":{}}}`))), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := siteconfig.Open(filepath.Join(dir, "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	site, err := store.Upsert(t.Context(), siteconfig.Site{Name: "Fixture site", SignURL: "https://site.fixture.test/", Cookie: "original=fixture-private", Note: `{"unknown":9007199254740993}`})
	if err != nil {
		t.Fatal(err)
	}
	var runner *rssRunAPI
	handler, err := buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: path, SiteCatalogPath: catalog}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("real and Python upstreams prohibited in fixture")
	}), &runner)
	if err != nil {
		t.Fatal(err)
	}
	api := runner.siteLogin
	api.browser = renderer
	ctx, cancel := context.WithCancel(context.Background())
	wait := startSiteLoginWorker(ctx, api)
	t.Cleanup(func() { cancel(); wait(); api.store.Close() })
	return &loginFixture{handler: handler, api: api, token: loginForTest(t, handler, "admin", "password"), path: filepath.Join(dir, "user.db"), configPath: path, site: site}
}
func (f *loginFixture) request(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded := ""
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = string(raw)
	}
	return nativeJSONRequest(f.handler, method, path, f.token, encoded)
}
func (f *loginFixture) review(t *testing.T, options map[string]any) siteLoginReview {
	t.Helper()
	if options == nil {
		options = map[string]any{}
	}
	if _, supplied := options["siteIds"]; !supplied {
		options["siteIds"] = []string{text(f.site.ID)}
	}
	response := f.request(t, "POST", "/api/v1/site-login/review", options)
	var out struct{ Data siteLoginReview }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "fixture-private") {
		t.Fatal("review leaked original cookie")
	}
	return out.Data
}
func (f *loginFixture) start(t *testing.T, review siteLoginReview) *httptest.ResponseRecorder {
	t.Helper()
	return f.request(t, "POST", "/api/v1/site-login/requests", map[string]any{"scopeToken": review.ScopeToken, "username": "fixture-user", "password": "fixture-password", "confirm": true})
}
func (f *loginFixture) batch(t *testing.T, id string, wanted func(siteLoginBatchView) bool) siteLoginBatchView {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		response := f.request(t, "GET", "/api/v1/site-login/batches/"+id, nil)
		var out struct{ Data siteLoginBatchView }
		if response.Code == 200 && json.Unmarshal(response.Body.Bytes(), &out) == nil && wanted(out.Data) {
			return out.Data
		}
		if !time.Now().Before(deadline) {
			t.Fatal("original batch did not reach expected state", response.Code, response.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func loginSuccessRenderer(t *testing.T, calls *atomic.Int32) browserruntime.Renderer {
	t.Helper()
	return browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		calls.Add(1)
		if r.Login == nil || r.Login.Username != "fixture-user" || r.Login.Password != "fixture-password" || r.ReadOnly {
			t.Error("credential workflow missing")
		}
		if err := r.Guard(ctx); err != nil {
			return browserruntime.Result{}, err
		}
		action, err := r.Login.Action(ctx, "credentials")
		if err != nil {
			return browserruntime.Result{}, err
		}
		finish, err := r.Before(ctx, browserruntime.Exchange{Ordinal: 1, URL: r.URL, Method: "POST", Kind: "Document", Body: []byte("fixture-user:fixture-password")})
		if err != nil {
			return browserruntime.Result{}, err
		}
		if err = finish("received", browserruntime.Hash("response")); err != nil {
			return browserruntime.Result{}, err
		}
		if err = action("received", browserruntime.Hash("click")); err != nil {
			return browserruntime.Result{}, err
		}
		return browserruntime.Result{URL: r.URL, HTML: []byte(`<a href="/logout.php">退出</a>`), Cookie: "SID=new-private-cookie", UserAgent: "new-agent", LoginState: "acquired"}, nil
	})
}
func TestSiteLoginNativeConfirmedTargetAndPrivatePublication(t *testing.T) {
	var calls atomic.Int32
	f := newLoginFixture(t, loginSuccessRenderer(t, &calls))
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := `{"username":"old-user","password":"old-password","two_step_code":"keep-old","unknown":9007199254740993}`
	if _, err = db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)", sitelogin.SavedKey, old); err != nil {
		t.Fatal(err)
	}
	review := f.review(t, map[string]any{"remember": true})
	if bytes.Contains(mustDecodeLoginScope(t, review.ScopeToken), []byte("site.fixture.test")) {
		t.Fatal("review token exposed private source document")
	}
	response := f.start(t, review)
	if response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	out := f.batch(t, review.ID, func(b siteLoginBatchView) bool {
		return len(b.Items) == 1 && b.Items[0].State == "completed" && b.RememberState == "saved"
	})
	if calls.Load() != 1 || out.Items[0].Actions != 1 || out.Items[0].Requests != 1 {
		t.Fatal(out, calls.Load())
	}
	for _, secret := range []string{"fixture-password", "original=fixture-private", "SID=new-private-cookie", "old-password"} {
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), secret) {
			t.Fatal("public receipt leaked a credential")
		}
	}
	current, err := f.api.sites.Get(t.Context(), f.site.ID)
	if err != nil || current.Cookie != "SID=new-private-cookie" || !strings.Contains(current.Note, "9007199254740993") {
		t.Fatal("original note or cookie publication failed", err)
	}
	saved, err := f.api.store.Saved(t.Context())
	if err != nil || !strings.Contains(saved, "9007199254740993") || !strings.Contains(saved, "keep-old") || !strings.Contains(saved, "fixture-password") {
		t.Fatal("original saved account not retained correctly", err)
	}
	if response = f.start(t, review); response.Code != 202 || calls.Load() != 1 {
		t.Fatal("confirmed batch replayed", response.Code, calls.Load())
	}
}
func mustDecodeLoginScope(t *testing.T, token string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestSiteLoginChallengeSingleUseAndNoPublicCodeOrPassword(t *testing.T) {
	var renders, actions, inputs atomic.Int32
	renderer := browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		renders.Add(1)
		value, err := r.Login.Interact(ctx, browserruntime.LoginChallenge{Kind: "two_step"})
		if err != nil {
			return browserruntime.Result{}, err
		}
		if value != "654321" {
			t.Error("original input was changed")
		}
		inputs.Add(1)
		receipt, err := r.Login.Action(ctx, "credentials")
		if err != nil {
			return browserruntime.Result{}, err
		}
		actions.Add(1)
		if err = receipt("received", browserruntime.Hash("ack")); err != nil {
			return browserruntime.Result{}, err
		}
		return browserruntime.Result{HTML: []byte(`<a href="/logout.php">退出</a>`), Cookie: "new=private", UserAgent: "agent", LoginState: "acquired"}, nil
	})
	f := newLoginFixture(t, renderer)
	review := f.review(t, nil)
	if response := f.start(t, review); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	waiting := f.batch(t, review.ID, func(b siteLoginBatchView) bool { return len(b.Items) == 1 && b.Items[0].Challenge != nil })
	prompt := waiting.Items[0].Challenge
	id := waiting.Items[0].ID
	if response := f.start(t, review); response.Code != 202 || renders.Load() != 1 {
		t.Fatal("retry of confirmed token re-logged", response.Code)
	}
	response := f.request(t, "POST", "/api/v1/site-login/requests/"+id+"/input", map[string]string{"Nonce": prompt.Nonce, "Value": "654321"})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	completed := f.batch(t, review.ID, func(b siteLoginBatchView) bool { return b.Items[0].State == "completed" })
	if completed.Items[0].Challenge != nil || inputs.Load() != 1 || actions.Load() != 1 {
		t.Fatal(completed, inputs.Load(), actions.Load())
	}
	response = f.request(t, "POST", "/api/v1/site-login/requests/"+id+"/input", map[string]string{"Nonce": prompt.Nonce, "Value": "654321"})
	if response.Code != 409 || inputs.Load() != 1 {
		t.Fatal("one-time input replayed", response.Code, inputs.Load())
	}
	raw, _ := json.Marshal(completed)
	if bytes.Contains(raw, []byte("654321")) || bytes.Contains(raw, []byte("fixture-password")) {
		t.Fatal("transient secret in receipt")
	}
}
func TestSiteLoginFrozenSourceRevocationAndLostProviderResponse(t *testing.T) {
	for _, mode := range []string{"app", "site", "saved", "lost", "cookie-trigger"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			var f *loginFixture
			renderer := browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
				calls.Add(1)
				switch mode {
				case "site":
					site := f.site
					site.Cookie = "changed=operator"
					if _, err := f.api.sites.Upsert(ctx, site); err != nil {
						t.Error(err)
					}
					if err := r.Guard(ctx); err != nil {
						return browserruntime.Result{}, err
					}
				case "saved":
					if _, err := f.api.store.Saved(ctx); err != nil {
						return browserruntime.Result{}, err
					}
					db, _ := sql.Open("sqlite", f.path)
					_, err := db.Exec("UPDATE SYSTEM_DICT SET VALUE=? WHERE KEY=?", `{"username":"changed","password":"changed"}`, sitelogin.SavedKey)
					db.Close()
					if err != nil {
						return browserruntime.Result{}, err
					}
					if err = r.Guard(ctx); err != nil {
						return browserruntime.Result{}, err
					}
				}
				before, err := r.Before(ctx, browserruntime.Exchange{Ordinal: 1, URL: r.URL, Method: "POST", Kind: "Document", Body: []byte("private credentials")})
				if err != nil {
					return browserruntime.Result{}, err
				}
				if mode == "lost" {
					_ = before("uncertain", "")
					return browserruntime.Result{}, browserruntime.ErrResponse
				}
				if err = before("received", browserruntime.Hash("ok")); err != nil {
					return browserruntime.Result{}, err
				}
				return browserruntime.Result{HTML: []byte(`<a href="/logout.php">退出</a>`), Cookie: "new=forbidden", UserAgent: "agent", LoginState: "acquired"}, nil
			})
			f = newLoginFixture(t, renderer)
			options := map[string]any{}
			if mode == "saved" {
				db, _ := sql.Open("sqlite", f.path)
				if _, err := db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)", sitelogin.SavedKey, `{"username":"fixture-user","password":"fixture-password"}`); err != nil {
					t.Fatal(err)
				}
				db.Close()
				options["useSaved"] = true
			}
			review := f.review(t, options)
			if mode == "app" {
				if err := config.NewStore(f.configPath).Update(map[string]any{"app.user_agent": "changed"}); err != nil {
					t.Fatal(err)
				}
				response := f.start(t, review)
				if response.Code != 409 || calls.Load() != 0 {
					t.Fatal("changed application contacted site", response.Code, calls.Load())
				}
				return
			}
			if mode == "cookie-trigger" {
				db, _ := sql.Open("sqlite", f.path)
				if _, err := db.Exec("CREATE TRIGGER ignored_cookie BEFORE UPDATE OF COOKIE ON CONFIG_SITE BEGIN SELECT RAISE(IGNORE);END"); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			if response := f.start(t, review); response.Code != 202 {
				t.Fatal(response.Code, response.Body.String())
			}
			out := f.batch(t, review.ID, func(b siteLoginBatchView) bool { return len(b.Items) == 1 && !sitelogin.Active(b.Items[0].State) })
			if out.Items[0].State == "completed" || calls.Load() != 1 {
				t.Fatal("revoked/unknown publication became success", out, calls.Load())
			}
			current, _ := f.api.sites.Get(t.Context(), f.site.ID)
			if current.Cookie == "new=forbidden" {
				t.Fatal("unconfirmed cookie saved")
			}
			if response := f.start(t, review); response.Code != 202 || calls.Load() != 1 {
				t.Fatal("original unknown login repeated", response.Code, calls.Load())
			}
		})
	}
}
func TestSiteLoginCurrentRootAndStoppedRuntimeNeverFallback(t *testing.T) {
	var calls atomic.Int32
	f := newLoginFixture(t, loginSuccessRenderer(t, &calls))
	response := nativeJSONRequest(f.handler, "GET", "/api/v1/site-login", "", "")
	if response.Code != 401 || calls.Load() != 0 {
		t.Fatal(response.Code, calls.Load())
	}
	review := f.review(t, nil)
	f.api.pureGo = false
	response = f.start(t, review)
	if response.Code != 501 || calls.Load() != 0 {
		t.Fatal("mixed ownership login", response.Code, calls.Load())
	}
}

func peerLoginFixture(t *testing.T, original *loginFixture, renderer browserruntime.Renderer) *loginFixture {
	t.Helper()
	store, err := sitelogin.Open(original.path)
	if err != nil {
		t.Fatal(err)
	}
	guards, err := organization.OpenStore(original.path)
	if err != nil {
		t.Fatal(err)
	}
	api := &siteLoginAPI{store: store, sites: original.api.sites, config: original.api.config, auth: original.api.auth, browser: renderer, transport: original.api.transport, catalogPath: original.api.catalogPath, limits: original.api.limits, guards: &guards, pureGo: true}
	ctx, cancel := context.WithCancel(context.Background())
	wait := startSiteLoginWorker(ctx, api)
	t.Cleanup(func() { cancel(); wait(); store.Close(); guards.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/site-login/review", api.serveReview)
	mux.HandleFunc("POST /api/v1/site-login/requests", api.serveStart)
	mux.HandleFunc("GET /api/v1/site-login/batches/{id}", api.serveBatch)
	mux.HandleFunc("POST /api/v1/site-login/requests/{id}/stop", api.serveStop)
	return &loginFixture{handler: mux, api: api, token: original.token, path: original.path, site: original.site, configPath: original.configPath}
}

func TestSiteLoginBulkKeepsIndependentOutcomesAndDoesNotRememberPartialCredentials(t *testing.T) {
	var calls atomic.Int32
	success := loginSuccessRenderer(t, &calls)
	f := newLoginFixture(t, browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		if strings.Contains(r.URL, "two.fixture.test") {
			calls.Add(1)
			return browserruntime.Result{}, browserruntime.ErrLogin
		}
		return success.Render(ctx, r)
	}))
	second, err := f.api.sites.Upsert(t.Context(), siteconfig.Site{Name: "Second fixture", SignURL: "https://two.fixture.test/", Cookie: "keep=second", Note: `{"unknown":9007199254740993}`})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	old := `{"username":"old","password":"old-private","unknown":9007199254740993}`
	if _, err = db.Exec("INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE)VALUES('SystemConfig',?,?)", sitelogin.SavedKey, old); err != nil {
		t.Fatal(err)
	}
	review := f.review(t, map[string]any{"siteIds": []string{text(second.ID), text(f.site.ID)}, "remember": true})
	if len(review.Targets) != 2 || review.Targets[0].ID != text(f.site.ID) {
		t.Fatal("original targets not ordered or complete", review)
	}
	if response := f.start(t, review); response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	batch := f.batch(t, review.ID, func(b siteLoginBatchView) bool {
		return len(b.Items) == 2 && b.RememberState == "blocked" && !b.Items[0].RuntimeActive
	})
	if calls.Load() != 2 || batch.Items[0].State != "completed" || batch.Items[1].State != "failed" {
		t.Fatal(batch, calls.Load())
	}
	current, err := f.api.sites.Get(t.Context(), second.ID)
	if err != nil || current.Cookie != second.Cookie {
		t.Fatal("failed target changed", err)
	}
	saved, err := f.api.store.Saved(t.Context())
	if err != nil || saved != old {
		t.Fatal("partial login replaced saved credentials", err)
	}
}

func TestSiteLoginSeparateOwnersCannotLoginOrStopLiveOriginalAndReopenOnlyReadsReceipt(t *testing.T) {
	var calls, peerCalls atomic.Int32
	f := newLoginFixture(t, browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		calls.Add(1)
		if _, err := r.Login.Interact(ctx, browserruntime.LoginChallenge{Kind: "two_step"}); err != nil {
			return browserruntime.Result{}, err
		}
		return browserruntime.Result{HTML: []byte(`<a href="/logout.php">退出</a>`), Cookie: "verified=original", UserAgent: "agent", LoginState: "acquired"}, nil
	}))
	review := f.review(t, nil)
	if response := f.start(t, review); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	waiting := f.batch(t, review.ID, func(b siteLoginBatchView) bool { return len(b.Items) == 1 && b.Items[0].Challenge != nil })
	peer := peerLoginFixture(t, f, loginSuccessRenderer(t, &peerCalls))
	other := peer.review(t, nil)
	if response := peer.start(t, other); response.Code != 409 || peerCalls.Load() != 0 {
		t.Fatal("second owner started a duplicate login", response.Code, peerCalls.Load())
	}
	id := waiting.Items[0].ID
	response := peer.request(t, "POST", "/api/v1/site-login/requests/"+id+"/stop", map[string]string{"reason": "Do not stop another active executor"})
	if response.Code != 409 {
		t.Fatal("second owner closed live original receipt", response.Code)
	}
	response = f.request(t, "POST", "/api/v1/site-login/requests/"+id+"/input", map[string]string{"Nonce": waiting.Items[0].Challenge.Nonce, "Value": "654321"})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	done := f.batch(t, review.ID, func(b siteLoginBatchView) bool { return b.Items[0].State == "completed" && !b.Items[0].RuntimeActive })
	if response = peer.start(t, review); response.Code != 202 || peerCalls.Load() != 0 || calls.Load() != 1 {
		t.Fatal("reopened original confirmation re-logged", response.Code, calls.Load(), peerCalls.Load())
	}
	reopened := peer.batch(t, review.ID, func(b siteLoginBatchView) bool { return b.Items[0].State == "completed" })
	if reopened.Items[0].ID != done.Items[0].ID || reopened.Items[0].Updated != done.Items[0].Updated {
		t.Fatal("original receipt replaced")
	}
}

func TestSiteLoginLocalStopClosesOnlyOriginalWaitingInput(t *testing.T) {
	f := newLoginFixture(t, browserStub(func(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
		_, err := r.Login.Interact(ctx, browserruntime.LoginChallenge{Kind: "two_step"})
		return browserruntime.Result{}, err
	}))
	review := f.review(t, nil)
	if response := f.start(t, review); response.Code != 202 {
		t.Fatal(response.Body.String())
	}
	waiting := f.batch(t, review.ID, func(b siteLoginBatchView) bool { return b.Items[0].Challenge != nil })
	item := waiting.Items[0]
	response := f.request(t, "POST", "/api/v1/site-login/requests/"+item.ID+"/stop", map[string]string{"reason": "停止这个隔离测试中的原等待请求"})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	stopped := f.batch(t, review.ID, func(b siteLoginBatchView) bool { return b.Items[0].State == "stopped" && !b.Items[0].RuntimeActive })
	if stopped.Items[0].Challenge != nil {
		t.Fatal("stopped input still exposed")
	}
	response = f.request(t, "POST", "/api/v1/site-login/requests/"+item.ID+"/input", map[string]string{"Nonce": item.Challenge.Nonce, "Value": "654321"})
	if response.Code != 409 {
		t.Fatal("stopped input consumed", response.Code)
	}
	site, err := f.api.sites.Get(t.Context(), f.site.ID)
	if err != nil || site.Cookie != f.site.Cookie {
		t.Fatal("stop published a cookie", err)
	}
}

func TestSiteLoginLegacyRetryAndProgressOnlyReadOriginalAfterCookieChanges(t *testing.T) {
	var calls atomic.Int32
	f := newLoginFixture(t, loginSuccessRenderer(t, &calls))
	originalID := ""
	for i := 0; i < 2; i++ {
		response := f.request(t, "POST", "/do", map[string]any{"cmd": "update_sites_cookie_ua", "data": map[string]any{"siteid": text(f.site.ID), "username": "fixture-user", "password": "fixture-password", "request_id": "one-original-legacy-attempt"}})
		var out struct {
			Code    int
			BatchID string `json:"batch_id"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil || out.Code != 0 || !sitelogin.Hex(out.BatchID, 64) {
			t.Fatal(response.Code, response.Body.String())
		}
		if i == 0 {
			originalID = out.BatchID
		} else if out.BatchID != originalID {
			t.Fatal("cookie publication changed legacy request identity")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("legacy retry re-logged", calls.Load())
	}
	response := f.request(t, "GET", "/stream-progress?type=sitecookie", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), originalID) || calls.Load() != 1 {
		t.Fatal("progress did not read original", response.Code, response.Body.String(), calls.Load())
	}
	manual := f.request(t, "POST", "/do", map[string]any{"cmd": "update_site_cookie_ua", "data": map[string]any{"site_id": text(f.site.ID), "site_cookie": "manual=private", "site_ua": "manual-agent"}})
	if manual.Code != 200 {
		t.Fatal(manual.Code, manual.Body.String())
	}
	site, err := f.api.sites.Get(t.Context(), f.site.ID)
	if err != nil || site.Cookie != "manual=private" || !strings.Contains(site.Note, "9007199254740993") || calls.Load() != 1 {
		t.Fatal("manual native update lost original data", err)
	}
}

func TestSiteLoginRealChromeOriginalCodesAndAtomicCookiePublication(t *testing.T) {
	binary := os.Getenv("NASTOOL_TEST_BROWSER_BINARY")
	if binary == "" {
		t.Skip("explicit test Chrome not configured")
	}
	engine := browserruntime.New(binary)
	if !engine.Available() {
		t.Fatal("configured test Chrome unavailable")
	}
	image, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jHuQAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	var pages, images, posts atomic.Int32
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login.php":
			pages.Add(1)
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<!doctype html><form action="/take-login" method="post"><input name="username"><input name="password" type="password"><input name="two_step_code"><input name="imagestring"><img alt="CAPTCHA" src="/captcha.png"><button type="submit">Login</button></form>`)
		case "/captcha.png":
			images.Add(1)
			w.Header().Set("Content-Type", "image/png")
			http.SetCookie(w, &http.Cookie{Name: "captcha", Value: "original-image", Path: "/", Secure: true, HttpOnly: true})
			w.Write(image)
		case "/take-login":
			posts.Add(1)
			if r.ParseForm() != nil || r.PostForm.Get("username") != "fixture-user" || r.PostForm.Get("password") != "fixture-password" || r.PostForm.Get("two_step_code") != "246810" || r.PostForm.Get("imagestring") != "ABCD" || !strings.Contains(r.Header.Get("Cookie"), "captcha=original-image") {
				t.Error("original fields or captcha session changed")
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "verified-native-fixture", Path: "/", Secure: true, HttpOnly: true})
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<a href="/logout.php">退出</a>`)
		default:
			t.Error("unexpected login destination", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer fixture.Close()
	f := newLoginFixture(t, engine)
	f.api.transport = fixture.Client().Transport
	f.site.SignURL = fixture.URL + "/"
	if f.site, err = f.api.sites.Upsert(t.Context(), f.site); err != nil {
		t.Fatal(err)
	}
	review := f.review(t, nil)
	if response := f.start(t, review); response.Code != 202 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, kind := range []string{"two_step", "captcha"} {
		waiting := f.batch(t, review.ID, func(b siteLoginBatchView) bool {
			return len(b.Items) == 1 && b.Items[0].Challenge != nil && b.Items[0].Challenge.Kind == kind
		})
		item := waiting.Items[0]
		value := "246810"
		if kind == "captcha" {
			value = "ABCD"
			if item.Challenge.Image != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(image) {
				t.Fatal("public challenge image was replaced")
			}
		}
		response := f.request(t, "POST", "/api/v1/site-login/requests/"+item.ID+"/input", map[string]string{"Nonce": item.Challenge.Nonce, "Value": value})
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	done := f.batch(t, review.ID, func(b siteLoginBatchView) bool {
		return len(b.Items) == 1 && b.Items[0].State == "completed" && !b.Items[0].RuntimeActive
	})
	if pages.Load() != 1 || images.Load() != 1 || posts.Load() != 1 || done.Items[0].Actions != 1 || done.Items[0].Requests != 3 || engine.Active() != 0 {
		t.Fatal("real original workflow repeated or lost receipts", pages.Load(), images.Load(), posts.Load(), done)
	}
	current, err := f.api.sites.Get(t.Context(), f.site.ID)
	if err != nil || !strings.Contains(current.Cookie, "SID=verified-native-fixture") || !strings.Contains(current.Note, "9007199254740993") {
		t.Fatal("native session not published with original attributes", err)
	}
}

func TestSiteLoginReviewShowsOriginalHTTPReceiverWithoutSendingImagesOrPrivateQuery(t *testing.T) {
	var calls atomic.Int32
	f := newLoginFixture(t, loginSuccessRenderer(t, &calls))
	if err := config.NewStore(f.configPath).Update(map[string]any{"ocr.custom_ocr_url": "http://ocr-fixture.internal:9898/captcha/base64?fixture_key=private"}); err != nil {
		t.Fatal(err)
	}
	review := f.review(t, map[string]any{"ocr": true})
	if len(review.OCRReceivers) != 1 || review.OCRReceivers[0].Scheme != "http" || review.OCRReceivers[0].Host != "ocr-fixture.internal:9898" || review.OCRReceivers[0].Path != "/captcha/base64" || calls.Load() != 0 {
		t.Fatal("original OCR destination not shown without execution", review.OCRReceivers, calls.Load())
	}
	raw, _ := json.Marshal(review)
	if bytes.Contains(raw, []byte("fixture_key=private")) {
		t.Fatal("private OCR query leaked in public review")
	}
}
