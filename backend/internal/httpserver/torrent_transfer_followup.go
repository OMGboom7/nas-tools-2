package httpserver

import (
	"context"
	"database/sql"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type transferFollowup struct {
	Receipt string    `json:"receipt"`
	Since   time.Time `json:"since"`
}

// Each poll observes the original task and advances at most one remote action.
// Lost replies and missed check cycles remain available for operator review.
func (api torrentTransferAPI) followTransfer(ctx context.Context, db *sql.DB, l *torrenttransfer.Ledger, scope transferReviewScope, work transferFollowup, resolve torrenttransfer.ResolveScope) (string, error) {
	p, err := l.LoadPlan(ctx, work.Receipt)
	if err != nil {
		return "", err
	}
	if torrenttransfer.Hash(p) != work.Receipt || p.GrantDigest != scope.fingerprint {
		return "", torrenttransfer.ErrState
	}
	to, err := syncConfigID(scope.settings.To[0])
	if err != nil {
		return "", err
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, torrentLabelGuard(to, p.WireID))
	if err != nil {
		return "", err
	}
	defer guard.Close()
	status, err := torrenttransfer.ReadStatus(ctx, db, work.Receipt)
	if err != nil {
		return "", err
	}
	if status.SourceAbsentAt != "" {
		return "complete", nil
	}
	started := false
	for _, a := range status.Actions {
		if a.State != "acknowledged" {
			return "review", nil
		}
		if a.Action == "remove-source" {
			if err := l.ConfirmSourceAbsent(ctx, work.Receipt, resolve); err != nil {
				return "review", nil
			}
			return "complete", nil
		}
		if a.Action == "start" {
			started = true
		}
	}
	if status.TargetStage == "" {
		return "review", nil
	}
	if status.TargetStage == "verified" && (!scope.settings.AutoStart || started) {
		if started {
			task, err := scope.target.TransferTask(ctx, p.WireID)
			if err != nil || task.Error || !l.VerifiedInstance(ctx, work.Receipt, p.Receiver, task) {
				return "review", nil
			}
			if !task.SourceSeeding {
				if time.Since(work.Since) > 3*time.Minute {
					return "review", nil
				}
				return "waiting", nil
			}
		}
		if !scope.settings.DeleteSource {
			return "complete", nil
		}
		err := l.RemoveSource(ctx, work.Receipt, resolve, func(ctx context.Context, id string, source, target torrenttransfer.Task) error {
			if id != work.Receipt || !p.Source.Task.SameInstance(source) || target.WireID != p.WireID {
				return torrenttransfer.ErrState
			}
			return nil
		})
		if err != nil {
			return "review", nil
		}
		return "complete", nil
	}
	var start func(context.Context, string, torrenttransfer.Task) error
	if scope.settings.AutoStart && status.TargetStage == "verified" {
		start = func(_ context.Context, id string, t torrenttransfer.Task) error {
			if id != work.Receipt || t.WireID != p.WireID {
				return torrenttransfer.ErrState
			}
			return nil
		}
	}
	stage, err := l.ContinuePlan(ctx, work.Receipt, resolve, nil, start)
	if err != nil || stage == "unknown" || stage == "pending" {
		return "review", nil
	}
	if stage == "bound" && time.Since(work.Since) > 3*time.Minute {
		return "review", nil
	}
	if stage == "verified" && status.TargetStage == "verified" && start != nil {
		return "review", nil
	}
	return "waiting", nil
}
