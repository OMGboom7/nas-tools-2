package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

type Config struct {
	Address               string
	LegacyBackendURL      string
	FrontendDist          string
	ApplicationConfigPath string
	SiteCatalogPath       string
	DefaultCategoryPath   string
	DisableLegacy         bool
	HostsPath             string
	RcloneConfigFile      string
	RcloneBinary          string
	MinioConfigFile       string
	MinioCAFile           string
}

func FromEnv() (Config, error) {
	cfg := Config{
		Address:               envOrDefault("NASTOOL_GO_ADDRESS", ":3001"),
		LegacyBackendURL:      envOrDefault("NASTOOL_LEGACY_URL", "http://127.0.0.1:3000"),
		FrontendDist:          os.Getenv("NASTOOL_FRONTEND_DIST"),
		ApplicationConfigPath: os.Getenv("NASTOOL_CONFIG"),
		SiteCatalogPath:       os.Getenv("NASTOOL_SITE_CATALOG"),
		DefaultCategoryPath:   os.Getenv("NASTOOL_DEFAULT_CATEGORY"),
		DisableLegacy:         os.Getenv("NASTOOL_DISABLE_LEGACY") == "true",
		HostsPath:             os.Getenv("NASTOOL_GO_HOSTS_PATH"),
		RcloneConfigFile:      os.Getenv("NASTOOL_RCLONE_CONFIG"),
		RcloneBinary:          envOrDefault("NASTOOL_RCLONE_BINARY", "rclone"),
		MinioConfigFile:       os.Getenv("NASTOOL_MINIO_CONFIG"),
		MinioCAFile:           os.Getenv("NASTOOL_MINIO_CA"),
	}
	if cfg.DisableLegacy {
		cfg.LegacyBackendURL = ""
	}
	if cfg.HostsPath != "" && (!filepath.IsAbs(cfg.HostsPath) || !cfg.DisableLegacy) {
		return Config{}, fmt.Errorf("NASTOOL_GO_HOSTS_PATH requires an absolute path and NASTOOL_DISABLE_LEGACY=true")
	}
	if cfg.RcloneConfigFile != "" && (!filepath.IsAbs(cfg.RcloneConfigFile) || !cfg.DisableLegacy) {
		return Config{}, fmt.Errorf("NASTOOL_RCLONE_CONFIG requires an absolute path and NASTOOL_DISABLE_LEGACY=true")
	}
	if cfg.MinioConfigFile != "" && (!filepath.IsAbs(cfg.MinioConfigFile) || !cfg.DisableLegacy) || cfg.MinioCAFile != "" && (!filepath.IsAbs(cfg.MinioCAFile) || cfg.MinioConfigFile == "" || !cfg.DisableLegacy) {
		return Config{}, fmt.Errorf("NASTOOL_MINIO_CONFIG and optional NASTOOL_MINIO_CA require absolute paths and NASTOOL_DISABLE_LEGACY=true")
	}

	if cfg.LegacyBackendURL != "" {
		if _, err := url.ParseRequestURI(cfg.LegacyBackendURL); err != nil {
			return Config{}, fmt.Errorf("NASTOOL_LEGACY_URL: %w", err)
		}
	}

	return cfg, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
