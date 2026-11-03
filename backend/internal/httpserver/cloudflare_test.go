package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/cloudflare"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/customhosts"
)

func cfFixture(t *testing.T) (*loginFixture, *cloudflareAPI, string) {
	t.Helper()
	f := newLoginFixture(t, nil)
	api := f.signin.cloudflare
	if api == nil {
		t.Fatal("production CF runtime missing")
	}
	t.Cleanup(func() { api.store.Close() })
	dir := filepath.Dir(f.configPath)
	hostPath := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hostPath, []byte("127.0.0.1 localhost\n# keep fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	api.hosts = &nativeHostsPlugin{path: hostPath}
	if err := os.MkdirAll(api.root, 0700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "build", "-o", filepath.Join(api.root, "CloudflareST"), "../cloudflare/testdata/fixture/main.go")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("compile isolated Go CLI: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(api.root, "ip.txt"), []byte("198.18.0.0/24\n"), 0600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "cli-effects.txt")
	args, _ := json.Marshal("-dd -fixture-record " + record)
	for key, value := range map[string]string{"UserInstalledPlugins": `["CloudflareSpeedTest","CustomHosts"]`, cloudflare.ConfigKey: `{"cf_ip":"192.0.2.1","onlyonce":true,"additional_args":` + string(args) + `,"unknown":900719925474099312345}`, "plugin.CustomHosts": `{"enable":true,"hosts":"192.0.2.1 fixture.invalid","unknown":900719925474099312345}`} {
		if err := f.signin.system.Set(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	return f, api, record
}
func cfState(t *testing.T, f *loginFixture) map[string]any {
	t.Helper()
	response := f.request(t, "GET", "/api/v1/cloudflare", nil)
	var payload struct{ Data map[string]any }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &payload) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	return payload.Data
}
func cfActivate(t *testing.T, f *loginFixture) {
	t.Helper()
	status := cfState(t, f)
	response := f.request(t, "POST", "/api/v1/cloudflare/activate", map[string]any{"fingerprint": status["fingerprint"], "fromSignin": true, "confirm": true})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
}
func TestCloudflareNativeHTTPRunsOriginalGoCLIOnceAndPreservesReceipt(t *testing.T) {
	f, api, record := cfFixture(t)
	ctx := context.Background()
	// Old onlyonce did not acquire execution rights merely by starting a tick.
	if err := api.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("legacy onlyonce ran")
	}
	cfActivate(t, f)
	status := cfState(t, f)
	nonce := "fixture-original-request"
	response := f.request(t, "POST", "/api/v1/cloudflare/run", map[string]any{"fingerprint": status["fingerprint"], "nonce": nonce, "confirm": true})
	var result struct{ Data struct{ ID string } }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	c, err := api.store.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// An independently due once must not starve an already queued manual job.
	if err = api.store.Advance(ctx, c.Epoch, "", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err = api.tick(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := api.store.Get(ctx, result.Data.ID)
	if err != nil || j.State != "complete" {
		t.Fatalf("job=%+v err=%v", j, err)
	}
	data, err := os.ReadFile(record)
	if err != nil || string(data) != "executed\n" {
		t.Fatalf("effects=%q %v", data, err)
	}
	hosts, err := os.ReadFile(api.hosts.path)
	if err != nil || !strings.Contains(strings.Join(strings.Fields(string(hosts)), " "), "198.18.0.1 fixture.invalid") || !strings.Contains(string(hosts), "# keep fixture") {
		t.Fatalf("hosts=%q %v", hosts, err)
	}
	before := encodeCFJob(j)
	if err = api.store.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	response = f.request(t, "POST", "/api/v1/cloudflare/run", map[string]any{"fingerprint": "old", "nonce": nonce, "confirm": true})
	if response.Code != 200 {
		t.Fatal("original confirmation not reusable", response.Code, response.Body.String())
	}
	if err = api.tick(ctx); err != nil {
		t.Fatal(err)
	}
	j, err = api.store.Get(ctx, result.Data.ID)
	if err != nil || encodeCFJob(j) != before {
		t.Fatal("original receipt changed")
	}
	data, _ = os.ReadFile(record)
	if string(data) != "executed\n" {
		t.Fatal("original action repeated")
	}
	src, err := api.store.Source(ctx)
	if err != nil || !strings.Contains(src.Config, "900719925474099312345") || !strings.Contains(src.Hosts, "900719925474099312345") {
		t.Fatal("original unknown JSON changed")
	}
}
func encodeCFJob(j cloudflare.Job) string { data, _ := json.Marshal(j); return string(data) }
func TestCloudflareExactHostsWriteRefusesInterveningEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	original := "127.0.0.1 localhost\n"
	changed := original + "# keep later edit\n"
	if err := os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := customhosts.ApplyExact(t.Context(), path, "198.18.0.1 fixture.invalid", original); err == nil {
		t.Fatal("intervening file edit overwritten")
	}
	after, _ := os.ReadFile(path)
	if string(after) != changed {
		t.Fatal("file changed after rejected plan")
	}
}

func TestCloudflareStartupPreservesAfterImageUntilOriginalDBCommitCanRecover(t *testing.T) {
	f, api, _ := cfFixture(t)
	ctx := context.Background()
	cfActivate(t, f)
	c, err := api.store.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j, err := api.store.Queue(ctx, "fixture:interrupted-apply", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = api.store.Before(ctx, j.ID, cloudflare.Operation{Kind: "execute"}); err != nil {
		t.Fatal(err)
	}
	settings, _ := cloudflare.DecodeSettings(c.Source.Config)
	plan, err := cloudflare.PlanHosts(c.Source.Hosts, settings, "198.18.0.1")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(api.hosts.path)
	if err != nil {
		t.Fatal(err)
	}
	mappings, _, err := customhosts.Parse(ctx, plan.Input)
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := customhosts.Rewrite(string(before), mappings)
	if err != nil {
		t.Fatal(err)
	}
	if err = api.store.PrepareApply(ctx, j.ID, cloudflare.ApplyPlan{Hosts: plan, FileBefore: string(before), FileBeforeHash: cfHash(before), FileAfter: after}, cloudflare.Result{BestIP: "198.18.0.1", Version: "v2.3.5"}); err != nil {
		t.Fatal(err)
	}
	if _, err = customhosts.ApplyExact(ctx, api.hosts.path, plan.Input, string(before)); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER hold_cf BEFORE UPDATE ON SYSTEM_DICT WHEN NEW.KEY='plugin.CustomHosts' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	restart := func() *cloudflareAPI {
		t.Helper()
		var runner *rssRunAPI
		_, err := buildHandler(config.Config{DisableLegacy: true, ApplicationConfigPath: f.configPath, HostsPath: api.hosts.path}, api.transport, &runner)
		if err != nil {
			t.Fatal(err)
		}
		reopened := runner.cloudflare
		t.Cleanup(func() { reopened.store.Close() })
		return reopened
	}
	restart()
	kept, _ := os.ReadFile(api.hosts.path)
	if string(kept) != after {
		t.Fatal("startup overwrote uncommitted after-image")
	}
	if _, err = db.Exec(`DROP TRIGGER hold_cf`); err != nil {
		t.Fatal(err)
	}
	newAPI := restart()
	recovered, err := newAPI.store.Get(ctx, j.ID)
	if err != nil || recovered.State != "complete" {
		t.Fatalf("original recovery=%+v %v", recovered, err)
	}
	kept, _ = os.ReadFile(api.hosts.path)
	if string(kept) != after {
		t.Fatal("recovery rewrote original after-image")
	}
	if recovered.Plan.FileBefore != string(before) {
		t.Fatal("full before-image was lost")
	}
}

func TestCloudflareChangedReviewCannotAdoptNewSettingsAndEditorTextIsEditable(t *testing.T) {
	f, api, _ := cfFixture(t)
	status := cfState(t, f)
	response := f.request(t, "GET", "/api/v1/plugins/CloudflareSpeedTest", nil)
	var detail struct{ Data pluginConfigDetail }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &detail) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, field := range detail.Data.Fields {
		if field.Key == "cf_ip" || field.Key == "cron" || field.Key == "additional_args" {
			if !field.WriteOnly || field.ReadOnly {
				t.Fatalf("generic editor cannot submit text replacements: %+v", field)
			}
		}
	}
	response = f.request(t, "PUT", "/api/v1/plugins/CloudflareSpeedTest", map[string]any{"fingerprint": detail.Data.Meta.Fingerprint, "values": map[string]any{"cron": "0 9 * * *"}})
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = f.request(t, "POST", "/api/v1/cloudflare/activate", map[string]any{"fingerprint": status["fingerprint"], "confirm": true})
	if response.Code != 409 {
		t.Fatal("stale review acquired new configuration", response.Code)
	}
	c, err := api.store.Control(t.Context())
	if err != nil || c.Enabled {
		t.Fatal("unreviewed settings acquired execution")
	}
}
