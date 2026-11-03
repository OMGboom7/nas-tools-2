package cloudflare

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func storeFixture(t *testing.T) (*Store, *systemconfig.Store, string, Control) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "user.db")
	sys, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for key, value := range map[string]string{"UserInstalledPlugins": `["CustomHosts","CloudflareSpeedTest","KeepOther"]`, ConfigKey: `{"cf_ip":"192.0.2.1","onlyonce":true,"ipv4":true,"unknown":{"n":900719925474099312345,"null":null}}`, "plugin.CustomHosts": `{"enable":true,"hosts":["192.0.2.1 fixture.invalid"],"unknown":900719925474099312345}`} {
		if err := sys.Set(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := Control{Epoch: "fixture-epoch", Actor: "fixture-admin", Authority: "fixture-authority", Root: filepath.Join(filepath.Dir(path), "plugins"), HostsPath: filepath.Join(filepath.Dir(path), "hosts"), Location: "Asia/Shanghai", FromSignin: true}
	return s, sys, path, c
}
func activateFixture(t *testing.T, s *Store, c Control) Control {
	t.Helper()
	ctx := context.Background()
	src, err := s.Source(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Activate(ctx, c, src); err != nil {
		t.Fatal(err)
	}
	c, err = s.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStoredOriginalSwitchDoesNotGrantExecutionAndUnknownIsNotReplayed(t *testing.T) {
	s, _, _, c := storeFixture(t)
	ctx := context.Background()
	if _, err := s.Queue(ctx, "signin:original", true); err == nil {
		t.Fatal("legacy switch granted execution")
	}
	c = activateFixture(t, s, c)
	if !strings.Contains(c.Source.Config, `900719925474099312345`) || !strings.Contains(c.Source.Config, `"onlyonce":false`) {
		t.Fatalf("original unknown values or switch changed: %s", c.Source.Config)
	}
	j, err := s.Queue(ctx, "signin:original", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Queue(ctx, "manual:concurrent", false); err == nil {
		t.Fatal("concurrent occurrence accepted")
	}
	if err = s.Before(ctx, j.ID, Operation{Kind: "execute", Digest: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	old, err := s.Queue(ctx, "signin:original", true)
	if err != nil || old.ID != j.ID || old.State != "unknown" || len(old.Operations) != 1 {
		t.Fatalf("original result replayed: %+v %v", old, err)
	}
	if _, err = s.Queue(ctx, "manual:new", false); err == nil {
		t.Fatal("unknown execution did not hold local slot")
	}
	if err = s.Terminal(ctx, j.ID, "closed", "fixture acknowledgement"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Queue(ctx, "manual:new", false); err != nil {
		t.Fatal(err)
	}
}

func TestSettingsChangeRevokesQueuedWorkAndPreservesOtherPlugin(t *testing.T) {
	s, sys, _, c := storeFixture(t)
	ctx := context.Background()
	c = activateFixture(t, s, c)
	j, err := s.Queue(ctx, "manual:before-edit", false)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Patch(c.Source.Config, map[string]json.RawMessage{"cron": json.RawMessage(`"0 9 * * *"`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Configure(ctx, c.Source, raw, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.Before(ctx, j.ID, Operation{Kind: "execute"}); err == nil {
		t.Fatal("revoked queued task dispatched")
	}
	installed, err := sys.Get(ctx, "UserInstalledPlugins")
	if err != nil || installed != c.Source.Installed {
		t.Fatalf("other installation state changed: %s %v", installed, err)
	}
	config, err := sys.Get(ctx, ConfigKey)
	if err != nil || !strings.Contains(config, "900719925474099312345") {
		t.Fatal("unknown original integer changed")
	}
}

func TestAfterImageFinalizationRollsBackAndRecoversWithoutRepeatingFileWrite(t *testing.T) {
	s, _, path, c := storeFixture(t)
	ctx := context.Background()
	c = activateFixture(t, s, c)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	j, err := s.Queue(ctx, "manual:after-image", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Before(ctx, j.ID, Operation{Kind: "execute"}); err != nil {
		t.Fatal(err)
	}
	settings, _ := DecodeSettings(c.Source.Config)
	hosts, err := PlanHosts(c.Source.Hosts, settings, "198.51.100.2")
	if err != nil {
		t.Fatal(err)
	}
	plan := ApplyPlan{Hosts: hosts, FileBefore: "before", FileBeforeHash: sum([]byte("before")), FileAfter: "after-file"}
	if err = s.PrepareApply(ctx, j.ID, plan, Result{BestIP: "198.51.100.2", Version: "v2.3.5"}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER reject_cf BEFORE UPDATE ON SYSTEM_DICT WHEN NEW.KEY='plugin.CustomHosts' BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Finalize(ctx, j.ID, sum([]byte(plan.FileAfter))); err == nil {
		t.Fatal("failed cross-setting commit claimed success")
	}
	current, err := s.Source(ctx)
	if err != nil || current != c.Source {
		t.Fatal("partial DB settings escaped rollback")
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	unknown, err := s.Get(ctx, j.ID)
	if err != nil || unknown.State != "unknown" {
		t.Fatalf("interruption=%+v %v", unknown, err)
	}
	if _, err = db.Exec(`DROP TRIGGER reject_cf`); err != nil {
		t.Fatal(err)
	}
	if err = s.Finalize(ctx, j.ID, sum([]byte("different-file"))); err == nil {
		t.Fatal("changed file accepted as original after-image")
	}
	if err = s.Finalize(ctx, j.ID, sum([]byte(plan.FileAfter))); err != nil {
		t.Fatal(err)
	}
	complete, err := s.Get(ctx, j.ID)
	if err != nil || complete.State != "complete" || len(complete.Operations) != 2 {
		t.Fatalf("recovered=%+v %v", complete, err)
	}
	if err = s.Finalize(ctx, j.ID, sum([]byte(plan.FileAfter))); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Source(ctx)
	if err != nil || updated.Hosts != hosts.Raw || !strings.Contains(updated.Config, `"cf_ip":"198.51.100.2"`) || !strings.Contains(updated.Config, "900719925474099312345") {
		t.Fatalf("recovered values=%+v err=%v", updated, err)
	}
}
