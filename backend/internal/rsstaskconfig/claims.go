package rsstaskconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

type TVIdentity struct {
	ID, Name string
	Season   int
}

func claimKey(enclosure string) string {
	key := sha256.Sum256([]byte(enclosure))
	return hex.EncodeToString(key[:])
}

func (store *Store) HasPendingDownload(ctx context.Context, enclosure string) (bool, error) {
	var exists bool
	err := store.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND STATE='pending')`, claimKey(enclosure)).Scan(&exists)
	return exists, err
}

// Call only when the client proves no submission was accepted, never on a
// timeout, malformed acknowledgement, or database failure after submission.
func (store *Store) ReleaseUnsubmittedDownload(ctx context.Context, taskID int64, enclosure string) error {
	_, err := store.database.ExecContext(ctx, `DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND TASK_ID=? AND STATE='pending'`, claimKey(enclosure), taskID)
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
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM CONFIG_USER_RSS WHERE ID=?)`, taskID).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrNotFound
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, enclosure).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_RSS_DOWNLOAD_CLAIMS (RESOURCE_KEY,TASK_ID,STATE,UPDATED_AT) VALUES (?,?,'pending',?) ON CONFLICT(RESOURCE_KEY) DO NOTHING`, claimKey(enclosure), taskID, time.Now().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return count == 1, nil
}

// Complete all local effects atomically only after a confirmed submission.
// If local persistence fails, the pending claim still prevents automatic retry.
func (store *Store) CompleteReservedDownload(ctx context.Context, taskID int64, title, year, enclosure, downloader string, tv *TVIdentity) error {
	return store.completeReservedDownload(ctx, taskID, title, year, enclosure, downloader, tv, true)
}

func (store *Store) CompleteReservedManualDownload(ctx context.Context, taskID int64, title, year, enclosure, downloader string) error {
	return store.completeReservedDownload(ctx, taskID, title, year, enclosure, downloader, nil, false)
}

func (store *Store) completeReservedDownload(ctx context.Context, taskID int64, title, year, enclosure, downloader string, tv *TVIdentity, updateCounter bool) error {
	if taskID <= 0 || enclosure == "" || strings.TrimSpace(title) == "" || strings.TrimSpace(downloader) == "" {
		return errors.New("invalid RSS completion")
	}
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND TASK_ID=? AND STATE='pending')`, claimKey(enclosure), taskID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errors.New("RSS reservation is unavailable")
	}
	if err := recordDownloadTx(ctx, tx, taskID, title, year, enclosure, downloader); err != nil {
		return err
	}
	if tv != nil {
		if _, err := strconv.ParseUint(tv.ID, 10, 64); err != nil || tv.ID == "0" || tv.Season < 0 || tv.Season > 1000 || tv.Name == "" || len(tv.Name) > 4096 {
			return errors.New("invalid RSS TV identity")
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MEDIAINFOS,'') FROM CONFIG_USER_RSS WHERE ID=?`, taskID).Scan(&raw); err != nil {
			return err
		}
		items := []map[string]any{}
		if len(raw) > 1<<20 || (raw != "" && (json.Unmarshal([]byte(raw), &items) != nil || items == nil)) {
			return errors.New("invalid RSS media identities")
		}
		found := false
		for _, item := range items {
			found = found || item["id"] == tv.ID && item["season"] == float64(tv.Season)
		}
		if !found {
			items = append(items, map[string]any{"id": tv.ID, "rssid": "", "season": tv.Season, "name": tv.Name})
		}
		encoded, err := json.Marshal(items)
		if err != nil || len(encoded) > 1<<20 {
			return errors.New("RSS media identities exceeded limit")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE CONFIG_USER_RSS SET MEDIAINFOS=? WHERE ID=?`, string(encoded), taskID); err != nil {
			return err
		}
	}
	if updateCounter {
		if _, err := tx.ExecContext(ctx, `UPDATE CONFIG_USER_RSS SET PROCESS_COUNT=MAX(CAST(COALESCE(PROCESS_COUNT,'0') AS INTEGER),0)+1, UPDATE_TIME=? WHERE ID=?`, time.Now().Format("2006-01-02 15:04:05"), taskID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_RSS_DOWNLOAD_CLAIMS SET STATE='submitted', UPDATED_AT=? WHERE RESOURCE_KEY=? AND TASK_ID=? AND STATE='pending'`, time.Now().Format(time.RFC3339), claimKey(enclosure), taskID); err != nil {
		return err
	}
	return tx.Commit()
}
