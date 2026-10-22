package organization

import (
	"context"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

// ReadCompletedRemoteMedia lends one content-bound capability, never a cloud
// URL, account or local-looking logical path. No disc STREAM clip is guessed.
// The durable scrape claim prevents replay; the current approval/configuration
// is checked throughout preparation/decoding and the complete target afterwards.
func ReadCompletedRemoteMedia(ctx context.Context, job Job, item JobItem, current func(context.Context) error, consume func(context.Context, *remotestore.VerifiedObject) ([]byte, error)) ([]byte, error) {
	if !RemoteMode(job.Mode) || job.Mode != job.Definition.Mode || job.State != "completed" || item.State != "completed" || item.Kind != "media" || item.Reason != "" || consume == nil || current == nil {
		return nil, ErrState
	}
	if IsDisc(item.Entry) {
		return nil, ErrMode
	}
	if item.Proof.Incomplete || item.Proof.Remote == nil || !item.Proof.Remote.StageReady || !item.Proof.Remote.PublicationIntent || !item.Proof.Remote.TargetVerified || remoteProofValid(job.Definition, item.Entry, item.Proof) != nil {
		return nil, ErrState
	}
	call, cancel := context.WithCancel(ctx)
	defer cancel()
	check := func(ctx context.Context) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := current(ctx); err != nil {
			return err
		}
		_, err := remoteClient(ctx, job.Definition)
		return err
	}
	if err := check(call); err != nil {
		return nil, err
	}
	// Revocation also interrupts a stalled full read or decoder, not just the
	// next range request. Join this watcher before releasing the remote guard.
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-call.Done():
				return
			case <-ticker.C:
				if check(call) != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	c, err := remoteClient(call, job.Definition)
	if err != nil {
		return nil, err
	}
	object, err := c.Pin(call, job.Definition.TargetRoot, item.Target, item.Size, item.Proof.Digest, check)
	if err != nil {
		return nil, err
	}
	defer object.Close()
	data, err := consume(call, object)
	if err != nil {
		return nil, err
	}
	if err := check(call); err != nil {
		return nil, err
	}
	if err := object.Current(call); err != nil {
		return nil, err
	}
	if err := VerifyCompletedTarget(call, job, item); err != nil {
		return nil, err
	}
	if err := check(call); err != nil {
		return nil, err
	}
	return data, nil
}
