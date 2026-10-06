package httpserver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func TestRSSDownloadSettingsSelectsTaskOrDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user.db")
	downloaders, err := downloaderconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer downloaders.Close()
	system, err := systemconfig.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	ctx := context.Background()
	setting, err := downloaders.UpsertSetting(ctx, downloaderconfig.DownloadSetting{Name: "Default", Category: "tv", Tags: "rss; 4k", Paused: 1, UploadLimit: 100, DownloadLimit: 200, RatioLimit: 150, SeedingTimeLimit: 60, DownloaderID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if err := system.Set(ctx, "DefaultDownloadSetting", "1"); err != nil {
		t.Fatal(err)
	}
	task := rsstaskconfig.Task{SavePath: "/downloads/rss"}
	id, options, err := rssDownloadSettings(ctx, task, downloaders, system)
	if err != nil || id != "7" || options.SavePath != "/downloads/rss" || options.Category != "tv" || len(options.Tags) != 2 || !options.Paused || options.UploadLimitKB != 100 || options.RatioLimit != 1.5 || options.SeedingTimeLimit != 60 {
		t.Fatalf("id=%q options=%+v err=%v", id, options, err)
	}
	task.DownloadSetting = setting.ID
	id, _, err = rssDownloadSettings(ctx, task, downloaders, system)
	if err != nil || id != "7" {
		t.Fatalf("explicit setting=%q err=%v", id, err)
	}
	task.DownloadSetting = -2
	id, options, err = rssDownloadSettings(ctx, task, downloaders, system)
	if err != nil || id != "" || options.SavePath != "/downloads/rss" || options.Category != "" {
		t.Fatalf("bypass setting=%q options=%+v err=%v", id, options, err)
	}
}
