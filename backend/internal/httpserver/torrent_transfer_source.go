package httpserver

import (
	"context"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type protectedTransferSource struct {
	torrenttransfer.SourceClient
	service organizationAPI
	item    downloaderconfig.Downloader
	client  completionOrganizationClient
}

func (api organizationAPI) transferSource(item downloaderconfig.Downloader) (torrenttransfer.ProtectedSource, error) {
	if api.jobs == nil || item.Enabled != 1 {
		return nil, torrenttransfer.ErrState
	}
	client, err := api.clientFromDownloader(item)
	if err != nil {
		return nil, err
	}
	if client.transfer == nil || client.remove == nil {
		return nil, torrenttransfer.ErrState
	}
	return protectedTransferSource{SourceClient: client.transfer, service: api, item: item, client: client}, nil
}

func (s protectedTransferSource) RemoveTransferKeepingFiles(ctx context.Context, expected torrenttransfer.Task) error {
	if !expected.Valid() || expected.Kind != s.item.Type {
		return torrenttransfer.ErrState
	}
	guard, err := s.service.jobs.AcquireJob(ctx, torrentLabelGuard(s.item.ID, expected.WireID))
	if err != nil {
		return err
	}
	defer guard.Close()
	current, err := s.service.service.downloaders.Get(ctx, s.item.ID)
	if err != nil {
		return err
	}
	if removalDownloaderDigest(current) != removalDownloaderDigest(s.item) {
		return torrenttransfer.ErrState
	}
	task, err := s.TransferTask(ctx, expected.WireID)
	if err != nil {
		return err
	}
	if !expected.SameInstance(task) || !task.Complete || task.Error || task.Checking || (task.Kind == "transmission" && !task.SourceSeeding) {
		return torrenttransfer.ErrState
	}
	// The organization client acquires the seeding/profile guards and checks
	// PT obligations before invoking the protocol's keep-files operation.
	return s.client.remove(ctx, expected.WireID)
}

func (s protectedTransferSource) CheckTransferRemoval(ctx context.Context, expected torrenttransfer.Task) error {
	if !expected.Valid() || expected.Kind != s.item.Type {
		return torrenttransfer.ErrState
	}
	return s.service.seedingService().checkSeedingAction(ctx, s.item, expected.WireID, "remove")
}
