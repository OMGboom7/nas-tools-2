package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const limiterBase = "/api/v1/speed-limiter"

type limiterWireFixture struct {
	*webhookHTTPFixture
	server, identity, sessions string
	upload, download           int64
	writes, reads              int
	lost, mediaFailed, mode    bool
	beforeWrite                func()
	targetID                   string
}

func newLimiterWireFixture(t *testing.T, server string) *limiterWireFixture {
	t.Helper()
	f := &limiterWireFixture{webhookHTTPFixture: newWebhookHTTPFixture(t, server), server: server, identity: "original-limiter-media", sessions: `[]`}
	if server == "plex" {
		f.sessions = `{"MediaContainer":{"size":0,"Metadata":[]}}`
	}
	previous := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "hook-media.test" {
			f.reads++
			if r.Method != "GET" {
				t.Fatal("media mutation by limiter")
			}
			if strings.HasSuffix(r.URL.Path, "/Sessions") || strings.HasSuffix(r.URL.Path, "/status/sessions") {
				if f.mediaFailed {
					return nil, errors.New("complete media unavailable")
				}
				return jsonResponse(r, f.sessions), nil
			}
			if server == "plex" {
				return jsonResponse(r, `{"MediaContainer":{"machineIdentifier":"`+f.identity+`"}}`), nil
			}
			return jsonResponse(r, `{"Id":"`+f.identity+`"}`), nil
		}
		if r.URL.Hostname() != "limiter-q.test" {
			return previous.RoundTrip(r)
		}
		switch r.URL.Path {
		case "/api/v2/auth/login":
			return jsonResponse(r, "Ok."), nil
		case "/api/v2/transfer/speedLimitsMode":
			if f.mode {
				return jsonResponse(r, "1"), nil
			}
			return jsonResponse(r, "0"), nil
		case "/api/v2/transfer/uploadLimit":
			return jsonResponse(r, strconv.FormatInt(f.upload, 10)), nil
		case "/api/v2/transfer/downloadLimit":
			return jsonResponse(r, strconv.FormatInt(f.download, 10)), nil
		case "/api/v2/transfer/setUploadLimit", "/api/v2/transfer/setDownloadLimit":
			if r.Method != "POST" || r.ParseForm() != nil {
				t.Fatal("bad global write")
			}
			var n int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM GO_SPEED_LIMITER_OPERATIONS WHERE STATE='intent' AND TARGET_ID=?`, f.targetID).Scan(&n); err != nil || n != 1 {
				t.Fatal("global setting before durable original intent", n, err)
			}
			f.writes++
			value, err := strconv.ParseInt(r.Form.Get("limit"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasSuffix(r.URL.Path, "setUploadLimit") {
				f.upload = value
			} else {
				f.download = value
			}
			if f.beforeWrite != nil {
				f.beforeWrite()
			}
			if f.lost {
				return nil, errors.New("accepted then acknowledgement lost")
			}
			return jsonResponse(r, ""), nil
		default:
			t.Fatal("unrelated mutation", r.URL)
			return nil, context.Canceled
		}
	})
	d, err := f.runner.download.downloaders.Upsert(t.Context(), downloaderconfig.Downloader{Name: "speed-only qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"https://limiter-q.test","username":"private-user","password":"private-pass"}`})
	if err != nil {
		t.Fatal(err)
	}
	f.targetID = strconv.FormatInt(d.ID, 10)
	monitorRebuild(t, f.subscriptionRunFixture)
	if w := limiterHTTP(t, f, "POST", "/api/v1/plugins/SpeedLimiter/install", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	limiterSave(t, f, map[string]any{"downloaders": []string{f.targetID}, "upload_limit": "64", "download_limit": "128", "upload_unlimit": "0", "download_unlimit": "0", "ipv4": "192.168.0.0/16", "ipv6": "fd00::/8", "interval": "300"})
	return f
}
func limiterHTTP(t *testing.T, f *limiterWireFixture, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return mediaRefreshHTTP(t, f.subscriptionRunFixture, method, path, body)
}
func limiterSave(t *testing.T, f *limiterWireFixture, values map[string]any) {
	t.Helper()
	detail := recoveryData[pluginConfigDetail](t, limiterHTTP(t, f, "GET", "/api/v1/plugins/SpeedLimiter", nil))
	w := limiterHTTP(t, f, "PUT", "/api/v1/plugins/SpeedLimiter", map[string]any{"fingerprint": detail.Meta.Fingerprint, "values": values})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func limiterStatusHTTP(t *testing.T, f *limiterWireFixture) speedLimiterStatus {
	t.Helper()
	return recoveryData[speedLimiterStatus](t, limiterHTTP(t, f, "GET", limiterBase, nil))
}
func limiterReviewHTTP(t *testing.T, f *limiterWireFixture) speedLimiterReview {
	t.Helper()
	s := limiterStatusHTTP(t, f)
	return recoveryData[speedLimiterReview](t, limiterHTTP(t, f, "POST", limiterBase+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirm": true}))
}
func limiterEnable(t *testing.T, f *limiterWireFixture) {
	t.Helper()
	v := limiterReviewHTTP(t, f)
	w := limiterHTTP(t, f, "POST", limiterBase+"/authorize", map[string]any{"id": v.ID, "fingerprint": v.Fingerprint, "confirm": true, "global": true, "unlimited": true, "legacyStopped": true, "notify": true})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func limiterPlaying(f *limiterWireFixture) {
	f.sessions = `[{"NowPlayingItem":{"MediaType":"Video","Bitrate":8000000,"MediaStreams":[{"BitRate":7000000},{"BitRate":1000000}]},"RemoteEndPoint":"8.8.8.8","PlayState":{"IsPaused":false}}]`
	if f.server == "plex" {
		f.sessions = `{"MediaContainer":{"size":1,"Metadata":[{"type":"movie","Player":{"state":"playing","address":"8.8.8.8"},"Media":[{"bitrate":8000}]}]}}`
	}
}
func limiterReceive(t *testing.T, f *limiterWireFixture, key string) {
	t.Helper()
	webhookIDHTTP(t, webhookReceiveHTTP(f.webhookHTTPFixture, f.server, webhookWireBody(f.server), key))
}
func limiterJobs(t *testing.T, f *limiterWireFixture) []speedlimit.Job {
	t.Helper()
	return recoveryData[[]speedlimit.Job](t, limiterHTTP(t, f, "GET", limiterBase+"/jobs", nil))
}
func limiterTick(t *testing.T, f *limiterWireFixture) {
	t.Helper()
	if err := f.runner.speedLimiter.tick(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSpeedLimiterAllProviderCallbacksReadCurrentSessionsThenGlobalRates(t *testing.T) {
	for _, server := range []string{"emby", "jellyfin", "plex"} {
		t.Run(server, func(t *testing.T) {
			f := newLimiterWireFixture(t, server)
			webhookAuthorizeHTTP(t, f.webhookHTTPFixture)
			limiterReceive(t, f, "pre-grant")
			triggers, err := f.runner.speedLimiter.queue.Triggers(t.Context(), false)
			if s := limiterStatusHTTP(t, f); s.Active || err != nil || len(triggers) != 0 || f.writes != 0 {
				t.Fatal("settings or callback auto-enabled", s)
			}
			limiterEnable(t, f)
			limiterPlaying(f)
			limiterReceive(t, f, "future")
			limiterReceive(t, f, "future")
			if f.writes != 0 {
				t.Fatal("receiver made settings")
			}
			if server == "plex" {
				limiterTick(t, f)
				if f.writes != 0 {
					t.Fatal("Plex delay bypassed")
				}
				time.Sleep(3100 * time.Millisecond)
			}
			limiterTick(t, f)
			j := limiterJobs(t, f)
			if f.writes != 2 || f.upload != 65536 || f.download != 131072 || len(j) != 1 || j[0].State != "confirmed" || j[0].Observation.Bitrate != 8000000 {
				t.Fatal(f.writes, f.upload, f.download, j)
			}
			limiterReceive(t, f, "future")
			limiterTick(t, f)
			if f.writes != 2 || len(limiterJobs(t, f)) != 1 {
				t.Fatal("duplicate adopted")
			}
			if r := limiterHTTP(t, f, "GET", limiterBase+"/jobs/"+j[0].ID, nil); r.Code != 200 || strings.Contains(r.Body.String(), "private-") || strings.Contains(r.Body.String(), "limiter-q.test") {
				t.Fatal(r.Code, r.Body.String())
			}
			// Only a fresh future callback sees the current idle state, regardless of
			// the earlier start callback's claimed action.
			f.sessions = "[]"
			if server == "plex" {
				f.sessions = `{"MediaContainer":{"size":0,"Metadata":[]}}`
			}
			limiterReceive(t, f, "idle-now")
			if server == "plex" {
				time.Sleep(3100 * time.Millisecond)
			}
			limiterTick(t, f)
			if f.writes != 4 || f.upload != 0 || f.download != 0 {
				t.Fatal("idle zero did not use global unlimited semantics", f.writes, f.upload, f.download)
			}
		})
	}
}
func TestSpeedLimiterUnknownMutationFindVerifyIgnoreAndRestartNeverResend(t *testing.T) {
	f := newLimiterWireFixture(t, "emby")
	webhookAuthorizeHTTP(t, f.webhookHTTPFixture)
	limiterEnable(t, f)
	limiterPlaying(f)
	f.lost = true
	limiterReceive(t, f, "lost")
	limiterTick(t, f)
	j := limiterJobs(t, f)
	if len(j) != 1 || j[0].State != "partial" || j[0].Operations[0].State != "uncertain" || j[0].Operations[1].State != "blocked" || f.writes != 1 {
		t.Fatal(j, f.writes)
	}
	id := j[0].ID
	monitorRebuild(t, f.subscriptionRunFixture)
	limiterReceive(t, f, "later")
	limiterTick(t, f)
	if f.writes != 1 {
		t.Fatal("restart/new trigger resent unknown")
	}
	if w := limiterHTTP(t, f, "POST", limiterBase+"/jobs/"+id+"/verify", map[string]any{"confirm": true}); w.Code != 400 {
		t.Fatal("omitted ordinal adopted zero", w.Code)
	}
	w := limiterHTTP(t, f, "POST", limiterBase+"/jobs/"+id+"/verify", map[string]any{"confirm": true, "ordinal": 0})
	current := recoveryData[speedlimit.Job](t, w)
	if current.Operations[0].State != "verified_current" || f.writes != 1 {
		t.Fatal(current, f.writes)
	}
	w = limiterHTTP(t, f, "POST", limiterBase+"/jobs/"+id+"/ignore", map[string]any{"confirm": true, "reason": "fake receipt checked"})
	current = recoveryData[speedlimit.Job](t, w)
	if current.State != "ignored" || current.Operations[0].State != "verified_current" || current.Operations[1].State != "ignored" || limiterStatusHTTP(t, f).Active {
		t.Fatal(current)
	}
	monitorRebuild(t, f.subscriptionRunFixture)
	limiterTick(t, f)
	if f.writes != 1 {
		t.Fatal("ignored replayed after restart")
	}
}
func TestSpeedLimiterLiveFailureExternalRateModeOrServerChangeBlocksMutation(t *testing.T) {
	for _, change := range []string{"failed-sessions", "external-speed", "alternate-mode", "server-id", "disabled", "root"} {
		t.Run(change, func(t *testing.T) {
			f := newLimiterWireFixture(t, "emby")
			webhookAuthorizeHTTP(t, f.webhookHTTPFixture)
			limiterEnable(t, f)
			limiterPlaying(f)
			limiterReceive(t, f, "block")
			switch change {
			case "failed-sessions":
				f.mediaFailed = true
			case "external-speed":
				f.upload = 100
			case "alternate-mode":
				f.mode = true
			case "server-id":
				f.identity = "different"
			case "disabled":
				if _, err := f.db.Exec(`UPDATE DOWNLOADER SET ENABLED=0 WHERE ID=?`, f.targetID); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := config.NewStore(f.path).Update(map[string]any{"app.login_password": "new-private"}); err != nil {
					t.Fatal(err)
				}
			}
			_ = f.runner.speedLimiter.tick(t.Context())
			jobs, err := f.runner.speedLimiter.queue.Jobs(t.Context(), 1)
			if err != nil || f.writes != 0 || len(jobs) != 0 {
				t.Fatal("unconfirmed current scope wrote settings", f.writes, err)
			}
		})
	}
}
func TestSpeedLimiterTimerSmartAllocationAndSettingsAlwaysStop(t *testing.T) {
	f := newLimiterWireFixture(t, "jellyfin")
	limiterSave(t, f, map[string]any{"bandwidth": "10", "interval": "1"})
	limiterEnable(t, f)
	limiterPlaying(f)
	// No receiver grant is needed for the independently authorized timer.
	c, spec, on, err := f.runner.speedLimiter.queue.Scope(t.Context())
	if err != nil || !on {
		t.Fatal(on, err)
	}
	if err := f.runner.speedLimiter.queue.Timer(t.Context(), c, spec, time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	// The synthetic future bucket is deliberately not due. Make this original
	// fixture trigger due without fabricating a callback.
	if _, err := f.db.Exec(`UPDATE GO_SPEED_LIMITER_TRIGGERS SET DUE=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	limiterTick(t, f)
	if f.upload != 244*1024 || f.download != 128*1024 || f.writes != 2 {
		t.Fatal("smart remaining bandwidth", f.upload, f.download, f.writes)
	}
	limiterSave(t, f, map[string]any{"upload_limit": "32"})
	if limiterStatusHTTP(t, f).Active || f.upload != 244*1024 {
		t.Fatal("settings saved with active grant or rolled back rates")
	}
	limiterTick(t, f)
	if f.writes != 2 {
		t.Fatal("saved settings wrote without regrant")
	}
}
func TestSpeedLimiterSeparateConsentsLegacyOwnershipAndUnknownFieldPreservation(t *testing.T) {
	f := newLimiterWireFixture(t, "emby")
	v := limiterReviewHTTP(t, f)
	for _, flags := range []map[string]any{{"confirm": true}, {"confirm": true, "global": true}, {"confirm": true, "global": true, "unlimited": true}} {
		flags["id"], flags["fingerprint"] = v.ID, v.Fingerprint
		if w := limiterHTTP(t, f, "POST", limiterBase+"/authorize", flags); w.Code != 400 {
			t.Fatal("grant bypass", w.Code)
		}
	}
	if f.writes != 0 {
		t.Fatal("review wrote")
	}
	limiterEnable(t, f)
	original := fmt.Sprintf(`{"downloaders":[%s],"upload_limit":64,"ipv4":"192.168.0.0/16","ipv6":"fd00::/8","unknown":{"n":9007199254740993}}`, f.targetID)
	w := performFormRequest(f.handler, "/api/v1/plugin/config", f.token, url.Values{"id": {"SpeedLimiter"}, "config": {original}})
	if w.Code != 200 || limiterStatusHTTP(t, f).Active {
		t.Fatal(w.Code, w.Body.String())
	}
	limiterSave(t, f, map[string]any{"download_limit": "32"})
	saved, _ := f.runner.download.system.Get(t.Context(), speedlimit.ConfigKey)
	if !strings.Contains(saved, "9007199254740993") {
		t.Fatal("unknown precision lost", saved)
	}
	for _, path := range []string{"/api/v1/plugin/status", "/api/v1/plugin/page"} {
		w := performFormRequest(f.handler, path, f.token, url.Values{"id": {"SpeedLimiter"}})
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{limiterBase, limiterBase + "/jobs", limiterBase + "/triggers", "/api/v1/plugins/SpeedLimiter"} {
		if w := nativeJSONRequest(f.handler, "GET", path, "", ""); w.Code != 401 {
			t.Fatal("anonymous", path, w.Code)
		}
	}
	if w := limiterHTTP(t, f, "DELETE", "/api/v1/plugins/SpeedLimiter", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if r := limiterStatusHTTP(t, f); r.Installed || r.Active {
		t.Fatal(r)
	}
	if w := limiterHTTP(t, f, "POST", "/api/v1/plugins/SpeedLimiter/install", map[string]any{}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if limiterStatusHTTP(t, f).Active {
		t.Fatal("reinstall enabled old scope")
	}
}
func TestSpeedLimiterInstallationDoesNotBlockNativeSubscriptionSearch(t *testing.T) {
	f := newSubscriptionRunFixture(t)
	if w := nativeJSONRequest(f.handler, "POST", "/api/v1/plugins/SpeedLimiter/install", f.token, "{}"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if code, body := f.run("MOV", "1"); code != 200 || f.adds != 1 {
		t.Fatal("new native plugin blocked unrelated subscription search", code, body)
	}
}
func TestSpeedLimiterHybridAndUnsupportedRoutesNeverFallThrough(t *testing.T) {
	f := newLimiterWireFixture(t, "emby")
	handler, err := newHandler(config.Config{ApplicationConfigPath: f.path, LegacyBackendURL: "http://legacy:3000"}, f.transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{limiterBase, "/api/v1/plugins/SpeedLimiter", "/api/v1/plugins/SpeedLimiter/page", "/api/v1/plugins/SpeedLimiter/unsupported"} {
		if w := nativeJSONRequest(handler, "GET", path, f.token, ""); w.Code != 501 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	if w := limiterHTTP(t, f, "POST", "/api/v1/plugins/SpeedLimiter/unsupported", map[string]any{}); w.Code != 405 {
		t.Fatal(w.Code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	wait := startSpeedLimiterWorker(ctx, f.runner.speedLimiter)
	cancel()
	wait()
	if f.runner.speedLimiter.workers.Load() != 0 || f.writes != 0 {
		t.Fatal("worker shutdown")
	}
}
func TestSpeedLimiterOtherClientCanFinishAfterOriginalQBResponseLost(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(strconv.FormatBool(modern), func(t *testing.T) {
			f := newLimiterWireFixture(t, "emby")
			previous := f.transport
			sets, handshakes := 0, 0
			rates := map[string]any{"speed-limit-up": 0, "speed-limit-down": 0, "speed-limit-up-enabled": false, "speed-limit-down-enabled": false, "alt-speed-up": 50, "alt-speed-down": 100, "alt-speed-enabled": false, "alt-speed-time-enabled": false}
			f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() != "limiter-t.test" {
					return previous.RoundTrip(r)
				}
				if r.Header.Get("X-Transmission-Session-Id") == "" {
					handshakes++
					v := jsonResponse(r, "")
					v.StatusCode = 409
					v.Header.Set("X-Transmission-Session-Id", "csrf")
					if modern {
						v.Header.Set("X-Transmission-Rpc-Version", "6.0.0")
					}
					return v, nil
				}
				var in struct {
					Method    string
					Arguments map[string]any
					Params    map[string]any
				}
				if json.NewDecoder(r.Body).Decode(&in) != nil {
					t.Fatal("bad RPC")
				}
				if strings.Contains(in.Method, "set") {
					sets++
					args := in.Arguments
					if modern {
						args = in.Params
					}
					if len(args) != 4 {
						t.Fatal("changed non-normal fields", args)
					}
					for k, v := range args {
						rates[strings.ReplaceAll(k, "_", "-")] = v
					}
				} else if in.Method != "session-get" && in.Method != "session_get" {
					t.Fatal("unrelated Transmission operation", in.Method)
				}
				out := map[string]any{"result": "success", "arguments": rates}
				if modern {
					values := map[string]any{}
					for k, v := range rates {
						values[strings.ReplaceAll(k, "-", "_")] = v
					}
					out = map[string]any{"jsonrpc": "2.0", "id": 1, "result": values}
				}
				raw, _ := json.Marshal(out)
				return jsonResponse(r, string(raw)), nil
			})
			d, err := f.runner.download.downloaders.Upsert(t.Context(), downloaderconfig.Downloader{Name: "speed-only transmission", Type: "transmission", Enabled: 1, Config: `{"host":"https://limiter-t.test","username":"private-tr","password":"private-tr-pass"}`})
			if err != nil {
				t.Fatal(err)
			}
			monitorRebuild(t, f.subscriptionRunFixture)
			limiterSave(t, f, map[string]any{"downloaders": []string{f.targetID, strconv.FormatInt(d.ID, 10)}})
			webhookAuthorizeHTTP(t, f.webhookHTTPFixture)
			limiterEnable(t, f)
			limiterPlaying(f)
			f.lost = true
			limiterReceive(t, f, "two-clients")
			limiterTick(t, f)
			jobs := limiterJobs(t, f)
			if len(jobs) != 1 || len(jobs[0].Operations) != 3 || jobs[0].Operations[0].State != "uncertain" || jobs[0].Operations[1].State != "blocked" || jobs[0].Operations[2].State != "confirmed" || sets != 1 || f.writes != 1 || rates["speed-limit-up"] != float64(64) || rates["speed-limit-down"] != float64(128) {
				t.Fatal(jobs, sets, f.writes, rates)
			}
			priorHandshake := handshakes
			monitorRebuild(t, f.subscriptionRunFixture)
			limiterTick(t, f)
			if sets != 1 || handshakes != priorHandshake {
				t.Fatal("restart replayed or reopened unfinished plan", sets, handshakes)
			}
		})
	}
}
func TestSpeedLimiterPlainHTTPAndPlexAccountNeedConsentBeforeRead(t *testing.T) {
	f := newLimiterWireFixture(t, "emby")
	if err := config.NewStore(f.path).Update(map[string]any{"emby.host": "http://hook-media.test/prefix"}); err != nil {
		t.Fatal(err)
	}
	monitorRebuild(t, f.subscriptionRunFixture)
	s := limiterStatusHTTP(t, f)
	w := limiterHTTP(t, f, "POST", limiterBase+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirm": true})
	if w.Code != 400 || f.reads != 0 {
		t.Fatal("plaintext credentials sent before consent", w.Code, f.reads)
	}
	w = limiterHTTP(t, f, "POST", limiterBase+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirm": true, "http": true})
	if w.Code != 200 || f.reads != 2 {
		t.Fatal(w.Code, w.Body.String(), f.reads)
	}
	if err := config.NewStore(f.path).Update(map[string]any{"media.media_server": "plex", "plex.host": "", "plex.token": "", "plex.username": "private-plex-user", "plex.password": "private-plex-pass", "plex.servername": "Selected"}); err != nil {
		t.Fatal(err)
	}
	signins := 0
	previous := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "plex.tv" {
			return previous.RoundTrip(r)
		}
		if strings.Contains(r.URL.Path, "sign_in") {
			signins++
			return jsonResponse(r, `{"user":{"authToken":"private-cloud-token"}}`), nil
		}
		return jsonResponse(r, `<MediaContainer size="1"><Device name="Selected" provides="server" clientIdentifier="cloud-machine" accessToken="private-server-token"><Connection protocol="https" uri="https://plex-limiter.test"/></Device></MediaContainer>`), nil
	})
	previousCloud := f.transport
	f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "plex-limiter.test" {
			return previousCloud.RoundTrip(r)
		}
		if r.Header.Get("X-Plex-Token") != "private-server-token" {
			t.Fatal("wrong scoped account token")
		}
		if r.URL.Path == "/status/sessions" {
			return jsonResponse(r, `{"MediaContainer":{"size":0,"Metadata":[]}}`), nil
		}
		return jsonResponse(r, `{"MediaContainer":{"machineIdentifier":"cloud-machine"}}`), nil
	})
	monitorRebuild(t, f.subscriptionRunFixture)
	s = limiterStatusHTTP(t, f)
	if !s.Cloud {
		t.Fatal("account fallback missing", s)
	}
	w = limiterHTTP(t, f, "POST", limiterBase+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirm": true})
	if w.Code != 400 || signins != 0 {
		t.Fatal(w.Code, signins)
	}
	v := recoveryData[speedLimiterReview](t, limiterHTTP(t, f, "POST", limiterBase+"/review", map[string]any{"fingerprint": s.Fingerprint, "confirm": true, "plexAccount": true}))
	w = limiterHTTP(t, f, "POST", limiterBase+"/authorize", map[string]any{"id": v.ID, "fingerprint": v.Fingerprint, "confirm": true, "global": true, "unlimited": true, "legacyStopped": true})
	if w.Code != 200 || signins != 1 || f.writes != 0 {
		t.Fatal(w.Code, w.Body.String(), signins, f.writes)
	}
	for _, private := range []string{"private-cloud-token", "private-plex-pass", "private-server-token"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("account secret escaped")
		}
	}
}
func TestSpeedLimiterStoppingDuringFirstWritePreservesResultAndBlocksSecondDimension(t *testing.T) {
	f := newLimiterWireFixture(t, "emby")
	webhookAuthorizeHTTP(t, f.webhookHTTPFixture)
	limiterEnable(t, f)
	limiterPlaying(f)
	f.beforeWrite = func() {
		status := limiterStatusHTTP(t, f)
		w := limiterHTTP(t, f, "POST", limiterBase+"/stop", map[string]any{"fingerprint": status.Fingerprint, "confirm": true})
		if w.Code != 200 {
			t.Fatal("stop while original request in flight", w.Code, w.Body.String())
		}
	}
	limiterReceive(t, f, "stop-during-first")
	limiterTick(t, f)
	jobs := limiterJobs(t, f)
	if len(jobs) != 1 || jobs[0].State != "stopped" || jobs[0].Operations[0].State != "confirmed" || jobs[0].Operations[1].State != "blocked" || f.writes != 1 || f.upload != 65536 || f.download != 0 || limiterStatusHTTP(t, f).Active {
		t.Fatal(jobs, f.writes, f.upload, f.download)
	}
	monitorRebuild(t, f.subscriptionRunFixture)
	limiterTick(t, f)
	if f.writes != 1 {
		t.Fatal("stopped run resumed")
	}
}
func TestSpeedLimiterNonRootCannotReadOrChangeLimiterAuthority(t *testing.T) {
	f := newLimiterWireFixture(t, "emby")
	w := performFormRequest(f.handler, "/api/v1/user/manage", f.token, url.Values{"oper": {"add"}, "name": {"speed-viewer"}, "password": {"fixture-only-password"}, "pris": {""}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	viewer := loginForTest(t, f.handler, "speed-viewer", "fixture-only-password")
	for _, tc := range []struct{ method, path string }{{"GET", limiterBase}, {"GET", limiterBase + "/jobs"}, {"POST", limiterBase + "/review"}, {"POST", limiterBase + "/authorize"}, {"POST", limiterBase + "/stop"}, {"GET", "/api/v1/plugins/SpeedLimiter"}, {"PUT", "/api/v1/plugins/SpeedLimiter"}} {
		w := nativeJSONRequest(f.handler, tc.method, tc.path, viewer, "{}")
		if w.Code != 403 {
			t.Fatal(tc.path, w.Code, w.Body.String())
		}
	}
	if f.writes != 0 || f.reads != 0 {
		t.Fatal("non-root reached original connections", f.writes, f.reads)
	}
}
