package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

type csfFixture struct {
	f                    *subscriptionRunFixture
	job                  organization.Job
	source, target       string
	input                organizationPlanInput
	plan                 organizationPlan
	calls                int
	lost, unacknowledged bool
}

func subtitleFixture(t *testing.T, mode string) *csfFixture {
	t.Helper()
	c := &csfFixture{}
	c.f, c.source, c.target, c.input, c.plan = organizationLinkFixture(t, mode)
	base := c.f.transport
	c.f.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() != "csf.test" {
			return base.RoundTrip(r)
		}
		c.calls++
		if r.Method != "POST" || r.URL.Path != "/api/v1/add-job" || r.Header.Get("Authorization") != "Bearer private-csf-key" {
			t.Fatal("wrong subtitle scope", r.URL)
		}
		var payload subtitles.Payload
		if json.NewDecoder(r.Body).Decode(&payload) != nil || !strings.HasPrefix(payload.Path, "/csf/") || payload.VideoType != 0 || payload.Priority != 3 || payload.Bluray {
			t.Fatal(payload)
		}
		var sending int
		if err := c.f.db.QueryRow(`SELECT COUNT(*) FROM GO_SUBTITLE_JOBS WHERE STATE='sending'`).Scan(&sending); err != nil || sending != 1 {
			t.Fatal("missing durable sending intent", sending, err)
		}
		if c.lost {
			return nil, errors.New("private-csf-key response lost")
		}
		if c.unacknowledged {
			return jsonResponse(r, `{"message":"private physical path not found"}`), nil
		}
		return jsonResponse(r, `{"job_id":"CSF_Job-1","message":"ok"}`), nil
	})
	monitorRebuild(t, c.f)
	w := nativeJSONRequest(c.f.handler, "POST", "/api/v1/plugins/ChineseSubFinder/install", c.f.token, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	mappingRoot, err := filepath.EvalSymlinks(c.target)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(pluginConfigRequest{Values: map[string]any{"host": "http://csf.test", "api_key": "private-csf-key", "local_path": mappingRoot, "remote_path": "/csf"}})
	w = nativeJSONRequest(c.f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", c.f.token, string(body))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	c.job = organizationCreateJob(t, c.f, c.input, c.plan)
	if c.f.runner.subtitles == nil {
		t.Fatal("production runner lacks subtitle worker")
	}
	return c
}

func (c *csfFixture) execute(t *testing.T) {
	t.Helper()
	status, job, body := organizationMoveRequest(t, c.f, c.job.ID, "execute")
	if status != 200 || job.State != "completed" {
		t.Fatal(status, body)
	}
	c.job = job
}
func (c *csfFixture) receipts(t *testing.T) []subtitles.Receipt {
	t.Helper()
	rows, err := c.f.runner.subtitles.queue.List(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestChineseSubFinderIntakeAllModesProducesOneDurableTask(t *testing.T) {
	for _, mode := range []string{"copy", "link", "softlink", "move"} {
		t.Run(mode, func(t *testing.T) {
			c := subtitleFixture(t, mode)
			if rows := c.receipts(t); len(rows) != 0 {
				t.Fatal("draft enqueued subtitles", rows)
			}
			c.execute(t)
			rows := c.receipts(t)
			if len(rows) != 1 || rows[0].State != "pending" || c.calls != 0 {
				t.Fatal(rows, c.calls)
			}
			// No-op configuration save must not invalidate a pending event.
			w := nativeJSONRequest(c.f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", c.f.token, `{"values":{"host":"","api_key":""}}`)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if err := c.f.runner.subtitles.tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			rows = c.receipts(t)
			if len(rows) != 1 || rows[0].State != "submitted" || rows[0].ExternalID != "CSF_Job-1" || c.calls != 1 {
				t.Fatal(rows, c.calls)
			}
			monitorRebuild(t, c.f)
			if err := c.f.runner.subtitles.tick(t.Context()); err != nil || c.calls != 1 {
				t.Fatal("restart replay", err, c.calls)
			}
			c.execute(t)
			if err := c.f.runner.subtitles.tick(t.Context()); err != nil || c.calls != 1 {
				t.Fatal("re-execute replay", err, c.calls)
			}
			page := nativeJSONRequest(c.f.handler, "GET", "/api/v1/plugins/ChineseSubFinder/page", c.f.token, "")
			if page.Code != 200 || !strings.Contains(page.Body.String(), "CSF_Job-1") {
				t.Fatal(page.Code, page.Body.String())
			}
			for _, secret := range []string{"private-csf-key", c.source, c.target, "http://csf.test"} {
				if strings.Contains(page.Body.String(), secret) {
					t.Fatal("page leaked scope")
				}
			}
		})
	}
}

func TestChineseSubFinderLostResponseOrMissingJobIDNeverRepeatsIntakeOrSubmission(t *testing.T) {
	for _, lost := range []bool{false, true} {
		c := subtitleFixture(t, "copy")
		c.lost = lost
		c.unacknowledged = !lost
		c.execute(t)
		if err := c.f.runner.subtitles.tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if rows := c.receipts(t); len(rows) != 1 || rows[0].State != "uncertain" {
			t.Fatal(rows)
		}
		monitorRebuild(t, c.f)
		c.execute(t)
		if err := c.f.runner.subtitles.tick(t.Context()); err != nil || c.calls != 1 {
			t.Fatal(err, c.calls)
		}
		var histories int
		if err := c.f.db.QueryRow(`SELECT COUNT(*) FROM TRANSFER_HISTORY`).Scan(&histories); err != nil || histories != 1 {
			t.Fatal("intake replayed", histories, err)
		}
	}
}

func TestChineseSubFinderConfigEpochAndTargetReplacementBlockOriginalTask(t *testing.T) {
	for _, mode := range []string{"config", "reinstall", "target", "proof"} {
		t.Run(mode, func(t *testing.T) {
			c := subtitleFixture(t, "copy")
			c.execute(t)
			switch mode {
			case "config":
				w := nativeJSONRequest(c.f.handler, "PUT", "/api/v1/plugins/ChineseSubFinder", c.f.token, `{"values":{"api_key":"replacement-key"}}`)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "reinstall":
				w := nativeJSONRequest(c.f.handler, "DELETE", "/api/v1/plugins/ChineseSubFinder", c.f.token, "")
				if w.Code != 200 {
					t.Fatal(w.Code)
				}
				w = nativeJSONRequest(c.f.handler, "POST", "/api/v1/plugins/ChineseSubFinder/install", c.f.token, "")
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			case "target":
				for _, item := range c.job.Items {
					if item.Kind == "media" {
						if err := os.WriteFile(filepath.Join(c.target, item.Target), []byte("changed media"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "proof":
				if _, err := c.f.db.Exec(`UPDATE GO_ORGANIZATION_ITEMS SET PROOF=json_set(PROOF,'$.digest',?) WHERE JOB_ID=? AND ORDINAL=0`, strings.Repeat("a", 64), c.job.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.f.runner.subtitles.tick(t.Context()); err != nil || c.calls != 0 {
				t.Fatal("changed scope submitted", err, c.calls)
			}
			if rows := c.receipts(t); len(rows) != 1 || rows[0].State != "blocked" {
				t.Fatal(rows)
			}
		})
	}
}

func TestChineseSubFinderInvalidConfigDoesNotUndoIntakeAndLegacyReadsHideSecrets(t *testing.T) {
	c := subtitleFixture(t, "copy")
	if _, err := c.f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE='invalid-private-csf-config' WHERE KEY='plugin.ChineseSubFinder'`); err != nil {
		t.Fatal(err)
	}
	c.execute(t)
	if rows := c.receipts(t); len(rows) != 1 || rows[0].State != "blocked" {
		t.Fatal(rows)
	}
	w := performFormRequest(c.f.handler, "/api/v1/plugin/config", c.f.token, url.Values{"id": {"ChineseSubFinder"}, "config": {`{"host":"http://csf.test","api_key":"private-csf-key","unknown_cache":"private-cache"}`}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, endpoint := range []string{"/api/v1/plugin/list", "/api/v1/plugin/status", "/api/v1/plugin/page"} {
		w = performFormRequest(c.f.handler, endpoint, c.f.token, url.Values{"id": {"ChineseSubFinder"}})
		if w.Code != 200 {
			t.Fatal(endpoint, w.Code, w.Body.String())
		}
		for _, secret := range []string{"private-csf-key", "private-cache", c.target} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("legacy read leaked", secret)
			}
		}
	}
	if err := c.f.runner.subtitles.tick(t.Context()); err != nil || c.calls != 0 {
		t.Fatal("repaired config replayed blocked event", err, c.calls)
	}
	h, err := newHandler(config.Config{ApplicationConfigPath: c.f.path, LegacyBackendURL: "http://legacy:3000"}, c.f.transport)
	if err != nil {
		t.Fatal(err)
	}
	w = nativeJSONRequest(h, "PUT", "/api/v1/plugins/ChineseSubFinder", c.f.token, `{"values":{"api_key":"new-key"}}`)
	if w.Code != 501 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestChineseSubFinderPartialGroupAndViewerCannotDispatch(t *testing.T) {
	c := subtitleFixture(t, "copy")
	conflict := ""
	for _, item := range c.job.Items {
		if item.Kind == "subtitle" {
			conflict = filepath.Join(c.target, item.Target)
			break
		}
	}
	if conflict == "" {
		t.Fatal("fixture lacks companion")
	}
	if err := os.MkdirAll(filepath.Dir(conflict), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conflict, []byte("unrelated-existing-subtitle"), 0600); err != nil {
		t.Fatal(err)
	}
	status, job, body := organizationMoveRequest(t, c.f, c.job.ID, "execute")
	if status == 200 && job.State == "completed" {
		t.Fatal("conflict silently completed", body)
	}
	if rows := c.receipts(t); len(rows) != 0 {
		t.Fatal("partial group enqueued", rows)
	}
	if err := c.f.runner.subtitles.tick(t.Context()); err != nil || c.calls != 0 {
		t.Fatal(err, c.calls)
	}
	w := performFormRequest(c.f.handler, "/api/v1/user/manage", c.f.token, url.Values{"oper": {"add"}, "name": {"viewer"}, "password": {"strong-password"}, "pris": {""}})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	viewer := loginForTest(t, c.f.handler, "viewer", "strong-password")
	for _, spec := range []struct{ method, path, body string }{{"PUT", "/api/v1/plugins/ChineseSubFinder", `{"values":{"api_key":"replacement"}}`}, {"DELETE", "/api/v1/plugins/ChineseSubFinder", ""}, {"GET", "/api/v1/plugins/ChineseSubFinder/page", ""}} {
		w := nativeJSONRequest(c.f.handler, spec.method, spec.path, viewer, spec.body)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := performFormRequest(c.f.handler, "/api/v1/plugin/config", viewer, url.Values{"id": {"ChineseSubFinder"}, "config": {`{"host":"http://attacker.test","api_key":"key"}`}}); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if c.calls != 0 {
		t.Fatal("viewer dispatched")
	}
}
