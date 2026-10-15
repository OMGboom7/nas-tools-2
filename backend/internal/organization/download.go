package organization

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
)

type DownloadOrigin struct {
	MonitorGeneration string                  `json:"monitorGeneration,omitempty"`
	MonitorEvent      string                  `json:"monitorEvent,omitempty"`
	BatchID           string                  `json:"batchId"`
	DownloaderID      string                  `json:"downloaderId"`
	DownloaderName    string                  `json:"downloaderName"`
	DownloaderType    string                  `json:"downloaderType"`
	ConfigDigest      string                  `json:"configDigest"`
	SourceRoot        string                  `json:"sourceRoot"`
	SourceIdentity    string                  `json:"sourceIdentity"`
	SourceID          string                  `json:"sourceId"`
	Actor             string                  `json:"actor"`
	Task              downloadcompletion.Task `json:"task"`
	Files             []File                  `json:"files"`
}
type DownloadBatch struct {
	ID          string         `json:"id"`
	Origin      DownloadOrigin `json:"origin"`
	Fingerprint string         `json:"fingerprint"`
	State       string         `json:"state"`
	Reason      string         `json:"reason,omitempty"`
	Jobs        []string       `json:"jobs"`
	Omitted     []File         `json:"omitted"`
	Created     string         `json:"created"`
	ResultProof string         `json:"resultProof,omitempty"`
	Expected    int            `json:"expected"`
	Mode        string         `json:"mode"`
}
type downloadBatchDocument struct {
	Origin      DownloadOrigin
	Definitions []Definition
	Omitted     []File
}
type DownloadResultIntent struct {
	BatchID              string   `json:"batchId"`
	Actor                string   `json:"actor"`
	Operation            string   `json:"operation"`
	BeforeLabels         []string `json:"beforeLabels"`
	AfterLabels          []string `json:"afterLabels"`
	NeedsExclusiveLabels bool     `json:"needsExclusiveLabels"`
	Fingerprint          string   `json:"fingerprint"`
}

func downloadTables(ctx context.Context, w syncWriter) error {
	_, err := w.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_DOWNLOAD_ORGANIZATION_BATCHES(ID TEXT PRIMARY KEY,TASK_KEY TEXT NOT NULL UNIQUE,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',CREATED TEXT NOT NULL,RESULT_PROOF TEXT NOT NULL DEFAULT '')`)
	return err
}
func DownloadTaskKey(downloaderID, taskID string) string {
	return Digest([]string{downloaderID, taskID})
}
func verifyDownloadConfig(ctx context.Context, r unknownReader, origin DownloadOrigin) error {
	id, err := strconv.ParseInt(origin.DownloaderID, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != origin.DownloaderID {
		return ErrState
	}
	var item downloaderconfig.Downloader
	err = r.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(ENABLED,0),COALESCE(TYPE,''),COALESCE(TRANSFER,0),COALESCE(ONLY_NASTOOL,0),COALESCE(MATCH_PATH,0),COALESCE(RMT_MODE,''),COALESCE(CONFIG,'{}'),COALESCE(DOWNLOAD_DIR,'[]') FROM DOWNLOADER WHERE ID=?`, id).Scan(&item.ID, &item.Name, &item.Enabled, &item.Type, &item.Transfer, &item.OnlyNastool, &item.MatchPath, &item.RmtMode, &item.Config, &item.DownloadDir)
	if err != nil {
		return err
	}
	if item.Enabled != 1 || item.Transfer != 1 || Digest(item) != origin.ConfigDigest || item.Type != origin.DownloaderType {
		return ErrState
	}
	if item.OnlyNastool == 1 && item.Type == "aria2" {
		var exists bool
		if err := r.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM DOWNLOAD_HISTORY WHERE DOWNLOADER=? AND DOWNLOAD_ID=?)`, origin.DownloaderID, origin.Task.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrState
		}
	}
	return nil
}
func verifyDownloadDefinition(ctx context.Context, r unknownReader, d Definition, reserving bool) error {
	if d.Download == nil {
		return nil
	}
	o := d.Download
	if o.Actor == "" || o.BatchID == "" || o.SourceRoot != d.SourceRoot || o.SourceIdentity != d.SourceIdentity || o.SourceID != d.SourceID || d.Sync != nil || d.History != nil || d.Unknown != nil || d.LegacyOrigin != nil || d.LegacyTargetOverride {
		return ErrState
	}
	if err := verifyDownloadConfig(ctx, r, *o); err != nil {
		return err
	}
	var raw, state string
	if err := r.QueryRowContext(ctx, `SELECT DOCUMENT,STATE FROM GO_DOWNLOAD_ORGANIZATION_BATCHES WHERE ID=?`, o.BatchID).Scan(&raw, &state); err != nil {
		return err
	}
	if state != "ready" && state != "executing" && state != "needs_review" && !(reserving && state == "reserving") {
		return ErrState
	}
	var doc downloadBatchDocument
	if json.Unmarshal([]byte(raw), &doc) != nil || Digest(doc.Origin) != Digest(*o) {
		return ErrState
	}
	for _, expected := range doc.Definitions {
		if Digest(expected) == Digest(d) {
			return nil
		}
	}
	return ErrState
}
func (s *Store) VerifyDownload(ctx context.Context, job Job) error {
	return verifyDownloadDefinition(ctx, s.db, job.Definition, false)
}
func (s *Store) VerifyDownloadConfig(ctx context.Context, origin DownloadOrigin) error {
	return verifyDownloadConfig(ctx, s.db, origin)
}

// Mutable seeding/paused state and added user labels do not change a torrent's
// file authority. Removed original labels, changed file selection or a newly
// organized task do; no display-name-based source substitution is permitted.
func DownloadTaskMatches(expected, current downloadcompletion.Task, allowOrganized bool) bool {
	if !expected.Complete || !current.Complete || expected.LabelsKnown != current.LabelsKnown || !allowOrganized && current.HasLabel("已整理") {
		return false
	}
	for _, label := range expected.Labels {
		if !current.HasLabel(label) {
			return false
		}
	}
	expected.State = ""
	current.State = ""
	expected.Labels = nil
	current.Labels = nil
	return Digest(expected) == Digest(current)
}
func (s *Store) BeginDownloadBatch(ctx context.Context, fp string, origin DownloadOrigin, definitions []Definition, omitted []File) (DownloadBatch, []Definition, error) {
	if len(fp) != 64 || len(definitions) == 0 || len(definitions) > 128 || len(origin.Files) == 0 || !origin.Task.Complete {
		return DownloadBatch{}, nil, ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DownloadBatch{}, nil, err
	}
	defer tx.Rollback()
	if origin.MonitorGeneration != "" {
		if err := verifyDownloadMonitor(ctx, tx, origin.MonitorGeneration); err != nil {
			return DownloadBatch{}, nil, err
		}
	}
	if err := verifyDownloadConfig(ctx, tx, origin); err != nil {
		return DownloadBatch{}, nil, err
	}
	if err := downloadTables(ctx, tx); err != nil {
		return DownloadBatch{}, nil, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return DownloadBatch{}, nil, err
	}
	id := hex.EncodeToString(random[:])
	origin.BatchID = id
	defs := append([]Definition{}, definitions...)
	for i := range defs {
		copy := origin
		defs[i].Download = &copy
	}
	raw, err := json.Marshal(downloadBatchDocument{origin, defs, omitted})
	if err != nil {
		return DownloadBatch{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_DOWNLOAD_ORGANIZATION_BATCHES(ID,TASK_KEY,DOCUMENT,FINGERPRINT,STATE,CREATED) VALUES(?,?,?,?,'reserving',?)`, id, DownloadTaskKey(origin.DownloaderID, origin.Task.ID), string(raw), fp, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return DownloadBatch{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return DownloadBatch{}, nil, err
	}
	batch, err := s.GetDownloadBatch(ctx, id)
	return batch, defs, err
}
func (s *Store) FindDownloadBatch(ctx context.Context, downloaderID, taskID string) (DownloadBatch, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_DOWNLOAD_ORGANIZATION_BATCHES' AND type='table')`).Scan(&exists); err != nil {
		return DownloadBatch{}, err
	}
	if !exists {
		return DownloadBatch{}, ErrNotFound
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT ID FROM GO_DOWNLOAD_ORGANIZATION_BATCHES WHERE TASK_KEY=?`, DownloadTaskKey(downloaderID, taskID)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return DownloadBatch{}, ErrNotFound
	}
	if err != nil {
		return DownloadBatch{}, err
	}
	return s.GetDownloadBatch(ctx, id)
}
func (s *Store) GetDownloadBatch(ctx context.Context, id string) (DownloadBatch, error) {
	b := DownloadBatch{ID: id, Jobs: []string{}}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT DOCUMENT,FINGERPRINT,STATE,REASON,CREATED,RESULT_PROOF FROM GO_DOWNLOAD_ORGANIZATION_BATCHES WHERE ID=?`, id).Scan(&raw, &b.Fingerprint, &b.State, &b.Reason, &b.Created, &b.ResultProof)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	var doc downloadBatchDocument
	if json.Unmarshal([]byte(raw), &doc) != nil || doc.Origin.BatchID != id {
		return b, ErrState
	}
	b.Origin, b.Omitted, b.Expected = doc.Origin, doc.Omitted, len(doc.Definitions)
	if len(doc.Definitions) == 0 {
		return b, ErrState
	}
	b.Mode = doc.Definitions[0].Mode
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_ORGANIZATION_JOBS WHERE json_extract(DEFINITION,'$.download.batchId')=? ORDER BY CREATED,ID`, id)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return b, err
		}
		b.Jobs = append(b.Jobs, id)
	}
	return b, rows.Err()
}
func (s *Store) SetDownloadBatch(ctx context.Context, id, from, to, reason, proof string) error {
	return s.SetDownloadBatchAuthorized(ctx, id, from, to, reason, proof, "")
}
func (s *Store) SetDownloadBatchAuthorized(ctx context.Context, id, from, to, reason, proof, generation string) error {
	allowed := map[string]bool{"ready": true, "executing": true, "needs_review": true, "files_done": true, "result_pending": true, "completed": true}
	if !allowed[to] {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if generation != "" {
		if err := verifyDownloadMonitor(ctx, tx, generation); err != nil {
			return err
		}
	}
	if to == "result_pending" || to == "completed" {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_DOWNLOAD_ORGANIZATION_BATCHES WHERE ID=?`, id).Scan(&raw); err != nil {
			return err
		}
		var doc downloadBatchDocument
		if json.Unmarshal([]byte(raw), &doc) != nil {
			return ErrState
		}
		if err := verifyDownloadConfig(ctx, tx, doc.Origin); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_DOWNLOAD_ORGANIZATION_BATCHES SET STATE=?,REASON=?,RESULT_PROOF=? WHERE ID=? AND STATE=?`, to, reason, proof, id, from)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	return tx.Commit()
}
