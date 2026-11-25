package database

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSharedPluginInstallationMergesAndPreservesOtherRecords(t *testing.T) {
	db, _, cfg := initialFixture(t, "app: {}\n")
	const settings = `{"unknown":"kept"}`
	if _, err := db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES ('SystemConfig','plugin.外部扩展',?),('SystemConfig','UserInstalledPlugins','["PreviouslyInstalled","外部扩展","外部扩展"]')`, settings); err != nil {
		t.Fatal(err)
	}
	changes := [][]string{{"install", "MovieRandom"}, {"install", "ThirdParty_1"}, {"install", "ThirdParty_1"}, {"uninstall", "外部扩展"}, {"uninstall", "Missing"}}
	errs := make(chan error, len(changes))
	for _, args := range changes {
		go func() { errs <- PluginInstallation(t.Context(), cfg, args, io.Discard) }()
	}
	for range changes {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := PluginInstallation(t.Context(), cfg, []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	var ids []string
	if err := json.Unmarshal(out.Bytes(), &ids); err != nil || len(ids) != 3 || ids[0] != "PreviouslyInstalled" || !slices.Contains(ids, "MovieRandom") || !slices.Contains(ids, "ThirdParty_1") {
		t.Fatal(ids, err)
	}
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.外部扩展'", settings)
	if _, err := db.Exec(`CREATE TRIGGER deny_plugin_change BEFORE UPDATE ON SYSTEM_DICT WHEN NEW.KEY='UserInstalledPlugins' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := PluginInstallation(t.Context(), cfg, []string{"uninstall", "MovieRandom"}, io.Discard); err == nil {
		t.Fatal("ignored membership change returned success")
	}
	var after bytes.Buffer
	if err := PluginInstallation(t.Context(), cfg, []string{"list"}, &after); err != nil || after.String() != out.String() {
		t.Fatal("failed write changed installation", err)
	}
}

func TestPluginInstallationRequiresExistingDatabaseAndValidList(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	for _, args := range [][]string{{"list"}, {"install", "Valid"}, {"install", "bad.id"}, {"invalid"}} {
		if err := PluginInstallation(t.Context(), cfg, args, io.Discard); err == nil {
			t.Fatal("unexpected success", args)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg), "user.db")); !os.IsNotExist(err) {
		t.Fatal("created empty database", err)
	}
	db, _, cfg := initialFixture(t, "app: {}\n")
	const invalid = `["ThirdParty","bad.id"]`
	if _, err := db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','UserInstalledPlugins',?)`, invalid); err != nil {
		t.Fatal(err)
	}
	if err := PluginInstallation(t.Context(), cfg, []string{"install", "New"}, io.Discard); err == nil {
		t.Fatal("overwrote invalid list")
	}
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='UserInstalledPlugins'", invalid)
}

func TestScopedPluginLifecycleUsesCommittedMembershipAndOwnConfig(t *testing.T) {
	db, _, cfg := initialFixture(t, "app: {}\n")
	const settings = `{"large":900719925474099312345}`
	if _, err := db.Exec(`INSERT INTO SYSTEM_DICT(TYPE,KEY,VALUE) VALUES('SystemConfig','plugin.扩展',?)`, settings); err != nil {
		t.Fatal(err)
	}
	check := func(action, config string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := PluginInstallation(t.Context(), cfg, args, &out); err != nil {
			t.Fatal(args, err)
		}
		var plan struct {
			Action  string
			Config  json.RawMessage
			Receipt string
		}
		if json.Unmarshal(out.Bytes(), &plan) != nil || plan.Action != action || string(plan.Config) != config {
			t.Fatal(args, out.String())
		}
		if (action == "keep") != (plan.Receipt == "") {
			t.Fatal("unexpected receipt", action, plan.Receipt)
		}
		return plan.Receipt
	}
	check("keep", "null", "restore", "扩展")
	first := check("start", settings, "apply", "install", "扩展", "idle")
	second := check("start", settings, "restore", "扩展")
	if err := PluginInstallation(t.Context(), cfg, []string{"ack", "扩展", first}, io.Discard); err == nil {
		t.Fatal("stale acknowledgement cleared a newer start")
	}
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='GoPluginLifecycle.扩展'", second)
	if err := PluginInstallation(t.Context(), cfg, []string{"ack", "扩展", second}, io.Discard); err != nil {
		t.Fatal(err)
	}
	check("keep", "null", "apply", "install", "扩展", "running")
	check("start", settings, "restore", "扩展")
	check("stop", "null", "apply", "uninstall", "扩展", "running")
	check("keep", "null", "apply", "uninstall", "扩展", "idle")
	check("keep", "null", "apply", "install", "Jackett", "unavailable")
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='plugin.扩展'", settings)
	if _, err := db.Exec(`UPDATE SYSTEM_DICT SET VALUE='{' WHERE KEY='plugin.扩展'`); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := PluginInstallation(t.Context(), cfg, []string{"apply", "install", "扩展", "idle"}, &out); err == nil || out.Len() != 0 {
		t.Fatal("accepted unreadable configuration", err, out.String())
	}
	requireSchemaValue(t, db, "SELECT VALUE FROM SYSTEM_DICT WHERE KEY='UserInstalledPlugins'", `["Jackett"]`)
}
