package downloaderconfig

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

var ErrFileSelectionPending = errors.New("original torrent file selection requires explicit review")

type FileSelectionDefinition struct {
	Sources           []FileSelectionSource `json:"sources,omitempty"`
	Version           int                   `json:"version"`
	ID                string                `json:"id"`
	DownloaderID      int64                 `json:"downloaderId"`
	DownloaderType    string                `json:"downloaderType"`
	ConfigDigest      string                `json:"configDigest"`
	InfoHash          string                `json:"infoHash"`
	Title             string                `json:"title"`
	SubscriptionID    int64                 `json:"subscriptionId"`
	Season            int                   `json:"season"`
	Episodes          []int                 `json:"episodes"`
	Scope             string                `json:"scope"`
	Options           json.RawMessage       `json:"options"`
	PresetFingerprint string                `json:"presetFingerprint"`
	Proof             string                `json:"proof"`
	Files             []torrentfiles.File   `json:"files"`
}
type FileSelectionSource struct {
	SubscriptionID int64 `json:"subscriptionId,string"`
	Season         int   `json:"season"`
	Total          int   `json:"total"`
	Episodes       []int `json:"episodes"`
}
type FileSelection struct {
	Definition  FileSelectionDefinition `json:"-"`
	Fingerprint string                  `json:"fingerprint"`
	State       string                  `json:"state"`
	Committed   bool                    `json:"sourceRecorded"`
	Created     string                  `json:"created"`
	Updated     string                  `json:"updated"`
}

func (d FileSelectionDefinition) Marker() string             { return "NASTOOL-FILES-" + d.ID }
func (d FileSelectionDefinition) ProfileFingerprint() string { return "files:" + d.ID }
func validSelectionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			if ch < 'a' || ch > 'f' {
				return false
			}
		}
	}
	return true
}
func validSelection(d FileSelectionDefinition) bool {
	if d.Version != 1 && d.Version != 2 || !validSelectionID(d.ID) || d.DownloaderID < 1 || d.SubscriptionID < 1 || d.DownloaderType != "qbittorrent" && d.DownloaderType != "transmission" || !validSeedingDigest(d.ConfigDigest) || !validSeedingDigest(d.Scope) || !validSeedingDigest(d.InfoHash) && len(d.InfoHash) != 40 || len(d.InfoHash) == 64 && d.DownloaderType != "qbittorrent" || len(d.Title) == 0 || len(d.Title) > 4096 || d.Season < 0 || d.Season > 1000 || len(d.Episodes) == 0 || len(d.Episodes) > 10000 || !slices.IsSorted(d.Episodes) || len(d.Options) > 32<<10 || !json.Valid(d.Options) || len(d.Proof) > 2<<20 || !json.Valid([]byte(d.Proof)) {
		return false
	}
	for i, n := range d.Episodes {
		if n < 1 || n > 10000 || i > 0 && d.Episodes[i-1] == n {
			return false
		}
	}
	for _, ch := range d.InfoHash {
		if ch < '0' || ch > '9' {
			if ch < 'a' || ch > 'f' {
				return false
			}
		}
	}
	if d.Version == 1 && len(d.Sources) != 0 || d.Version == 2 && (len(d.Sources) == 0 || len(d.Sources) > 100) {
		return false
	}
	lead := d.Version == 1
	for i, source := range d.Sources {
		if source.SubscriptionID < 1 || i > 0 && d.Sources[i-1].SubscriptionID >= source.SubscriptionID || source.Season < 0 || source.Season > 1000 || source.Total < 1 || source.Total > 10000 || len(source.Episodes) == 0 || !slices.IsSorted(source.Episodes) {
			return false
		}
		for j, n := range source.Episodes {
			if n < 1 || n > source.Total || j > 0 && source.Episodes[j-1] == n {
				return false
			}
		}
		if source.SubscriptionID == d.SubscriptionID {
			lead = source.Season == d.Season && slices.Equal(source.Episodes, d.Episodes)
		}
	}
	if !lead {
		return false
	}
	files, err := torrentfiles.Normalize(d.Files)
	return err == nil && slices.Equal(files, d.Files) && len(torrentfiles.IDs(files, true)) > 0
}

const fileSelectionSelect = `SELECT DOCUMENT,FINGERPRINT,STATE,COMMITTED,CREATED,UPDATED FROM GO_TORRENT_FILE_SELECTIONS`

func scanFileSelection(row interface{ Scan(...any) error }) (FileSelection, error) {
	p := FileSelection{}
	var raw string
	if err := row.Scan(&raw, &p.Fingerprint, &p.State, &p.Committed, &p.Created, &p.Updated); err != nil {
		return p, err
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 8<<20 || decoder.Decode(&p.Definition) != nil || decoder.Decode(new(any)) != io.EOF || !validSelection(p.Definition) || bus.Hash(p.Definition) != p.Fingerprint || !slices.Contains([]string{"adding", "added", "unwanting", "unwanted", "wanting", "selected", "starting", "ready", "rejected"}, p.State) {
		return p, ErrFileSelectionPending
	}
	if p.Committed && p.State != "ready" {
		return p, ErrFileSelectionPending
	}
	return p, nil
}
func selectionSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_TORRENT_FILE_SELECTIONS(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,STATE TEXT NOT NULL,COMMITTED INTEGER NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_TORRENT_FILE_SELECTION_SCOPES(SCOPE TEXT PRIMARY KEY,SELECTION_ID TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS GO_TORRENT_FILE_SELECTION_ACTIONS(ID INTEGER PRIMARY KEY,SELECTION_ID TEXT NOT NULL,ACTION TEXT NOT NULL,ACTOR TEXT NOT NULL,REVIEW TEXT NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func selectionOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrFileSelectionPending
	}
	return nil
}

// The original subscription intent, exact files and unique hash scope are
// reserved in one transaction, before any paused add request is issued.
func ReserveFileSelectionTx(ctx context.Context, tx *sql.Tx, d FileSelectionDefinition) error {
	if !validSelection(d) {
		return ErrFileSelectionPending
	}
	if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: d.DownloaderID, DownloaderType: d.DownloaderType, ConfigDigest: d.ConfigDigest}); err != nil {
		return err
	}
	if err := selectionSchema(ctx, tx); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(d)
	if err != nil || len(raw) > 8<<20 {
		return ErrFileSelectionPending
	}
	if err := selectionOne(tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_FILE_SELECTIONS VALUES(?,?,?,'adding',0,?,?)`, d.ID, string(raw), bus.Hash(d), now, now)); err != nil {
		return err
	}
	return selectionOne(tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_FILE_SELECTION_SCOPES VALUES(?,?)`, profileScope(d.DownloaderID, d.InfoHash), d.ID))
}
func (s *Store) FileSelection(ctx context.Context, id string) (FileSelection, error) {
	if !validSelectionID(id) {
		return FileSelection{}, ErrFileSelectionPending
	}
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_TORRENT_FILE_SELECTIONS')`).Scan(&exists); err != nil {
		return FileSelection{}, err
	}
	if !exists {
		return FileSelection{}, ErrNotFound
	}
	p, err := scanFileSelection(s.database.QueryRowContext(ctx, fileSelectionSelect+` WHERE ID=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	if err == nil && p.Definition.ID != id {
		err = ErrFileSelectionPending
	}
	return p, err
}
func (s *Store) FindFileSelection(ctx context.Context, id int64, hash string) (*FileSelection, error) {
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_TORRENT_FILE_SELECTION_SCOPES')`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	var key string
	err := s.database.QueryRowContext(ctx, `SELECT SELECTION_ID FROM GO_TORRENT_FILE_SELECTION_SCOPES WHERE SCOPE=?`, profileScope(id, hash)).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p, err := s.FileSelection(ctx, key)
	return &p, err
}
func (s *Store) FileSelections(ctx context.Context, page int) ([]FileSelection, error) {
	out := []FileSelection{}
	if page < 1 || page > 10000 {
		return nil, ErrFileSelectionPending
	}
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_TORRENT_FILE_SELECTIONS')`).Scan(&exists); err != nil || !exists {
		return out, err
	}
	rows, err := s.database.QueryContext(ctx, fileSelectionSelect+` ORDER BY CREATED DESC,ID LIMIT 30 OFFSET ?`, (page-1)*30)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanFileSelection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func FileSelectionTransitionTx(ctx context.Context, tx *sql.Tx, p FileSelection, to, actor, review string) error {
	allowed := map[string][]string{"adding": {"added", "rejected"}, "added": {"added", "unwanting", "unwanted", "wanting"}, "unwanting": {"unwanted"}, "unwanted": {"wanting"}, "wanting": {"selected"}, "selected": {"selected", "starting", "ready"}, "starting": {"starting", "ready"}}
	if !slices.Contains(allowed[p.State], to) {
		return ErrFileSelectionPending
	}
	if actor == "" || p.Committed || !slices.Contains([]string{"added", "unwanting", "unwanted", "wanting", "selected", "starting", "ready", "rejected"}, to) {
		return ErrFileSelectionPending
	}
	if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: p.Definition.DownloaderID, DownloaderType: p.Definition.DownloaderType, ConfigDigest: p.Definition.ConfigDigest}); err != nil {
		return err
	}
	if err := selectionOne(tx.ExecContext(ctx, `UPDATE GO_TORRENT_FILE_SELECTIONS SET STATE=?,UPDATED=? WHERE ID=? AND STATE=? AND FINGERPRINT=? AND COMMITTED=0`, to, time.Now().UTC().Format(time.RFC3339Nano), p.Definition.ID, p.State, p.Fingerprint)); err != nil {
		return err
	}
	if to == "rejected" {
		if p.State != "adding" {
			return ErrFileSelectionPending
		}
		if err := selectionOne(tx.ExecContext(ctx, `DELETE FROM GO_TORRENT_FILE_SELECTION_SCOPES WHERE SCOPE=? AND SELECTION_ID=?`, profileScope(p.Definition.DownloaderID, p.Definition.InfoHash), p.Definition.ID)); err != nil {
			return err
		}
	}
	return selectionOne(tx.ExecContext(ctx, `INSERT INTO GO_TORRENT_FILE_SELECTION_ACTIONS(SELECTION_ID,ACTION,ACTOR,REVIEW,CREATED) VALUES(?,?,?,?,?)`, p.Definition.ID, to, actor, review, time.Now().UTC().Format(time.RFC3339Nano)))
}
func VerifyFileSelection(ctx context.Context, tx *sql.Tx, id string) error {
	if !validSelectionID(id) {
		return ErrFileSelectionPending
	}
	p, err := scanFileSelection(tx.QueryRowContext(ctx, fileSelectionSelect+` WHERE ID=?`, id))
	if err != nil || p.Definition.ID != id || p.State != "ready" {
		return ErrFileSelectionPending
	}
	if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: p.Definition.DownloaderID, DownloaderType: p.Definition.DownloaderType, ConfigDigest: p.Definition.ConfigDigest}); err != nil {
		return err
	}
	if strings.HasPrefix(p.Definition.PresetFingerprint, "files:") {
		return ErrFileSelectionPending
	}
	return VerifyDownloadProfile(ctx, tx, p.Definition.PresetFingerprint)
}
func FileSelectionTx(ctx context.Context, tx *sql.Tx, id string) (FileSelection, error) {
	if !validSelectionID(id) {
		return FileSelection{}, ErrFileSelectionPending
	}
	p, err := scanFileSelection(tx.QueryRowContext(ctx, fileSelectionSelect+` WHERE ID=?`, id))
	if err == nil && p.Definition.ID != id {
		return p, ErrFileSelectionPending
	}
	return p, err
}
func CommitFileSelectionTx(ctx context.Context, tx *sql.Tx, id, payload string) error {
	if err := VerifyFileSelection(ctx, tx, id); err != nil {
		return err
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_TORRENT_FILE_SELECTIONS WHERE ID=?`, id).Scan(&raw); err != nil {
		return err
	}
	var d FileSelectionDefinition
	if json.Unmarshal([]byte(raw), &d) != nil || d.Proof != payload {
		return ErrFileSelectionPending
	}
	return selectionOne(tx.ExecContext(ctx, `UPDATE GO_TORRENT_FILE_SELECTIONS SET COMMITTED=1,UPDATED=? WHERE ID=? AND STATE='ready' AND COMMITTED=0`, time.Now().UTC().Format(time.RFC3339Nano), id))
}
