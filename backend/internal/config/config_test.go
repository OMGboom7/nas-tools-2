package config

import "testing"

func TestRcloneRequiresExplicitGoOnlyConfiguration(t *testing.T) {
	t.Setenv("NASTOOL_RCLONE_CONFIG", "/config/rclone.conf")
	t.Setenv("NASTOOL_DISABLE_LEGACY", "false")
	if _, err := FromEnv(); err == nil {
		t.Fatal("shared runtime remote config accepted")
	}
	t.Setenv("NASTOOL_DISABLE_LEGACY", "true")
	cfg, err := FromEnv()
	if err != nil || cfg.RcloneConfigFile != "/config/rclone.conf" || cfg.RcloneBinary != "rclone" {
		t.Fatal(cfg, err)
	}
	t.Setenv("NASTOOL_RCLONE_CONFIG", "relative/rclone.conf")
	if _, err := FromEnv(); err == nil {
		t.Fatal("relative remote config accepted")
	}
}

func TestDisableLegacyAllowsGoOnlyMode(t *testing.T) {
	t.Setenv("NASTOOL_DISABLE_LEGACY", "true")
	t.Setenv("NASTOOL_LEGACY_URL", "://invalid")
	cfg, err := FromEnv()
	if err != nil || !cfg.DisableLegacy || cfg.LegacyBackendURL != "" {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
}

func TestHostsRestorationRequiresExplicitOwnership(t *testing.T) {
	t.Setenv("NASTOOL_GO_HOSTS_PATH", "/tmp/nastools-test-hosts")
	t.Setenv("NASTOOL_DISABLE_LEGACY", "false")
	if _, err := FromEnv(); err == nil {
		t.Fatal("shared ownership accepted")
	}
	t.Setenv("NASTOOL_DISABLE_LEGACY", "true")
	cfg, err := FromEnv()
	if err != nil || cfg.HostsPath != "/tmp/nastools-test-hosts" {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
	t.Setenv("NASTOOL_GO_HOSTS_PATH", "relative/hosts")
	if _, err := FromEnv(); err == nil {
		t.Fatal("relative path accepted")
	}
}
