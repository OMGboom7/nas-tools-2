// Package rssreceipt shares the custom RSS transaction between download entry points.
package rssreceipt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

var ErrProcessed = errors.New("RSS resource already processed")
var ErrTaskMissing = errors.New("rss task not found")
var ErrClaim = errors.New("RSS resource does not have the expected reservation")

const Schema = `CREATE TABLE IF NOT EXISTS GO_RSS_DOWNLOAD_CLAIMS (RESOURCE_KEY TEXT PRIMARY KEY, TASK_ID INTEGER NOT NULL, STATE TEXT NOT NULL, UPDATED_AT TEXT NOT NULL)`

// SQLite can reuse a deleted task ID. Keep its unresolved enclosure reserved,
// but prevent a later acknowledgement from writing history to the replacement.
const DetachDeletedTask = `CREATE TRIGGER IF NOT EXISTS GO_RSS_DETACH_DELETED_TASK AFTER DELETE ON CONFIG_USER_RSS
 BEGIN UPDATE GO_RSS_DOWNLOAD_CLAIMS SET TASK_ID=-OLD.ID WHERE TASK_ID=OLD.ID AND STATE='pending'; END`

type Article struct {
	TaskID                             int64
	Torrent, Enclosure                 string
	Title, Year, Type, Season, Episode *string
	Scheduled                          bool
	TV                                 *TVIdentity
}
type TVIdentity struct {
	ID, Name string
	Season   int
}

func (tv TVIdentity) valid() bool {
	raw := tv.ID
	if prefix, n, ok := strings.Cut(raw, ":"); ok && (prefix == "DB" || prefix == "BG") {
		raw = n
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	return err == nil && id > 0 && tv.Season >= 0 && tv.Season <= 1000 && tv.Name != "" && len(tv.Name) <= 4096
}

func Key(enclosure string) string {
	key := sha256.Sum256([]byte(enclosure))
	return hex.EncodeToString(key[:])
}

func requireOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrClaim
	}
	return nil
}

func Reserve(ctx context.Context, tx *sql.Tx, taskID int64, enclosure string) (bool, error) {
	if taskID < 1 || enclosure == "" || len(enclosure) > 4096 {
		return false, ErrClaim
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM CONFIG_USER_RSS WHERE ID=?)`, taskID).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, ErrTaskMissing
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, enclosure).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_RSS_DOWNLOAD_CLAIMS (RESOURCE_KEY,TASK_ID,STATE,UPDATED_AT) VALUES (?,?,'pending',?) ON CONFLICT(RESOURCE_KEY) DO NOTHING`, Key(enclosure), taskID, time.Now().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

func (item Article) Record(ctx context.Context, transaction *sql.Tx, downloaderName string) error {
	taskID, title, enclosure := item.TaskID, item.Torrent, item.Enclosure
	var exists int
	var err error
	if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM CONFIG_USER_RSS WHERE ID=?)`, taskID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrTaskMissing
	}
	if enclosure != "" {
		err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, enclosure).Scan(&exists)
	} else {
		err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE TORRENT_NAME=?)`, title).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if exists == 0 {
		if err := requireOne(transaction.ExecContext(ctx, `INSERT INTO RSS_TORRENTS (TORRENT_NAME,ENCLOSURE,TITLE,YEAR,TYPE,SEASON,EPISODE) VALUES (?,?,?,?,?,?,?)`, title, enclosure, item.Title, item.Year, item.Type, item.Season, item.Episode)); err != nil {
			return err
		}
	}
	if err := requireOne(transaction.ExecContext(ctx, `INSERT INTO USERRSS_TASK_HISTORY (TASK_ID,TITLE,DOWNLOADER,DATE) VALUES (?,?,?,?)`, strconv.FormatInt(taskID, 10), title, downloaderName, time.Now().Format("2006-01-02 15:04:05"))); err != nil {
		return err
	}
	return nil
}

func Advance(ctx context.Context, tx *sql.Tx, taskID int64, enclosure string, tv *TVIdentity, updateCounter bool) error {
	if tv != nil {
		if !tv.valid() {
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
		if err := requireOne(tx.ExecContext(ctx, `UPDATE CONFIG_USER_RSS SET MEDIAINFOS=? WHERE ID=?`, string(encoded), taskID)); err != nil {
			return err
		}
	}
	if updateCounter {
		if err := requireOne(tx.ExecContext(ctx, `UPDATE CONFIG_USER_RSS SET PROCESS_COUNT=MAX(CAST(COALESCE(PROCESS_COUNT,'0') AS INTEGER),0)+1, UPDATE_TIME=? WHERE ID=?`, time.Now().Format("2006-01-02 15:04:05"), taskID)); err != nil {
			return err
		}
	}
	return requireOne(tx.ExecContext(ctx, `UPDATE GO_RSS_DOWNLOAD_CLAIMS SET STATE='submitted', UPDATED_AT=? WHERE RESOURCE_KEY=? AND TASK_ID=? AND STATE='pending'`, time.Now().Format(time.RFC3339), Key(enclosure), taskID))
}

// ReserveClassic joins the enclosure reservation to the downloader's pending receipt.
func (item Article) ReserveClassic(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, Schema); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, DetachDeletedTask); err != nil {
		return err
	}
	if item.TV != nil && (!item.Scheduled || !item.TV.valid()) {
		return errors.New("invalid RSS TV identity")
	}
	if item.Scheduled {
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(USES,'') FROM CONFIG_USER_RSS WHERE ID=?", item.TaskID).Scan(&kind); err != nil {
			return err
		}
		if kind != "D" {
			return ErrTaskMissing
		}
	}
	ok, err := Reserve(ctx, tx, item.TaskID, item.Enclosure)
	if err != nil || ok {
		return err
	}
	var seen, pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?), EXISTS(SELECT 1 FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND STATE='pending')`, item.Enclosure, Key(item.Enclosure)).Scan(&seen, &pending); err != nil {
		return err
	}
	if seen && !pending {
		return ErrProcessed
	}
	return ErrClaim
}
