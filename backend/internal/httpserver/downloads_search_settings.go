package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

var errSearchDownloadSelection = errors.New("invalid search download selection")

// Resolve current server-side settings, never client-provided downloader IDs or
// arbitrary paths. An empty directory keeps the downloader's own default.
func (service downloadService) searchDownloadSettings(ctx context.Context, siteName string, input addResourceRequest) (downloaderconfig.Downloader, downloadAddOptions, error) {
	var downloader downloaderconfig.Downloader
	options := downloadAddOptions{}
	if service.downloaders == nil || service.systemConfig == nil || service.sites == nil {
		return downloader, options, errors.New("native download configuration is unavailable")
	}
	settingID := strings.TrimSpace(input.Setting)
	directory := strings.TrimSpace(input.Directory)
	if len(input.Setting) > 32 || len(input.Directory) > 4096 || strings.ContainsFunc(input.Directory, unicode.IsControl) {
		return downloader, options, errSearchDownloadSelection
	}
	sites, err := service.sites.List(ctx)
	if err != nil {
		return downloader, options, err
	}
	siteTags := ""
	matched := false
	for _, site := range sites {
		if site.Name != siteName {
			continue
		}
		if matched {
			return downloader, options, errors.New("ambiguous download site")
		}
		matched = true
		if site.Note == "" {
			continue
		}
		var note map[string]json.RawMessage
		if len(site.Note) > 64<<10 || json.Unmarshal([]byte(site.Note), &note) != nil || note == nil {
			return downloader, options, errors.New("invalid site download configuration")
		}
		if raw, exists := note["tags"]; exists && json.Unmarshal(raw, &siteTags) != nil {
			return downloader, options, errors.New("invalid site download tags")
		}
		if settingID == "" {
			if raw, exists := note["download_setting"]; exists && json.Unmarshal(raw, &settingID) != nil {
				return downloader, options, errors.New("invalid site download setting")
			}
			settingID = strings.TrimSpace(settingID)
		}
	}
	defaultID := func() (string, error) {
		value, err := service.systemConfig.Get(ctx, "DefaultDownloadSetting")
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value == "" || value == "0" {
			return "-1", nil
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || (id <= 0 && id != -1) {
			return "", errors.New("invalid default download setting")
		}
		return value, nil
	}
	if settingID == "" {
		settingID, err = defaultID()
		if err != nil {
			return downloader, options, err
		}
	}
	id, err := strconv.ParseInt(settingID, 10, 64)
	if err != nil || (id <= 0 && id != -1 && id != -2) {
		return downloader, options, errSearchDownloadSelection
	}
	var setting downloaderconfig.DownloadSetting
	if id > 0 {
		setting, err = service.downloaders.GetSetting(ctx, id)
		// Preserve the legacy fallback for settings removed since the search.
		if errors.Is(err, downloaderconfig.ErrSettingNotFound) {
			settingID, err = defaultID()
			if err != nil {
				return downloader, options, err
			}
			id, _ = strconv.ParseInt(settingID, 10, 64)
			if id > 0 {
				setting, err = service.downloaders.GetSetting(ctx, id)
			}
			if errors.Is(err, downloaderconfig.ErrSettingNotFound) {
				id, err = -1, nil
			}
		}
		if err != nil {
			return downloader, options, err
		}
	}
	downloaderID := ""
	if id > 0 {
		downloaderID = strings.TrimSpace(setting.DownloaderID)
		options = downloadAddOptions{Category: setting.Category, Paused: setting.Paused != 0, UploadLimitKB: setting.UploadLimit, DownloadLimitKB: setting.DownloadLimit, RatioLimit: float64(setting.RatioLimit) / 100, SeedingTimeLimit: setting.SeedingTimeLimit}
		if setting.Tags != "" {
			options.Tags = strings.Split(setting.Tags, ";")
		}
	}
	var handled bool
	downloader, _, handled, err = service.configuredDownloader(ctx, downloaderID)
	if err != nil {
		return downloader, options, err
	}
	if !handled || downloader.ID == 0 || downloader.Enabled == 0 {
		return downloader, options, errors.New("selected downloader is unavailable or disabled")
	}
	// The legacy preset labels supported clients with NASTOOL. Do not send a
	// synthetic label to clients whose protocol has no label support.
	if id == -1 && (downloader.Type == "qbittorrent" || downloader.Type == "transmission") {
		options.Tags = []string{"NASTOOL"}
	}
	if siteTags != "" {
		options.Tags = append(options.Tags, strings.Split(siteTags, ";")...)
	}
	if directory != "" {
		var rules []struct {
			SavePath string `json:"save_path"`
		}
		if len(downloader.DownloadDir) > 1<<20 || json.Unmarshal([]byte(downloader.DownloadDir), &rules) != nil {
			return downloader, options, errSearchDownloadSelection
		}
		allowed := false
		for _, rule := range rules {
			allowed = allowed || strings.TrimSpace(rule.SavePath) == directory
		}
		if !allowed {
			return downloader, options, errSearchDownloadSelection
		}
		options.SavePath = directory
	}
	if !searchDownloadOptionsSupported(downloader.Type, options) {
		return downloader, options, errDownloadOptionsUnsupported
	}
	return downloader, options, nil
}

func searchDownloadOptionsSupported(kind string, options downloadAddOptions) bool {
	switch kind {
	case "qbittorrent":
		return true
	case "transmission":
		return options.Category == "" && options.UploadLimitKB == 0 && options.DownloadLimitKB == 0 && options.RatioLimit == 0 && options.SeedingTimeLimit == 0
	case "aria2":
		return options.Category == "" && len(options.Tags) == 0 && options.RatioLimit == 0 && options.SeedingTimeLimit == 0
	case "pan115":
		return options.empty()
	default:
		return false
	}
}
