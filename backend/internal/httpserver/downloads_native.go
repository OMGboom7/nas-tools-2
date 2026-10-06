package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/pan115"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

var errDownloaderControlUnsupported = errors.New("downloader task control is not migrated")

func (service downloadService) configuredDownloader(ctx context.Context, requestedID string) (downloaderconfig.Downloader, string, bool, error) {
	if service.downloaders == nil || service.systemConfig == nil {
		return downloaderconfig.Downloader{}, "", false, nil
	}
	rawID := strings.TrimSpace(requestedID)
	if rawID == "" {
		var err error
		rawID, err = service.systemConfig.Get(ctx, "DefaultDownloader")
		if err != nil {
			return downloaderconfig.Downloader{}, "", true, err
		}
	}
	if rawID == "" {
		return downloaderconfig.Downloader{}, "", true, nil
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return downloaderconfig.Downloader{}, "", true, errors.New("invalid downloader")
	}
	item, err := service.downloaders.Get(ctx, id)
	if errors.Is(err, downloaderconfig.ErrNotFound) {
		return downloaderconfig.Downloader{}, "", true, errors.New("downloader not found")
	}
	if err != nil {
		return downloaderconfig.Downloader{}, "", true, err
	}
	return item, rawID, true, nil
}

func (service downloadService) nativeQbittorrent(ctx context.Context, requestedID string) (*qbittorrent.Client, string, bool, error) {
	item, rawID, available, err := service.configuredDownloader(ctx, requestedID)
	if !available || err != nil {
		return nil, rawID, available, err
	}
	if item.ID == 0 {
		return nil, rawID, true, nil
	}
	if item.Type != "qbittorrent" {
		return nil, rawID, false, nil
	}
	configuration := map[string]any{}
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		return nil, rawID, true, errors.New("invalid qBittorrent configuration")
	}
	client, err := qbittorrent.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), service.client.Transport)
	return client, rawID, true, err
}

func (service downloadService) nativeTransmission(ctx context.Context, requestedID string) (*transmission.Client, string, bool, error) {
	item, rawID, available, err := service.configuredDownloader(ctx, requestedID)
	if !available || err != nil {
		return nil, rawID, available, err
	}
	if item.ID == 0 {
		return nil, rawID, true, nil
	}
	if item.Type != "transmission" {
		return nil, rawID, false, nil
	}
	configuration := map[string]any{}
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		return nil, rawID, true, errors.New("invalid Transmission configuration")
	}
	client, err := transmission.New(text(configuration["host"]), text(configuration["port"]), text(configuration["username"]), text(configuration["password"]), service.client.Transport)
	return client, rawID, true, err
}

func (service downloadService) nativeAria2(ctx context.Context, requestedID string) (*aria2.Client, string, bool, error) {
	item, rawID, available, err := service.configuredDownloader(ctx, requestedID)
	if !available || err != nil {
		return nil, rawID, available, err
	}
	if item.ID == 0 {
		return nil, rawID, true, nil
	}
	if item.Type != "aria2" {
		return nil, rawID, false, nil
	}
	configuration := map[string]any{}
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		return nil, rawID, true, errors.New("invalid Aria2 configuration")
	}
	client, err := aria2.New(text(configuration["host"]), text(configuration["port"]), text(configuration["secret"]), service.client.Transport)
	return client, rawID, true, err
}

func (service downloadService) nativePan115(ctx context.Context, requestedID string) (*pan115.Client, string, bool, error) {
	item, rawID, available, err := service.configuredDownloader(ctx, requestedID)
	if !available || err != nil {
		return nil, rawID, available, err
	}
	if item.ID == 0 {
		return nil, rawID, true, nil
	}
	if item.Type != "pan115" {
		return nil, rawID, false, nil
	}
	configuration := map[string]any{}
	if json.Unmarshal([]byte(item.Config), &configuration) != nil {
		return nil, rawID, true, errors.New("invalid 115 configuration")
	}
	client, err := pan115.New(text(configuration["cookie"]), service.client.Transport)
	return client, rawID, true, err
}

func (service downloadService) nativeActive(ctx context.Context) ([]downloadTask, bool, error) {
	return service.nativeActiveFor(ctx, "")
}

func (service downloadService) nativeActiveFor(ctx context.Context, requestedID string) ([]downloadTask, bool, error) {
	client, downloaderID, handled, err := service.nativeQbittorrent(ctx, requestedID)
	if err != nil {
		return nil, handled, err
	}
	if handled && client == nil {
		return []downloadTask{}, true, nil
	}
	if handled {
		tasks, taskErr := client.Tasks(ctx)
		if taskErr != nil {
			return nil, true, taskErr
		}
		result := make([]downloadTask, 0, len(tasks))
		for _, task := range tasks {
			title, image, historyErr := service.taskPresentation(ctx, downloaderID, task.Hash, task.Name)
			if historyErr != nil {
				return nil, true, historyErr
			}
			state := "Downloading"
			if strings.HasPrefix(strings.ToLower(task.State), "paused") || strings.EqualFold(task.State, "stoppedDL") {
				state = "Stoped"
			}
			result = append(result, downloadTask{ID: task.Hash, Title: title, Progress: task.Progress * 100, Speed: formatTorrentSpeed(task.DownloadSpeed, task.UploadSpeed, task.ETA), State: state, SiteURL: client.Address(), Image: image, CanControl: true, CanRemove: true, ShowProgress: true})
		}
		return result, true, nil
	}

	transmissionClient, downloaderID, handled, err := service.nativeTransmission(ctx, requestedID)
	if err != nil {
		return nil, handled, err
	}
	if handled && transmissionClient == nil {
		return []downloadTask{}, true, nil
	}
	if handled {
		tasks, taskErr := transmissionClient.Tasks(ctx)
		if taskErr != nil {
			return nil, true, taskErr
		}
		result := make([]downloadTask, 0, len(tasks))
		for _, task := range tasks {
			title, image, historyErr := service.taskPresentation(ctx, downloaderID, task.Hash, task.Name)
			if historyErr != nil {
				return nil, true, historyErr
			}
			state := "Downloading"
			if task.Status == 0 {
				state = "Stoped"
			}
			result = append(result, downloadTask{ID: task.Hash, Title: title, Progress: task.Progress * 100, Speed: formatTorrentSpeed(task.DownloadSpeed, task.UploadSpeed, task.ETA), State: state, SiteURL: transmissionClient.Address(), Image: image, CanControl: true, CanRemove: true, ShowProgress: true})
		}
		return result, true, nil
	}

	panClient, downloaderID, handled, err := service.nativePan115(ctx, requestedID)
	if err != nil {
		return nil, handled, err
	}
	if handled && panClient == nil {
		return []downloadTask{}, true, nil
	}
	if handled {
		tasks, err := panClient.Tasks(ctx)
		if err != nil {
			return nil, true, err
		}
		result := make([]downloadTask, 0, len(tasks))
		for _, task := range tasks {
			title, image, historyErr := service.taskPresentation(ctx, downloaderID, task.Hash, task.Name)
			if historyErr != nil {
				return nil, true, historyErr
			}
			result = append(result, downloadTask{ID: task.Hash, Title: title, Progress: task.Progress, State: "Downloading", Image: image, CanControl: false, CanRemove: true, ShowProgress: true})
		}
		return result, true, nil
	}

	ariaClient, downloaderID, handled, err := service.nativeAria2(ctx, requestedID)
	if !handled || err != nil {
		return nil, handled, err
	}
	if ariaClient == nil {
		return []downloadTask{}, true, nil
	}
	tasks, err := ariaClient.Tasks(ctx)
	if err != nil {
		return nil, true, err
	}
	result := make([]downloadTask, 0, len(tasks))
	for _, task := range tasks {
		title, image, historyErr := service.taskPresentation(ctx, downloaderID, task.GID, task.Name)
		if historyErr != nil {
			return nil, true, historyErr
		}
		state := "Downloading"
		if task.Status == "paused" {
			state = "Stoped"
		}
		result = append(result, downloadTask{ID: task.GID, Title: title, Progress: task.Progress * 100, Speed: formatTorrentSpeed(task.DownloadSpeed, task.UploadSpeed, 0), State: state, SiteURL: ariaClient.Address(), Image: image, CanControl: true, CanRemove: true, ShowProgress: true})
	}
	return result, true, nil
}

func (service downloadService) taskPresentation(ctx context.Context, downloaderID, taskID, fallback string) (string, string, error) {
	title, image := fallback, ""
	history, err := service.downloaders.TaskHistory(ctx, downloaderID, taskID)
	if err != nil {
		return "", "", err
	}
	if history.Title != "" {
		title = strings.TrimSpace(history.Title + formatOptional(" (", history.Year, ")") + formatOptional(" ", history.SeasonEpisode, ""))
		image = history.Poster
	}
	if service.images != nil {
		image = service.images.rewrite(image)
	}
	return title, image, nil
}

func (service downloadService) nativeHistory(ctx context.Context, page int) ([]downloadHistory, error) {
	if service.downloaders == nil {
		return nil, errors.New("download history store is unavailable")
	}
	items, err := service.downloaders.ListDownloadHistory(ctx, page, 30)
	if err != nil {
		return nil, err
	}
	result := make([]downloadHistory, 0, len(items))
	for _, item := range items {
		image := item.Poster
		if service.images != nil {
			image = service.images.rewrite(image)
		}
		result = append(result, downloadHistory{ID: item.TMDBID, Title: item.Title, Type: item.MediaType, Year: item.Year, Image: image, Torrent: item.Torrent, Date: item.Date, Site: item.Site})
	}
	return result, nil
}

func formatOptional(prefix, value, suffix string) string {
	if value == "" {
		return ""
	}
	return prefix + value + suffix
}

func formatTorrentSpeed(download, upload, eta int64) string {
	value := fmt.Sprintf("↓%s/s ↑%s/s", formatBytes(download), formatBytes(upload))
	if eta > 0 && eta < 8640000 {
		value += fmt.Sprintf(" %02d:%02d:%02d", eta/3600, (eta%3600)/60, eta%60)
	}
	return value
}

func formatBytes(value int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	number, index := float64(value), 0
	for number >= 1024 && index < len(units)-1 {
		number /= 1024
		index++
	}
	if index == 0 {
		return fmt.Sprintf("%d %s", value, units[index])
	}
	return fmt.Sprintf("%.1f %s", number, units[index])
}

func (service downloadService) nativeControl(ctx context.Context, id, action string) (bool, error) {
	client, _, handled, err := service.nativeQbittorrent(ctx, "")
	if err != nil {
		return handled, err
	}
	if handled && client == nil {
		return true, errors.New("default downloader is not configured")
	}
	if handled {
		if !qbittorrent.ValidHash(id) {
			return true, qbittorrent.ErrConfiguration
		}
		return true, client.Control(ctx, id, action)
	}
	transmissionClient, _, handled, err := service.nativeTransmission(ctx, "")
	if err != nil {
		return handled, err
	}
	if handled && transmissionClient == nil {
		return true, errors.New("default downloader is not configured")
	}
	if handled {
		if !transmission.ValidHash(id) {
			return true, transmission.ErrConfiguration
		}
		return true, transmissionClient.Control(ctx, id, action)
	}
	panClient, _, handled, err := service.nativePan115(ctx, "")
	if handled {
		if err != nil {
			return true, err
		}
		if action != "remove" {
			return true, errDownloaderControlUnsupported
		}
		if panClient == nil {
			return true, errors.New("default downloader is not configured")
		}
		return true, panClient.Delete(ctx, id)
	}
	ariaClient, _, handled, err := service.nativeAria2(ctx, "")
	if !handled || err != nil {
		return handled, err
	}
	if ariaClient == nil {
		return true, errors.New("default downloader is not configured")
	}
	if !aria2.ValidGID(id) {
		return true, aria2.ErrConfiguration
	}
	return true, ariaClient.Control(ctx, id, action)
}
