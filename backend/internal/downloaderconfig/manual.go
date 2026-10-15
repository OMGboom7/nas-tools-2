package downloaderconfig

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

var ErrManualPending = errors.New("manual submission is pending or conflicts with its original scope")

type ManualDefinition struct {
	DownloaderID                                                int64
	DownloaderName, DownloaderType, ConfigDigest, OptionsDigest string
	InfoHash, Actor, Title, Source, SavePath                    string
}
type ManualSubmission struct {
	Definition ManualDefinition `json:"-"`
	ID         string           `json:"id"`
	State      string           `json:"state"`
	TaskID     string           `json:"taskId,omitempty"`
	Reason     string           `json:"reason,omitempty"`
	Created    string           `json:"created"`
	Updated    string           `json:"updated"`
}

func manualSchema(ctx context.Context, tx *sql.Tx) error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS GO_MANUAL_DOWNLOAD_SUBMISSIONS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,STATE TEXT NOT NULL,TASK_ID TEXT NOT NULL DEFAULT '',REASON TEXT NOT NULL DEFAULT '',CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_MANUAL_DOWNLOAD_SCOPES(SCOPE TEXT PRIMARY KEY,SUBMISSION_ID TEXT NOT NULL UNIQUE)`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}
func manualScope(d ManualDefinition) string { return bus.Hash([]any{d.DownloaderID, d.InfoHash}) }
func manualDownloader(ctx context.Context, tx *sql.Tx, id int64) (Downloader, error) {
	return scanDownloader(tx.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(ENABLED,0),COALESCE(TYPE,''),COALESCE(TRANSFER,0),COALESCE(ONLY_NASTOOL,0),COALESCE(MATCH_PATH,0),COALESCE(RMT_MODE,''),COALESCE(CONFIG,'{}'),COALESCE(DOWNLOAD_DIR,'[]') FROM DOWNLOADER WHERE ID=?`, id))
}
func verifyManualConfig(ctx context.Context, tx *sql.Tx, d ManualDefinition) error {
	c, err := manualDownloader(ctx, tx, d.DownloaderID)
	if err != nil {
		return err
	}
	if c.Enabled == 0 || c.Type != d.DownloaderType || bus.Hash(c) != d.ConfigDigest {
		return ErrManualPending
	}
	return nil
}

const manualSelect = `SELECT ID,DOCUMENT,STATE,TASK_ID,REASON,CREATED,UPDATED FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS`

func scanManual(row interface{ Scan(...any) error }) (ManualSubmission, error) {
	var r ManualSubmission
	var raw string
	err := row.Scan(&r.ID, &raw, &r.State, &r.TaskID, &r.Reason, &r.Created, &r.Updated)
	if err != nil {
		return r, err
	}
	if json.Unmarshal([]byte(raw), &r.Definition) != nil {
		return r, ErrManualPending
	}
	return r, nil
}

// ReserveManual returns created=false only for an identical acknowledged scope.
// Pending records never expire; source URL/token/torrent bytes are not stored.
func (s *Store) ReserveManual(ctx context.Context, d ManualDefinition) (ManualSubmission, bool, error) {
	if d.DownloaderID < 1 || d.Actor == "" || d.Title == "" || len(d.InfoHash) != 40 && len(d.InfoHash) != 64 || len(d.ConfigDigest) != 64 || len(d.OptionsDigest) != 64 {
		return ManualSubmission{}, false, ErrManualPending
	}
	if _, err := hex.DecodeString(d.InfoHash); err != nil || d.InfoHash != strings.ToLower(d.InfoHash) {
		return ManualSubmission{}, false, ErrManualPending
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return ManualSubmission{}, false, err
	}
	defer tx.Rollback()
	if err := verifyManualConfig(ctx, tx, d); err != nil {
		return ManualSubmission{}, false, err
	}
	if err := manualSchema(ctx, tx); err != nil {
		return ManualSubmission{}, false, err
	}
	var pending bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS WHERE STATE='pending' AND json_extract(DOCUMENT,'$.InfoHash')=?)`, d.InfoHash).Scan(&pending); err != nil {
		return ManualSubmission{}, false, err
	}
	if pending {
		return ManualSubmission{}, false, ErrManualPending
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT SUBMISSION_ID FROM GO_MANUAL_DOWNLOAD_SCOPES WHERE SCOPE=?`, manualScope(d)).Scan(&existing)
	if err == nil {
		r, err := scanManual(tx.QueryRowContext(ctx, manualSelect+` WHERE ID=?`, existing))
		if err != nil {
			return r, false, err
		}
		if r.State != "submitted" || r.Definition.Actor != d.Actor || r.Definition.ConfigDigest != d.ConfigDigest || r.Definition.OptionsDigest != d.OptionsDigest {
			return r, false, ErrManualPending
		}
		return r, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ManualSubmission{}, false, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ManualSubmission{}, false, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := ManualSubmission{ID: hex.EncodeToString(random[:]), Definition: d, State: "pending", Created: now, Updated: now}
	raw, err := json.Marshal(d)
	if err != nil {
		return r, false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_MANUAL_DOWNLOAD_SUBMISSIONS(ID,DOCUMENT,STATE,CREATED,UPDATED) VALUES(?,?,'pending',?,?)`, r.ID, string(raw), now, now)
	if err != nil {
		return r, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return r, false, ErrManualPending
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO GO_MANUAL_DOWNLOAD_SCOPES VALUES(?,?)`, manualScope(d), r.ID)
	if err != nil {
		return r, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return r, false, ErrManualPending
	}
	if err := tx.Commit(); err != nil {
		return r, false, err
	}
	return r, true, nil
}
func (s *Store) RejectManual(ctx context.Context, r ManualSubmission) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE GO_MANUAL_DOWNLOAD_SUBMISSIONS SET STATE='rejected',REASON='rejected before submission',UPDATED=? WHERE ID=? AND STATE='pending' AND DOCUMENT=?`, time.Now().UTC().Format(time.RFC3339Nano), r.ID, manualDocument(r))
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM GO_MANUAL_DOWNLOAD_SCOPES WHERE SCOPE=? AND SUBMISSION_ID=?`, manualScope(r.Definition), r.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	return tx.Commit()
}
func manualDocument(r ManualSubmission) string { b, _ := json.Marshal(r.Definition); return string(b) }
func (s *Store) CompleteManual(ctx context.Context, r ManualSubmission, taskID, proof, actor string) error {
	if proof != "acknowledged" && proof != "verified_present" || taskID == "" || len(taskID) > 128 {
		return ErrManualPending
	}
	if actor == "" {
		return ErrManualPending
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanManual(tx.QueryRowContext(ctx, manualSelect+` WHERE ID=?`, r.ID))
	if err != nil {
		return err
	}
	if current.State != "pending" || manualDocument(current) != manualDocument(r) {
		return ErrManualPending
	}
	d := r.Definition
	if err := verifyManualConfig(ctx, tx, d); err != nil {
		return err
	}
	var owns bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_MANUAL_DOWNLOAD_SCOPES WHERE SCOPE=? AND SUBMISSION_ID=?)`, manualScope(d), r.ID).Scan(&owns); err != nil {
		return err
	}
	if !owns {
		return ErrManualPending
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS DOWNLOAD_HISTORY(ID INTEGER PRIMARY KEY,TITLE TEXT,YEAR TEXT,TYPE TEXT,TMDBID TEXT,SE TEXT,VOTE TEXT,POSTER TEXT,OVERVIEW TEXT,TORRENT TEXT,ENCLOSURE TEXT,SITE TEXT,DESC TEXT,DOWNLOADER TEXT,DOWNLOAD_ID TEXT,SAVE_PATH TEXT,DATE TEXT)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO DOWNLOAD_HISTORY(TITLE,TORRENT,DOWNLOADER,DOWNLOAD_ID,SAVE_PATH,DATE) VALUES(?,?,?,?,?,?)`, d.Title, d.Title, strconv.FormatInt(d.DownloaderID, 10), taskID, d.SavePath, time.Now().Format("2006-01-02 15:04:05")); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_MANUAL_DOWNLOAD_SUBMISSIONS SET STATE='submitted',TASK_ID=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='pending'`, taskID, proof, time.Now().UTC().Format(time.RFC3339Nano), r.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrManualPending
	}
	title := d.Title + " 已提交下载"
	message := "任务已得到接收应答；不代表文件下载完成或入库。"
	if proof == "verified_present" {
		title = d.Title + " 已核实任务存在"
		message = "管理员只读核实原 Hash 当前存在；不证明本次新建任务或全部设置已生效，不代表文件完成或入库。"
	}
	if err := bus.Emit(ctx, tx, "download_start", r.ID, "manual-download:"+r.ID, title, "来自："+d.Source+"；下载器："+d.DownloaderName+"。"+message); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_MANUAL_DOWNLOAD_CONFIRMATIONS(SUBMISSION_ID TEXT PRIMARY KEY,ACTOR TEXT NOT NULL,PROOF TEXT NOT NULL,CREATED TEXT NOT NULL)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_MANUAL_DOWNLOAD_CONFIRMATIONS VALUES(?,?,?,?)`, r.ID, actor, proof, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if err := verifyManualConfig(ctx, tx, d); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ManualSubmissions(ctx context.Context) ([]ManualSubmission, error) {
	return s.ManualSubmissionsPage(ctx, 1)
}
func (s *Store) ManualSubmissionsPage(ctx context.Context, page int) ([]ManualSubmission, error) {
	if page < 1 || page > 10000 {
		return nil, ErrManualPending
	}
	result := []ManualSubmission{}
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_MANUAL_DOWNLOAD_SUBMISSIONS')`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	rows, err := s.database.QueryContext(ctx, manualSelect+` ORDER BY CASE WHEN STATE='pending' THEN 0 ELSE 1 END,CREATED DESC,ID LIMIT 100 OFFSET ?`, (page-1)*100)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanManual(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) ManualSubmission(ctx context.Context, id string) (ManualSubmission, error) {
	if len(id) != 32 || strings.TrimSpace(id) != id {
		return ManualSubmission{}, ErrManualPending
	}
	r, err := scanManual(s.database.QueryRowContext(ctx, manualSelect+` WHERE ID=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}
