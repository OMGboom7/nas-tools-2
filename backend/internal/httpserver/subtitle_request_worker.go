package httpserver

import (
	"context"
	"errors"
	"path"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

func (api *subtitleAPI) manualRequestReady(ctx context.Context, row subtitles.Receipt, id string) (organization.SubtitleRequest, bool) {
	p, e := api.organizer.jobs.SubtitleRequest(ctx, id)
	if e != nil || p.State != "queued" || !p.Queued || p.ServiceState != "pending" && p.ServiceState != "sending" || row.ID != organization.Digest([]string{organization.SubtitleRequestSource(id), "0"}) || row.JobID != organization.SubtitleRequestSource(id) || row.Ordinal != 0 || row.LocalPath != path.Join(p.Definition.Scope.Root.Path, p.Path) || row.OriginDigest != p.Fingerprint {
		return p, false
	}
	if api.requestCurrent(ctx, p.Definition) != nil {
		return p, false
	}
	scope, e := api.queue.Snapshot(ctx, row.LocalPath, row.MediaType, false)
	return p, e == nil && organization.Digest(scope) == organization.Digest(p.Definition.Plugin)
}
func (api *subtitleAPI) runManualRequest(ctx context.Context, row subtitles.Receipt, id string) error {
	ctx, cancelRequest := context.WithTimeout(ctx, organization.ExecutionTimeout)
	defer cancelRequest()
	closeGuards, e := api.requestGuards(ctx, true)
	if e != nil {
		return e
	}
	defer closeGuards()
	p, valid := api.manualRequestReady(ctx, row, id)
	cfg, claimed, e := api.queue.Claim(ctx, row)
	if errors.Is(e, subtitles.ErrState) {
		return nil
	}
	if e != nil {
		return e
	}
	state, external := "blocked", ""
	if valid {
		c, closeRemote, e := api.requestContext(ctx, p.Definition)
		valid = e == nil && organization.VerifySubtitleRequestSource(ctx, p.Definition, c) == nil
		closeRemote()
	}
	if valid {
		_, valid = api.manualRequestReady(ctx, claimed, id)
		if valid {
			call, cancel := context.WithTimeout(ctx, 20*time.Second)
			external, e = subtitles.Submit(call, cfg, claimed.Payload, api.transport)
			cancel()
			state = "uncertain"
			if e == nil {
				state = "submitted"
			}
		}
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return api.queue.Finish(persist, claimed.ID, state, external)
}
