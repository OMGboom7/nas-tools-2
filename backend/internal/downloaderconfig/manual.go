package downloaderconfig

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/downloadhistory"
	"github.com/0xforee/nas-tools/backend/internal/rssreceipt"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

var ErrManualPending = errors.New("manual submission is pending or conflicts with its original scope")

type ManualDefinition struct {
	SubscriptionKey                                             string       `json:"subscriptionKey,omitempty"`
	RSSTask                                                     int64        `json:"rssTask,omitempty"`
	RSSKey                                                      string       `json:"rssKey,omitempty"`
	Direct                                                      *DirectMedia `json:"direct,omitempty"`
	ProfileDigest                                               string       `json:"profileDigest,omitempty"`
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
		`CREATE TABLE IF NOT EXISTS GO_CLASSIC_PENDING_HISTORY(SUBMISSION_ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL)`,
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
	return s.reserveManual(ctx, d, nil, nil)
}

func (s *Store) ReserveClassic(ctx context.Context, d ManualDefinition, classic ClassicAcknowledgement) (ManualSubmission, bool, error) {
	return s.reserveManual(ctx, d, nil, &classic)
}

func (s *Store) ReserveDirectFiles(ctx context.Context, d ManualDefinition, selection FileSelectionDefinition) (ManualSubmission, bool, error) {
	if d.Direct == nil || selection.Version != 3 || d.ProfileDigest != selection.ProfileFingerprint() || d.InfoHash != selection.InfoHash || d.ConfigDigest != selection.ConfigDigest || d.DownloaderID != selection.DownloaderID {
		return ManualSubmission{}, false, ErrManualPending
	}
	return s.reserveManual(ctx, d, &selection, nil)
}
func (s *Store) reserveManual(ctx context.Context, d ManualDefinition, selection *FileSelectionDefinition, classic *ClassicAcknowledgement) (ManualSubmission, bool, error) {
	if d.SubscriptionKey != "" {
		return ManualSubmission{}, false, ErrManualPending
	} // derived only from the original private receipt
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
	if classic != nil && classic.RSS != nil {
		item := classic.RSS
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_CUSTOM_RSS_ACKS(SUBMISSION_ID TEXT PRIMARY KEY, TASK TEXT NOT NULL)`); err != nil {
			return ManualSubmission{}, false, err
		}
		if err := item.ReserveClassic(ctx, tx); err != nil {
			return ManualSubmission{}, false, err
		}
		d.RSSTask, d.RSSKey = item.TaskID, rssreceipt.Key(item.Enclosure)
	}
	if classic != nil && classic.Subscription != nil {
		d.SubscriptionKey = rssreceipt.Key(classic.Subscription.Marker.Enclosure)
	}
	if m := d.Direct; m != nil {
		if !m.valid() {
			return ManualSubmission{}, false, ErrManualPending
		}
		if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS GO_MANUAL_DIRECT_MEDIA ON GO_MANUAL_DOWNLOAD_SUBMISSIONS(json_extract(DOCUMENT,'$.direct.TMDBID'),json_extract(DOCUMENT,'$.direct.Kind'),json_extract(DOCUMENT,'$.direct.Season')) WHERE json_extract(DOCUMENT,'$.direct.TMDBID') IS NOT NULL`); err != nil {
			return ManualSubmission{}, false, err
		}
		bySeason := map[int][]int{m.Season: m.Units}
		maps.Copy(bySeason, m.ExtraSeasons)
		for season, units := range bySeason {
			covered, err := readDirectCoverage(ctx, tx, m.Kind, m.TMDBID, season)
			if err != nil {
				return ManualSubmission{}, false, err
			}
			for _, n := range units {
				if covered[n] {
					return ManualSubmission{}, false, ErrManualPending
				}
			}
		}
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
		if d.SubscriptionKey != "" || d.RSSKey != "" || d.Direct != nil || r.State != "submitted" || r.Definition.Actor != d.Actor || r.Definition.ConfigDigest != d.ConfigDigest || r.Definition.OptionsDigest != d.OptionsDigest {
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
	if d.SubscriptionKey != "" {
		if err := reserveOrdinaryRSS(ctx, tx, r.ID, classic.Subscription); err != nil {
			return r, false, err
		}
	}
	if classic != nil {
		raw, err := json.Marshal(classic)
		if err != nil {
			return r, false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_CLASSIC_PENDING_HISTORY(SUBMISSION_ID,DOCUMENT) VALUES(?,?)`, r.ID, string(raw)); err != nil {
			return r, false, err
		}
	}
	if selection != nil {
		if err := ReserveFileSelectionTx(ctx, tx, *selection); err != nil {
			return r, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return r, false, err
	}
	return r, true, nil
}
func (s *Store) RejectManual(ctx context.Context, r ManualSubmission, attempted bool) error {
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
	if r.Definition.SubscriptionKey != "" {
		if err := releaseOrdinaryRSS(ctx, tx, r); err != nil {
			return err
		}
	}
	if d := r.Definition; d.RSSKey != "" {
		res, err := tx.ExecContext(ctx, `DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND abs(TASK_ID)=? AND STATE='pending' AND NOT EXISTS(SELECT 1 FROM GO_CUSTOM_RSS_ACKS WHERE SUBMISSION_ID=?)`, d.RSSKey, d.RSSTask, r.ID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return ErrManualPending
		}
	}
	if strings.HasPrefix(r.Definition.Actor, "classic:") {
		if _, err := tx.ExecContext(ctx, `DELETE FROM GO_CLASSIC_PENDING_HISTORY WHERE SUBMISSION_ID=?`, r.ID); err != nil {
			return err
		}
	}
	if m := r.Definition.Direct; attempted && m != nil {
		reason := "下载器未接收这次提交，请核对下载设置和连接状态。"
		data := map[string]any{"media_info": map[string]any{"title": m.Name, "year": m.Year, "tmdb_id": m.TMDBID}, "reason": reason}
		if err := bus.EmitData(ctx, tx, "download_fail", r.ID, "manual-download:"+r.ID, "添加下载任务失败："+m.Name, reason, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func manualDocument(r ManualSubmission) string { b, _ := json.Marshal(r.Definition); return string(b) }
func (s *Store) CompleteManual(ctx context.Context, r ManualSubmission, taskID, proof, actor string) error {
	return s.CompleteWithHistory(ctx, r, taskID, proof, actor, nil)
}

// ClassicAcknowledgement contains private history fields for the original table.
// They never enter the public receipt or the notification data.
type ClassicAcknowledgement struct {
	Subscription             *OrdinaryRSS        `json:"subscription,omitempty"`
	RSS                      *rssreceipt.Article `json:"rss,omitempty"`
	Files                    []torrentfiles.File `json:"files,omitempty"`
	Paused                   bool                `json:"paused,omitempty"`
	Directory                string              `json:"directory,omitempty"`
	V1Hash, V2Hash           string
	Media                    downloadhistory.Media
	Path                     *string
	Title, Text, Image, Link string
}

// Pending classic subsets can be addressed by either the canonical hash or
// qBittorrent's shortened v2 task ID. Other starts must wait for verification.
func (s *Store) PendingClassicFiles(ctx context.Context, downloader int64, hash string) (*ClassicAcknowledgement, error) {
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_CLASSIC_PENDING_HISTORY')`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	var raw string
	err := s.database.QueryRowContext(ctx, `SELECT h.DOCUMENT FROM GO_CLASSIC_PENDING_HISTORY h JOIN GO_MANUAL_DOWNLOAD_SUBMISSIONS m ON m.ID=h.SUBMISSION_ID WHERE m.STATE='pending' AND json_extract(m.DOCUMENT,'$.DownloaderID')=? AND (json_extract(m.DOCUMENT,'$.InfoHash')=? OR substr(json_extract(h.DOCUMENT,'$.V2Hash'),1,40)=?) AND json_array_length(h.DOCUMENT,'$.files')>0 LIMIT 1`, downloader, hash, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h ClassicAcknowledgement
	err = json.Unmarshal([]byte(raw), &h)
	return &h, err
}

func (s *Store) ClassicPendingHistory(ctx context.Context, id string) (ClassicAcknowledgement, error) {
	var history ClassicAcknowledgement
	var raw string
	if err := s.database.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_CLASSIC_PENDING_HISTORY WHERE SUBMISSION_ID=?`, id).Scan(&raw); err != nil {
		return history, err
	}
	err := json.Unmarshal([]byte(raw), &history)
	return history, err
}

func (s *Store) CompleteWithHistory(ctx context.Context, r ManualSubmission, taskID, proof, actor string, classic *ClassicAcknowledgement) error {
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
	if current.State == "submitted" && (r.Definition.RSSKey != "" || r.Definition.SubscriptionKey != "") && current.TaskID == taskID && manualDocument(current) == manualDocument(r) {
		return nil // A concurrent recovery committed this exact acknowledgement.
	}
	if current.State != "pending" || manualDocument(current) != manualDocument(r) {
		return ErrManualPending
	}
	d := r.Definition
	if d.SubscriptionKey != "" {
		if classic == nil || classic.Subscription == nil || rssreceipt.Key(classic.Subscription.Marker.Enclosure) != d.SubscriptionKey {
			return ErrManualPending
		}
		raw, err := json.Marshal(classic.Subscription)
		if err != nil {
			return err
		}
		var matches bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_ORDINARY_RSS_RECEIPTS WHERE ID=? AND RESOURCE_KEY=? AND DOCUMENT=? AND (ACK=? OR (ACK='' AND ?='verified_present')))`, r.ID, d.SubscriptionKey, string(raw), taskID, proof).Scan(&matches)
		if err != nil {
			return err
		}
		if !matches {
			return ErrManualPending
		}
	}
	if d.RSSKey != "" {
		if classic == nil || classic.RSS == nil || classic.RSS.TaskID != d.RSSTask || rssreceipt.Key(classic.RSS.Enclosure) != d.RSSKey {
			return ErrManualPending
		}
		item := classic.RSS
		var saved string
		raw, err := json.Marshal(classic)
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT DOCUMENT FROM GO_CLASSIC_PENDING_HISTORY WHERE SUBMISSION_ID=?", r.ID).Scan(&saved); err != nil || saved != string(raw) {
			return ErrManualPending
		}
		if err := item.Record(ctx, tx, d.DownloaderName); err != nil {
			return err
		}
		if err := rssreceipt.Advance(ctx, tx, item.TaskID, item.Enclosure, item.TV, item.Scheduled); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM GO_CUSTOM_RSS_ACKS WHERE SUBMISSION_ID=?", r.ID); err != nil {
			return err
		}
	}
	if classic != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM GO_CLASSIC_PENDING_HISTORY WHERE SUBMISSION_ID=?`, r.ID); err != nil {
			return err
		}
	}
	if err := VerifyDownloadProfile(ctx, tx, d.ProfileDigest); err != nil {
		return err
	}
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
	date := time.Now().Format("2006-01-02 15:04:05")
	mediaInfo := map[string]any{"title": d.Title}
	if classic != nil && classic.Media != nil {
		if err := downloadhistory.RecordTx(ctx, tx, classic.Media, strconv.FormatInt(d.DownloaderID, 10), taskID, classic.Path); err != nil {
			return err
		}
		mediaInfo = map[string]any{"title": classic.Media["TITLE"], "year": classic.Media["YEAR"], "type": classic.Media["TYPE"], "tmdb_id": classic.Media["TMDBID"], "season_episode": classic.Media["SE"]}
	} else if m := d.Direct; m != nil {
		if !m.valid() {
			return ErrManualPending
		}
		mediaType, se := "电影", ""
		if m.Kind == "tv" {
			mediaType = "电视剧"
			bySeason := map[int][]int{m.Season: m.Units}
			maps.Copy(bySeason, m.ExtraSeasons)
			var label strings.Builder
			for _, season := range slices.Sorted(maps.Keys(bySeason)) {
				fmt.Fprintf(&label, "S%02d", season)
				for _, n := range bySeason[season] {
					fmt.Fprintf(&label, "E%02d", n)
				}
			}
			se = label.String()
		}
		mediaInfo = map[string]any{"title": m.Name, "year": m.Year, "type": mediaType, "tmdb_id": m.TMDBID, "season_episode": se}
		result, err := tx.ExecContext(ctx, `INSERT INTO DOWNLOAD_HISTORY(TITLE,YEAR,TYPE,TMDBID,SE,TORRENT,DOWNLOADER,DOWNLOAD_ID,SAVE_PATH,DATE) VALUES(?,?,?,?,?,?,?,?,?,?)`, m.Name, m.Year, mediaType, m.TMDBID, se, d.Title, strconv.FormatInt(d.DownloaderID, 10), taskID, d.SavePath, date)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrManualPending
		}
		if id, ok := strings.CutPrefix(d.ProfileDigest, "files:"); ok {
			p, err := FileSelectionTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if err = CommitFileSelectionTx(ctx, tx, id, p.Definition.Proof); err != nil {
				return err
			}
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO DOWNLOAD_HISTORY(TITLE,TORRENT,DOWNLOADER,DOWNLOAD_ID,SAVE_PATH,DATE) VALUES(?,?,?,?,?,?)`, d.Title, d.Title, strconv.FormatInt(d.DownloaderID, 10), taskID, d.SavePath, date); err != nil {
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
	reference := "manual-download:" + r.ID
	event := map[string]any{"media_info": mediaInfo, "downloader_id": d.DownloaderID, "download_dir": d.SavePath,
		"native_download": map[string]any{"task_id": taskID, "info_hash": d.InfoHash, "proof": proof, "source": d.Source}}
	body := "来自：" + d.Source + "；下载器：" + d.DownloaderName + "。" + message
	if classic != nil {
		reference = "classic-download:" + r.ID
		title, body = classic.Title, classic.Text
		event["classic_notice"] = map[string]string{"image": classic.Image, "link": classic.Link}
	}
	if err := bus.EmitData(ctx, tx, "download_start", r.ID, reference, title, body, event); err != nil {
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
	if err := VerifyDownloadProfile(ctx, tx, d.ProfileDigest); err != nil {
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

// Reset only the active scope of the completed RSS receipt. Its immutable
// submission and confirmation remain available after an explicit unmark.
func ResetCompletedRSSScope(ctx context.Context, tx *sql.Tx, key string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_MANUAL_DOWNLOAD_SCOPES')`).Scan(&exists); err != nil || !exists {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM GO_MANUAL_DOWNLOAD_SCOPES WHERE SUBMISSION_ID IN (
 SELECT m.ID FROM GO_MANUAL_DOWNLOAD_SUBMISSIONS m JOIN GO_RSS_DOWNLOAD_CLAIMS r
 ON json_extract(m.DOCUMENT,'$.rssKey')=r.RESOURCE_KEY AND json_extract(m.DOCUMENT,'$.rssTask')=r.TASK_ID
 WHERE m.STATE='submitted' AND r.STATE='submitted' AND r.RESOURCE_KEY=?)`, key)
	return err
}
