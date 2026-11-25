package torrenttransfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// recordHistory runs in the same transaction as target binding. The marker
// outlives the legacy row: a later cleanup by TorrentRemover must not cause a
// retry to recreate a relationship that has already been consumed.
func recordHistory(ctx context.Context, tx *sql.Tx, receipt string, settings Settings, sourceID, targetID string) error {
	if settings.Ready() != nil || !Hex(sourceID, 40) || !Hex(targetID, 40) {
		return ErrState
	}
	from, err := strconv.ParseInt(settings.From[0], 10, 64)
	if err != nil || from <= 0 || strconv.FormatInt(from, 10) != settings.From[0] {
		return ErrState
	}
	to, err := strconv.ParseInt(settings.To[0], 10, 64)
	if err != nil || to <= 0 || strconv.FormatInt(to, 10) != settings.To[0] {
		return ErrState
	}
	key := settings.From[0] + "-" + sourceID
	var priorKey string
	err = tx.QueryRowContext(ctx, "SELECT history_key FROM GO_TORRENT_TRANSFER_HISTORY WHERE receipt=?", receipt).Scan(&priorKey)
	if err == nil {
		if priorKey != key {
			return ErrState
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS PLUGIN_HISTORY (ID INTEGER PRIMARY KEY, PLUGIN_ID TEXT, KEY TEXT, VALUE TEXT, DATE TEXT)`)
	if err != nil {
		return err
	}
	// Preserve pre-existing rows, including rows with unrecognized fields.
	// An old association alone cannot identify this transfer incarnation.
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=?)`, Plugin, key).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrState
	}
	value, err := historyValue(to, targetID, false)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE,DATE) VALUES(?,?,?,?)`, Plugin, key, value, time.Now().Format("2006-01-02 15:04:05"))
	if err != nil {
		return err
	}
	rowID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_TRANSFER_HISTORY(receipt,history_key,row_id) VALUES(?,?,?)`, receipt, key, rowID)
	return err
}

func historyValue(to int64, targetID string, deleted bool) (string, error) {
	raw, err := json.Marshal(struct {
		Downloader int64  `json:"to_download"`
		Task       string `json:"to_download_id"`
		Deleted    bool   `json:"delete_source"`
	}{Downloader: to, Task: targetID, Deleted: deleted})
	return string(raw), err
}
