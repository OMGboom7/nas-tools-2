package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/iyuu"
)

func (f *iyuuFixture) configure(v map[string]any, status int) {
	path := strings.TrimSuffix(iyuuBase, "/")
	reply := f.request("GET", path, nil, 200)
	var detail struct{ Data pluginConfigDetail }
	if err := json.Unmarshal(reply.Body.Bytes(), &detail); err != nil {
		f.t.Fatal(err)
	}
	f.request("PUT", path, map[string]any{"fingerprint": detail.Data.Meta.Fingerprint, "values": v}, status)
}
func (f *iyuuFixture) schedule() iyuuSchedule {
	f.t.Helper()
	raw, err := f.api.transfer.system.ReadMany(f.t.Context(), iyuuKeys)
	if err != nil {
		f.t.Fatal(err)
	}
	state, err := readIyuuSchedule(raw[iyuuControlKey])
	if err != nil {
		f.t.Fatal(err)
	}
	return state
}
func (f *iyuuFixture) tickAt(at time.Time) {
	f.t.Helper()
	if err := f.api.scheduleTick(f.t.Context(), at); err != nil {
		f.t.Fatal(err)
	}
}

func TestIYUUScheduledBatchResumesAfterAddAndConsumesOnce(t *testing.T) {
	f := newIyuuFixture(t)
	f.seed = &iyuuSeedFixture{}
	f.configure(map[string]any{"enable": true, "cron": "0 3 * * *", "onlyonce": true, "notify": true}, 200)
	slot := f.schedule()
	f.tickAt(slot.Once.Add(-time.Second))
	if f.seed.adds != 0 {
		t.Fatal("ran before due")
	}
	// Simulate failure to checkpoint a submitted item, then rebuild the handler.
	// The manifest and seed receipt must recover the original result.
	_, err := f.db.Exec(`CREATE TABLE GO_IYUU_BATCH(slot INTEGER PRIMARY KEY,id TEXT,items TEXT,progress TEXT);
	CREATE TRIGGER fail_checkpoint BEFORE UPDATE OF progress ON GO_IYUU_BATCH BEGIN SELECT RAISE(ABORT,'fixture checkpoint'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.api.scheduleTick(t.Context(), slot.Once); err == nil || f.seed.adds != 1 {
		t.Fatal(err, f.seed.adds)
	}
	if _, err = f.db.Exec("DROP TRIGGER fail_checkpoint"); err != nil {
		t.Fatal(err)
	}
	f.start()
	f.tickAt(slot.Once)
	f.tickAt(slot.Once)
	st := f.schedule()
	if !st.Once.IsZero() || !st.Next.Equal(slot.Next) || !strings.Contains(st.Last, "暂停添加 1 个，已有 1 个") || !strings.Contains(st.Last, "服务返回 3 条匹配") || f.seed.adds != 1 {
		t.Fatal(st, f.seed.adds)
	}
	var count int
	if err = f.db.QueryRow("SELECT COUNT(*) FROM GO_BUSINESS_EVENTS WHERE KIND='plugin_message' AND BODY=?", st.Last).Scan(&count); err != nil || count != 1 {
		t.Fatal(err, count)
	}
	f.tickAt(slot.Next)
	if !f.schedule().Next.After(slot.Next) || f.seed.adds != 1 {
		t.Fatal("cron was not advanced")
	}
	w := f.request("GET", iyuuBase+"overview", nil, 200)
	if !strings.Contains(w.Body.String(), "已处理候选 0 个") {
		t.Fatal(w.Body.String())
	}
}
func TestIYUUScheduledFailuresAndClearCache(t *testing.T) {
	for _, tc := range []struct {
		code int
		key  string
	}{{404, "error_caches"}, {410, "permanent_error_caches"}, {503, ""}} {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			f := newIyuuFixture(t)
			f.seed = &iyuuSeedFixture{}
			f.configure(map[string]any{"enable": true, "onlyonce": true}, 200)
			f.api.downloads.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "site.fixture" {
					p := jsonResponse(r, "missing")
					p.StatusCode = tc.code
					return p, nil
				}
				return f.upstream(r)
			})
			f.tickAt(f.schedule().Once)
			if !strings.Contains(f.schedule().Last, "提交失败 1 个") || f.seed.adds != 0 {
				t.Fatal(f.schedule())
			}
			read := func() iyuu.Settings {
				raw, e := f.api.transfer.system.ReadMany(t.Context(), iyuuKeys)
				if e != nil {
					t.Fatal(e)
				}
				c, e := iyuu.Read(raw[iyuu.ConfigKey])
				if e != nil {
					t.Fatal(e)
				}
				return c
			}
			cfg := read()
			if cfg.Failed[iyuuTarget] != (tc.key == "error_caches") || cfg.Permanent[iyuuTarget] != (tc.key == "permanent_error_caches") {
				t.Fatal("wrong cache", cfg.Failed, cfg.Permanent)
			}
			f.configure(map[string]any{"clearcache": true}, 200)
			cfg = read()
			if cfg.Clear || cfg.Failed[iyuuTarget] || cfg.Permanent[iyuuTarget] != (tc.code == 410) {
				t.Fatal("incorrect clear")
			}
		})
	}
}
func TestIYUUImportsLegacyScheduleAndStopsOnScopeChange(t *testing.T) {
	f := newIyuuFixture(t)
	f.seed = &iyuuSeedFixture{}
	_, err := f.db.Exec(`UPDATE SYSTEM_DICT SET VALUE=json_set(VALUE,'$.enable',json('true'),'$.onlyonce',json('true'),'$.cron','0 3 * * *') WHERE KEY='plugin.IYUUAutoSeed'`)
	if err != nil {
		t.Fatal(err)
	}
	f.tickAt(time.Now())
	st := f.schedule()
	if st.Once.IsZero() || st.Next.IsZero() {
		t.Fatal(st)
	}
	_, err = f.db.Exec("UPDATE CONFIG_SITE SET SIGNURL='https://other.fixture/login' WHERE ID=10")
	if err != nil {
		t.Fatal(err)
	}
	f.tickAt(st.Once)
	if !f.schedule().Once.IsZero() || !f.schedule().Next.IsZero() || f.lookups != 0 || f.seed.adds != 0 {
		t.Fatal("stale scope executed")
	}
	f.configure(map[string]any{"cron": "invalid"}, 400)
	f.configure(map[string]any{"enable": false, "onlyonce": true}, 400)
}

func TestIYUUAutomaticLostReplyIsNeverResent(t *testing.T) {
	f := newIyuuFixture(t)
	f.seed = &iyuuSeedFixture{lost: true}
	f.configure(map[string]any{"enable": true, "onlyonce": true, "cron": "4 * * * *"}, 200)
	st := f.schedule()
	f.tickAt(st.Once)
	if !strings.Contains(f.schedule().Last, "待核实 1 个") || f.seed.adds != 1 {
		t.Fatal(f.schedule(), f.seed)
	}
	f.start()
	// The downloader no longer shows the target. Its lost response is still
	// owned by the original receipt, even when the next scheduled batch sees it.
	f.seed.state = ""
	f.tickAt(st.Next)
	if !strings.Contains(f.schedule().Last, "待核实 1 个") || f.seed.adds != 1 || f.seed.starts != 0 {
		t.Fatal(f.schedule(), f.seed)
	}
}
