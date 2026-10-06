package httpserver

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func rssDownloadSettings(ctx context.Context, task rsstaskconfig.Task, downloaders *downloaderconfig.Store, system *systemconfig.Store) (string, downloadAddOptions, error) {
	options := downloadAddOptions{SavePath: strings.TrimSpace(task.SavePath)}
	if downloaders == nil || system == nil {
		return "", options, errors.New("native download configuration is unavailable")
	}
	if task.DownloadSetting == -2 {
		return "", options, nil
	}
	settingID := task.DownloadSetting
	if settingID <= 0 {
		var err error
		settingID, err = defaultRSSDownloadSetting(ctx, system)
		if err != nil {
			return "", options, err
		}
	}
	if settingID <= 0 {
		return "", options, nil
	}
	setting, err := downloaders.GetSetting(ctx, settingID)
	if errors.Is(err, downloaderconfig.ErrSettingNotFound) && settingID == task.DownloadSetting {
		settingID, err = defaultRSSDownloadSetting(ctx, system)
		if err != nil {
			return "", options, err
		}
		if settingID > 0 {
			setting, err = downloaders.GetSetting(ctx, settingID)
		}
	}
	if errors.Is(err, downloaderconfig.ErrSettingNotFound) {
		return "", options, nil
	}
	if err != nil {
		return "", options, err
	}
	options.Category = setting.Category
	options.Paused = setting.Paused != 0
	options.UploadLimitKB = setting.UploadLimit
	options.DownloadLimitKB = setting.DownloadLimit
	options.RatioLimit = float64(setting.RatioLimit) / 100
	options.SeedingTimeLimit = setting.SeedingTimeLimit
	if setting.Tags != "" {
		options.Tags = strings.Split(setting.Tags, ";")
	}
	return strings.TrimSpace(setting.DownloaderID), options, nil
}

func defaultRSSDownloadSetting(ctx context.Context, system *systemconfig.Store) (int64, error) {
	raw, err := system.Get(ctx, "DefaultDownloadSetting")
	if err != nil || raw == "" {
		return 0, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id < 0 {
		return 0, nil
	}
	return id, nil
}
