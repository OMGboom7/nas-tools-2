package httpserver

import (
	"context"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type downloadReservationReview struct {
	BatchID     string              `json:"batchId"`
	Actor       string              `json:"actor"`
	Reason      string              `json:"reason"`
	State       string              `json:"state"`
	Jobs        []string            `json:"jobs"`
	Missing     []int               `json:"missing"`
	Plans       []organizationPlan  `json:"plans"`
	Omitted     []organization.File `json:"omitted"`
	Fingerprint string              `json:"fingerprint"`
	snapshot    string
}

func (api organizationAPI) reviewDownloadReservation(ctx context.Context, b organization.DownloadBatch, actor, reason string) (downloadReservationReview, error) {
	r := downloadReservationReview{}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 1000 || actor == "" {
		return r, organization.ErrState
	}
	saved, err := api.jobs.DownloadReservation(ctx, b.ID)
	if err != nil {
		return r, err
	}
	fresh, failure := api.planDownload(ctx, downloadCompletionInput{DownloaderID: b.Origin.DownloaderID, TaskID: b.Origin.Task.ID}, b.Origin.Actor)
	if failure != nil {
		return r, organization.ErrState
	}
	o := fresh.origin
	if !organization.DownloadTaskMatches(b.Origin.Task, o.Task, false) {
		return r, organization.ErrState
	}
	o.Task = b.Origin.Task
	o.BatchID, o.MonitorGeneration, o.MonitorEvent = b.ID, b.Origin.MonitorGeneration, b.Origin.MonitorEvent
	if organization.Digest(o) != organization.Digest(b.Origin) || organization.Digest(fresh.Omitted) != organization.Digest(b.Omitted) || len(fresh.Plans) != len(saved.Definitions) {
		return r, organization.ErrState
	}
	for i, p := range fresh.Plans {
		d := p.definition
		d.Download = saved.Definitions[i].Download
		if organization.Digest(d) != organization.Digest(saved.Definitions[i]) {
			return r, organization.ErrState
		}
	}
	r = downloadReservationReview{BatchID: b.ID, Actor: actor, Reason: reason, State: saved.State, Jobs: saved.Jobs, Missing: saved.Missing, Plans: fresh.Plans, Omitted: fresh.Omitted, snapshot: saved.Digest}
	r.Fingerprint = organization.Digest(struct {
		Review   downloadReservationReview
		Snapshot string
	}{r, saved.Digest})
	return r, nil
}

func (api organizationAPI) recoverDownloadReservation(ctx context.Context, b organization.DownloadBatch, input downloadBatchRequest) error {
	if !input.Confirm || !input.ConfirmOldExecutorsStopped || len(input.Fingerprint) != 64 {
		return organization.ErrState
	}
	r, err := api.reviewDownloadReservation(ctx, b, input.actor, input.Reason)
	if err != nil {
		return err
	}
	if r.Fingerprint != input.Fingerprint || len(r.Omitted) > 0 && !input.ConfirmOmitted {
		return organization.ErrState
	}
	return api.jobs.CompleteDownloadReservation(ctx, b.ID, r.snapshot, input.actor, r.Reason, r.Fingerprint)
}
