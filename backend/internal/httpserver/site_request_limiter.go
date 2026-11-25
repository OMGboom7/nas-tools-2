package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type siteRequestLimiter struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

func (limiter *siteRequestLimiter) try(ctx context.Context, store *siteconfig.Store, id int64) (time.Duration, error) {
	if store == nil {
		return 0, errors.New("site request configuration is unavailable")
	}
	clock := time.Now
	if limiter.now != nil {
		clock = limiter.now
	}
	return store.AdmitRequest(ctx, id, clock)
}

var errSiteRequestBusy = errors.New("site request limit reached")

// One batch per site. Its first exchange skips a busy site. Subsequent
// exchanges wait their turn, including parallel browser resource requests.
type siteBatchLimiter struct {
	limiter  siteRequestLimiter
	once     sync.Once
	firstErr error
}

func (batch *siteBatchLimiter) Wait(ctx context.Context, store *siteconfig.Store, id int64) error {
	first := false
	batch.once.Do(func() {
		first = true
		delay, err := batch.limiter.try(ctx, store, id)
		batch.firstErr = err
		if err == nil && delay > 0 {
			batch.firstErr = errSiteRequestBusy
		}
	})
	if first || batch.firstErr != nil {
		return batch.firstErr
	}
	return batch.limiter.Wait(ctx, store, id)
}

func SiteAdmissionCommand(ctx context.Context, path, rawID string, out io.Writer) error {
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id <= 0 {
		return errors.New("invalid site id")
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	db := (config.Application{Path: path}).UserDatabasePath()
	if info, err := os.Stat(db); err != nil || !info.Mode().IsRegular() {
		return errors.New("site database unavailable")
	}
	store, err := siteconfig.Open(db)
	if err != nil {
		return err
	}
	defer store.Close()
	delay, err := store.AdmitRequest(ctx, id, time.Now)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(delay == 0)
}

func waitSiteRequestDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (limiter *siteRequestLimiter) Wait(ctx context.Context, store *siteconfig.Store, id int64) error {
	if limiter == nil {
		return nil
	}
	if store == nil {
		return errors.New("site request configuration is unavailable")
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay, err := limiter.try(ctx, store, id)
		if err != nil || delay == 0 {
			return err
		}
		// Re-read saved settings at most one second after an edit. Never sleep
		// holding the lock or pre-reserve work which may be cancelled.
		if delay > time.Second {
			delay = time.Second
		}
		wait := waitSiteRequestDelay
		if limiter.wait != nil {
			wait = limiter.wait
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}
