package httpserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

// A pending row covers the gap between sending a request to the downloader
// and learning its ID. Only ready rows can create active brush history.
const brushReceiptSchema = `
CREATE TABLE IF NOT EXISTS GO_BRUSH_SUBMISSION (
 token TEXT PRIMARY KEY, enclosure TEXT NOT NULL UNIQUE, task_id INTEGER,
 downloader TEXT NOT NULL, title TEXT NOT NULL, size INTEGER NOT NULL,
 deadline TEXT NOT NULL, partial INTEGER NOT NULL, remote_id TEXT, ready INTEGER NOT NULL DEFAULT 0
);
CREATE TRIGGER IF NOT EXISTS go_brush_submission_task_deleted AFTER DELETE ON SITE_BRUSH_TASK
BEGIN UPDATE GO_BRUSH_SUBMISSION SET task_id=NULL WHERE task_id=OLD.ID; END;
CREATE TRIGGER IF NOT EXISTS go_brush_submission_task_retargeted AFTER UPDATE OF SITE,DOWNLOADER ON SITE_BRUSH_TASK
WHEN OLD.SITE IS NOT NEW.SITE OR OLD.DOWNLOADER IS NOT NEW.DOWNLOADER
BEGIN UPDATE GO_BRUSH_SUBMISSION SET task_id=NULL WHERE task_id=OLD.ID; END;`

type brushDownloadRequest struct {
	TaskID, SiteID                                                               int64
	Token, Enclosure, Downloader, Title, Deadline, RemoteID, RuleSource, FeedURL string
	Size                                                                         int64
	FractionSource                                                               *string
	Partial                                                                      bool
}

func BrushDownloadCommand(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	var q brushDownloadRequest
	if err := readMediaLookup(in, &q); err != nil {
		return err
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	db, err := (subscriptionService{databasePath: (config.Application{Path: path}).UserDatabasePath()}).openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, brushReceiptSchema); err != nil {
		return err
	}
	var result any
	switch op {
	case "reserve":
		result, err = reserveBrushDownload(ctx, db, q)
	case "accept", "selected":
		if q.Token == "" || strings.TrimSpace(q.RemoteID) == "" || q.RemoteID == "0" || q.Size < 0 {
			return errBrushDetail
		}
		if op == "accept" {
			err = acceptBrushDownload(ctx, db, q.Token, q.RemoteID)
		} else {
			err = fuzzyOne(db.ExecContext(ctx, `UPDATE GO_BRUSH_SUBMISSION SET size=?,ready=1 WHERE token=? AND partial=1 AND remote_id=? AND ready=0 AND task_id IS NOT NULL`, q.Size, q.Token, q.RemoteID))
		}
		result = err == nil
	case "finish":
		if q.Token == "" {
			return errBrushDetail
		}
		result, err = finishBrushDownload(ctx, db, q.Token)
	case "recover":
		if err = recoverBrushFractions(ctx, db, (config.Application{Path: path}).UserDatabasePath(), q.TaskID); err == nil {
			result, err = recoverBrushDownloads(ctx, db, q.TaskID)
		}
	default:
		return errors.New("unknown brush download operation")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func acceptBrushDownload(ctx context.Context, db *sql.DB, token, hash string) error {
	return fuzzyOne(db.ExecContext(ctx, `UPDATE GO_BRUSH_SUBMISSION SET remote_id=?,ready=1-partial WHERE token=? AND remote_id IS NULL AND task_id IS NOT NULL
				AND NOT EXISTS(SELECT 1 FROM SITE_BRUSH_TORRENTS h WHERE h.DOWNLOADER=GO_BRUSH_SUBMISSION.downloader AND h.DOWNLOAD_ID=?)
				AND NOT EXISTS(SELECT 1 FROM GO_BRUSH_SUBMISSION other WHERE other.downloader=GO_BRUSH_SUBMISSION.downloader AND other.remote_id=?)`, hash, token, hash, hash))
}

func reserveBrushDownload(ctx context.Context, db *sql.DB, q brushDownloadRequest) (string, error) {
	if q.TaskID <= 0 || q.SiteID <= 0 || q.FeedURL == "" || q.Title == "" || q.Enclosure == "" || q.Downloader == "" || q.Size < 0 {
		return "", errBrushDetail
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	token := rand.Text()
	// Recheck the executing task and its feed inside the transaction that wins
	// the enclosure. Old history, including removed torrents, stays authoritative.
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_BRUSH_SUBMISSION(token,enclosure,task_id,downloader,title,size,deadline,partial)
	SELECT ?,?,t.ID,t.DOWNLOADER,?,?,CASE WHEN t.BRUSHTASK_FREE_LIMIT_SPEED='Y' OR t.BRUSHTASK_FREE_DDL_DELETE='Y' THEN ? ELSE '' END,?
	FROM SITE_BRUSH_TASK t JOIN CONFIG_SITE s ON s.ID=t.SITE
	WHERE t.ID=? AND s.ID=? AND t.STATE='Y' AND instr(s.INCLUDE,'S')>0 AND t.DOWNLOADER=?
	AND (? IS NULL OR COALESCE(t.FRACTION_RULE,'')=?) AND COALESCE(t.RSS_RULE,'')=? AND COALESCE(NULLIF(t.RSSURL,''),s.RSSURL)=?
	AND NOT EXISTS(SELECT 1 FROM SITE_BRUSH_TORRENTS WHERE ENCLOSURE=?)
	ON CONFLICT(enclosure) DO NOTHING`, token, q.Enclosure, q.Title, q.Size, q.Deadline, q.Partial, q.TaskID, q.SiteID, q.Downloader, q.FractionSource, q.FractionSource, q.RuleSource, q.FeedURL, q.Enclosure)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return "", err
	}
	return token, tx.Commit()
}

func finishBrushDownload(ctx context.Context, db *sql.DB, token string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var q brushDownloadRequest
	err = tx.QueryRowContext(ctx, `SELECT task_id,title,size,enclosure,downloader,remote_id,deadline FROM GO_BRUSH_SUBMISSION
	WHERE token=? AND ready=1 AND task_id IS NOT NULL`, token).Scan(&q.TaskID, &q.Title, &q.Size, &q.Enclosure, &q.Downloader, &q.RemoteID, &q.Deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now().Format(time.DateTime)
	// An old worker may have written history during a rolling upgrade. Never
	// increment twice or attribute another task's row to this receipt.
	var existing int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SITE_BRUSH_TORRENTS WHERE ENCLOSURE=?`, q.Enclosure).Scan(&existing)
	if err != nil {
		return false, err
	}
	if existing == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO SITE_BRUSH_TORRENTS(TASK_ID,TORRENT_NAME,TORRENT_SIZE,ENCLOSURE,DOWNLOADER,DOWNLOAD_ID,FREE_DEADLINE,LST_MOD_DATE) VALUES(?,?,?,?,?,?,?,?)`, q.TaskID, q.Title, q.Size, q.Enclosure, q.Downloader, q.RemoteID, q.Deadline, now)
		if err != nil {
			return false, err
		}
		err = fuzzyOne(tx.ExecContext(ctx, `UPDATE SITE_BRUSH_TASK SET DOWNLOAD_COUNT=COALESCE(DOWNLOAD_COUNT,0)+1,LST_MOD_DATE=? WHERE ID=?`, now, q.TaskID))
		if err != nil {
			return false, err
		}
	} else {
		var same int
		err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM SITE_BRUSH_TORRENTS WHERE ENCLOSURE=? AND TASK_ID=? AND DOWNLOADER=? AND DOWNLOAD_ID=?`, q.Enclosure, q.TaskID, q.Downloader, q.RemoteID).Scan(&same)
		if err != nil {
			return false, err
		}
		if existing != 1 || same != 1 {
			return false, errors.New("conflicting brush download history")
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM GO_BRUSH_SUBMISSION WHERE token=?`, token); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func recoverBrushDownloads(ctx context.Context, db *sql.DB, taskID int64) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT token FROM GO_BRUSH_SUBMISSION WHERE ready=1 AND task_id IS NOT NULL AND (?=0 OR task_id=?) ORDER BY rowid`, taskID, taskID)
	if err != nil {
		return 0, err
	}
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			rows.Close()
			return 0, err
		}
		tokens = append(tokens, token)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var count int
	var firstErr error
	for _, token := range tokens {
		done, err := finishBrushDownload(ctx, db, token)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if done {
			count++
		}
	}
	return count, firstErr
}
