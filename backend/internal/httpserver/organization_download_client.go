package httpserver

import (
	"context"
	"encoding/json"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

type completionOrganizationClient struct {
	completed func(context.Context) ([]string, error)
	stop      func(context.Context, string) error
	read      func(context.Context, string) (downloadcompletion.Task, error)
	has       func(context.Context, string) (bool, error)
	mark      func(context.Context, string, []string) error
	remove    func(context.Context, string) error
}

func (api organizationAPI) completionOrganizationClient(ctx context.Context, o organization.DownloadOrigin) (completionOrganizationClient, error) {
	if err := api.jobs.VerifyDownloadConfig(ctx, o); err != nil {
		return completionOrganizationClient{}, err
	}
	// Construct from the exact checked row, not a helper that re-reads a
	// potentially switched host/credentials after validating the old digest.
	id, err := syncConfigID(o.DownloaderID)
	if err != nil {
		return completionOrganizationClient{}, err
	}
	item, err := api.service.downloaders.Get(ctx, id)
	if err != nil {
		return completionOrganizationClient{}, err
	}
	if organization.Digest(item) != o.ConfigDigest {
		return completionOrganizationClient{}, organization.ErrState
	}
	return api.clientFromDownloader(item)
}
func (api organizationAPI) clientFromDownloader(item downloaderconfig.Downloader) (completionOrganizationClient, error) {
	var cfg map[string]any
	if json.Unmarshal([]byte(item.Config), &cfg) != nil {
		return completionOrganizationClient{}, organization.ErrState
	}
	switch item.Type {
	case "qbittorrent":
		c, err := qbittorrent.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), api.service.client.Transport)
		if err != nil {
			return completionOrganizationClient{}, err
		}
		if c != nil {
			return completionOrganizationClient{read: c.Completion, has: c.HasTorrent, mark: c.MarkOrganized, remove: c.RemoveOrganizationTask, completed: c.CompletedIDs, stop: c.StopForOrganization}, nil
		}
	case "transmission":
		c, err := transmission.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), api.service.client.Transport)
		if err != nil {
			return completionOrganizationClient{}, err
		}
		if c != nil {
			return completionOrganizationClient{read: c.Completion, has: c.HasTorrent, mark: c.MarkOrganized, remove: c.RemoveOrganizationTask, completed: c.CompletedIDs, stop: func(ctx context.Context, id string) error { return c.Control(ctx, id, "stop") }}, nil
		}
	case "aria2":
		c, err := aria2.New(text(cfg["host"]), text(cfg["port"]), text(cfg["secret"]), api.service.client.Transport)
		if err != nil {
			return completionOrganizationClient{}, err
		}
		if c != nil {
			return completionOrganizationClient{read: c.Completion, has: c.HasOrganizationTask, mark: c.MarkOrganized, remove: c.RemoveOrganizationTask, completed: c.CompletedIDs}, nil
		}
	}
	return completionOrganizationClient{}, organization.ErrState
}
func downloadTaskStopped(kind, state string) bool {
	return kind == "qbittorrent" && (state == "pausedUP" || state == "stoppedUP") || kind == "transmission" && state == "stopped" || kind == "aria2" && state == "complete"
}
func (api organizationAPI) validateDownloadJob(ctx context.Context, job organization.Job) error {
	o := job.Definition.Download
	if o == nil {
		return nil
	}
	c, err := api.completionOrganizationClient(ctx, *o)
	if err != nil {
		return err
	}
	task, err := c.read(ctx, o.Task.ID)
	if err != nil {
		return err
	}
	if !organization.DownloadTaskMatches(o.Task, task, false) || job.Mode == "move" && !downloadTaskStopped(o.DownloaderType, task.State) {
		return organization.ErrState
	}
	return api.jobs.VerifyDownloadConfig(ctx, *o)
}
