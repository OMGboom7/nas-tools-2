package rsstaskconfig

import (
	"context"
	"errors"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/rssreceipt"
)

type TVIdentity = rssreceipt.TVIdentity

func (store *Store) HasPendingDownload(ctx context.Context, enclosure string) (bool, error) {
	var exists bool
	err := store.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND STATE='pending')`, rssreceipt.Key(enclosure)).Scan(&exists)
	return exists, err
}

// Call only when the client proves no submission was accepted, never on a
// timeout, malformed acknowledgement, or database failure after submission.
func (store *Store) ReleaseUnsubmittedDownload(ctx context.Context, taskID int64, enclosure string) error {
	_, err := store.database.ExecContext(ctx, `DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND abs(TASK_ID)=? AND STATE='pending'`, rssreceipt.Key(enclosure), taskID)
	return err
}

// Pending claims deliberately survive crashes and uncertain external results.
// They are not time-based leases: expiry could submit the same torrent twice.
func (store *Store) ReserveDownload(ctx context.Context, taskID int64, enclosure string) (bool, error) {
	if taskID <= 0 || enclosure == "" || len(enclosure) > 4096 {
		return false, errors.New("invalid RSS reservation")
	}
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	reserved, err := rssreceipt.Reserve(ctx, tx, taskID, enclosure)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return reserved, nil
}

// Complete all local effects atomically only after a confirmed submission.
// If local persistence fails, the pending claim still prevents automatic retry.
func (store *Store) CompleteReservedDownload(ctx context.Context, taskID int64, title, year, enclosure, downloader string, tv *TVIdentity, profiles ...string) error {
	return store.completeReservedDownload(ctx, taskID, title, year, enclosure, downloader, tv, true, profiles...)
}

func (store *Store) CompleteReservedManualDownload(ctx context.Context, taskID int64, title, year, enclosure, downloader string, profiles ...string) error {
	return store.completeReservedDownload(ctx, taskID, title, year, enclosure, downloader, nil, false, profiles...)
}

func (store *Store) completeReservedDownload(ctx context.Context, taskID int64, title, year, enclosure, downloader string, tv *TVIdentity, updateCounter bool, profiles ...string) error {
	if len(profiles) > 1 {
		return errors.New("invalid settings receipt")
	}
	if taskID <= 0 || enclosure == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(downloader) == "" {
		return errors.New("invalid RSS completion")
	}
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, fp := range profiles {
		if err := downloaderconfig.VerifyDownloadProfile(ctx, tx, fp); err != nil {
			return err
		}
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND TASK_ID=? AND STATE='pending')`, rssreceipt.Key(enclosure), taskID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("RSS reservation is unavailable")
	}
	if err := recordDownloadTx(ctx, tx, taskID, title, year, enclosure, downloader); err != nil {
		return err
	}
	if err := rssreceipt.Advance(ctx, tx, taskID, enclosure, tv, updateCounter); err != nil {
		return err
	}
	for _, fp := range profiles {
		if err := downloaderconfig.VerifyDownloadProfile(ctx, tx, fp); err != nil {
			return err
		}
	}
	return tx.Commit()
}
