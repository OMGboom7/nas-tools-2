package organization

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

var ErrClaimed = errors.New("organization target is already reserved")
var ErrState = errors.New("organization item requires review")
var ErrNotFound = errors.New("organization job not found")
var ErrMode = errors.New("organization mode is unavailable on this filesystem")
var ErrBusy = errors.New("organization operation is already active")

func SupportedMode(mode string) bool {
	return mode == "copy" || mode == "link" || mode == "softlink" || mode == "move"
}

// Persisted receipt values: keep stable across upgrades; translate in the UI,
// not by changing these strings or clearing a pending receipt on startup.
const MoveSourceCleanupPending = "Move source recovery cleanup pending; explicit source-removal confirmation required"
const MoveTargetCleanupPending = "Move target recovery cleanup pending"

// Whole-disc hash/copy/verification can exceed ordinary request timeouts on a
// NAS. Cancellation still propagates; this does not authorize automatic replay.
const ExecutionTimeout = 2 * time.Hour

func ModeLabel(mode string) string {
	return map[string]string{"copy": "复制", "link": "硬链接", "softlink": "软链接", "move": "移动"}[mode]
}

type Entry struct {
	Disc          *DiscInfo `json:"disc,omitempty"`
	PartOverride  *string   `json:"partOverride,omitempty"`
	EpisodeFormat string    `json:"episodeFormat,omitempty"`
	EpisodeOffset int       `json:"episodeOffset,omitempty"`
	Part          string    `json:"part,omitempty"`
	Assigned      bool      `json:"assigned,omitempty"`
	Source        string    `json:"source"`
	Target        string    `json:"target"`
	Kind          string    `json:"kind"`
	Size          int64     `json:"size"`
	Modified      string    `json:"modified"`
	Identity      string    `json:"identity"`
	TMDBID        string    `json:"tmdbId"`
	Title         string    `json:"title"`
	Year          string    `json:"year"`
	MediaType     string    `json:"mediaType"`
	Category      string    `json:"category"`
	SeasonEpisode string    `json:"seasonEpisode"`
}

type EpisodeSelection struct {
	TMDBID        string  `json:"tmdbId"`
	Season        *int    `json:"season"`
	EpisodeFormat string  `json:"episodeFormat"`
	StartEpisode  *int    `json:"startEpisode,omitempty"`
	EndEpisode    *int    `json:"endEpisode,omitempty"`
	EpisodeOffset int     `json:"episodeOffset,omitempty"`
	PartOverride  *string `json:"partOverride,omitempty"`
}

type LegacyOrganizationOrigin struct {
	Action   string `json:"action"`
	Kind     string `json:"kind"`
	RecordID string `json:"recordId"`
	Actor    string `json:"actor"`
}
type LegacyMediaSelection struct {
	Kind           string  `json:"kind,omitempty"`
	TMDBID         string  `json:"tmdbId,omitempty"`
	Season         *int    `json:"season,omitempty"`
	EpisodeFormat  string  `json:"episodeFormat,omitempty"`
	EpisodeDetails string  `json:"episodeDetails,omitempty"`
	EpisodeOffset  int     `json:"episodeOffset,omitempty"`
	PartOverride   *string `json:"partOverride,omitempty"`
	MinimumMB      *int64  `json:"minimumMB,omitempty"`
}

// Legacy rename can explicitly select a new mode and, with separate consent,
// expand a recorded single file to its parent. A partial/expanded unknown scope
// must leave the legacy record unchanged rather than claim full resolution.
type LegacyRenameOptions struct {
	ExpandedScope   bool `json:"expandedScope,omitempty"`
	PreserveUnknown bool `json:"preserveUnknown,omitempty"`
}

type Definition struct {
	AllowDiscExpansion    bool                      `json:"allowDiscExpansion,omitempty"`
	Discs                 map[string]DiscManifest   `json:"discs,omitempty"`
	ScraperEnabled        bool                      `json:"scraperEnabled,omitempty"`
	Download              *DownloadOrigin           `json:"download,omitempty"`
	Sync                  *SyncOrigin               `json:"sync,omitempty"`
	LegacyTargetOverride  bool                      `json:"legacyTargetOverride,omitempty"`
	LegacyRename          *LegacyRenameOptions      `json:"legacyRename,omitempty"`
	LegacySelection       *LegacyMediaSelection     `json:"legacySelection,omitempty"`
	LegacyOrigin          *LegacyOrganizationOrigin `json:"legacyOrigin,omitempty"`
	UnknownAllowProcessed bool                      `json:"unknownAllowProcessed,omitempty"`
	EpisodeSelection      *EpisodeSelection         `json:"episodeSelection,omitempty"`
	History               *HistoryRecord            `json:"history,omitempty"`
	Unknown               *UnknownRecord            `json:"unknown,omitempty"`
	UnknownScope          string                    `json:"unknownScope,omitempty"`
	UnknownScopeIdentity  string                    `json:"unknownScopeIdentity,omitempty"`
	UnknownDirectory      bool                      `json:"unknownDirectory,omitempty"`
	SourceRoot            string                    `json:"sourceRoot"`
	TargetRoot            string                    `json:"targetRoot"`
	SourceIdentity        string                    `json:"sourceIdentity"`
	TargetIdentity        string                    `json:"targetIdentity"`
	ConfigDigest          string                    `json:"configDigest"`
	SourceID              string                    `json:"sourceId"`
	TargetID              string                    `json:"targetId"`
	Mode                  string                    `json:"mode"`
	Entries               []Entry                   `json:"entries"`
}

type Proof struct {
	Disc                 *DiscProof `json:"disc,omitempty"`
	Incomplete           bool       `json:"incomplete,omitempty"`
	Anchor               bool       `json:"anchor,omitempty"`
	Temp                 string     `json:"temp"`
	Identity             string     `json:"identity"`
	ParentIdentity       string     `json:"parentIdentity"`
	StagingIdentity      string     `json:"stagingIdentity"`
	Digest               string     `json:"digest"`
	Kind                 string     `json:"kind,omitempty"`
	LinkTarget           string     `json:"linkTarget,omitempty"`
	SourceHold           string     `json:"sourceHold,omitempty"`
	SourceHoldIdentity   string     `json:"sourceHoldIdentity,omitempty"`
	SourceParentIdentity string     `json:"sourceParentIdentity,omitempty"`
}

type JobItem struct {
	Entry
	Index  int    `json:"index"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Proof  Proof  `json:"-"`
}

type Job struct {
	SourceRestore        *SourceRestoreReceipt     `json:"sourceRestore,omitempty"`
	DownloadGeneration   string                    `json:"-"`
	Download             *DownloadOrigin           `json:"download,omitempty"`
	AutomaticGeneration  string                    `json:"-"`
	Sync                 *SyncOrigin               `json:"sync,omitempty"`
	LegacyTargetOverride bool                      `json:"legacyTargetOverride,omitempty"`
	LegacyRename         *LegacyRenameOptions      `json:"legacyRename,omitempty"`
	LegacySelection      *LegacyMediaSelection     `json:"legacySelection,omitempty"`
	LegacyOrigin         *LegacyOrganizationOrigin `json:"legacyOrigin,omitempty"`
	EpisodeSelection     *EpisodeSelection         `json:"episodeSelection,omitempty"`
	HistoryRecordID      int64                     `json:"historyRecordId,omitempty"`
	UnknownDetach        *UnknownDetachReceipt     `json:"unknownDetach,omitempty"`
	UnknownRecordID      int64                     `json:"unknownRecordId,omitempty"`
	UnknownState         string                    `json:"unknownState,omitempty"`
	ID                   string                    `json:"id"`
	Fingerprint          string                    `json:"fingerprint"`
	Created              string                    `json:"created"`
	State                string                    `json:"state"`
	Mode                 string                    `json:"mode"`
	SourceRoot           string                    `json:"sourceRoot"`
	TargetRoot           string                    `json:"targetRoot"`
	Items                []JobItem                 `json:"items"`
	Definition           Definition                `json:"-"`
}

type Store struct {
	db                *sql.DB
	guardRoot         string
	guardRootIdentity string
}

func Digest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

// Open must follow the application's pre-Go backup. New tables are additive;
// the existing Python-compatible history is written only after publication.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_JOBS (ID TEXT PRIMARY KEY, FINGERPRINT TEXT NOT NULL, DEFINITION TEXT NOT NULL, CREATED TEXT NOT NULL, STATE TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_ITEMS (JOB_ID TEXT NOT NULL, ORDINAL INTEGER NOT NULL, STATE TEXT NOT NULL, REASON TEXT NOT NULL DEFAULT '', PROOF TEXT NOT NULL DEFAULT '{}', PRIMARY KEY(JOB_ID,ORDINAL))`,
		`CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_TARGETS (TARGET_KEY TEXT PRIMARY KEY, JOB_ID TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_UNKNOWN_LINKS (JOB_ID TEXT PRIMARY KEY, RECORD_ID INTEGER NOT NULL, STATE TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS GO_ORGANIZATION_UNKNOWN_PENDING ON GO_ORGANIZATION_UNKNOWN_LINKS(RECORD_ID) WHERE STATE='pending'`,
		`CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_UNKNOWN_DETACH (JOB_ID TEXT PRIMARY KEY, RECEIPT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_UNKNOWN_DELETE (ID TEXT PRIMARY KEY,RECORD_ID INTEGER NOT NULL,RECEIPT TEXT NOT NULL,RESTORE TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS GO_ORGANIZATION_SOURCE_RESTORE (JOB_ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL)`,
	} {
		if _, err = db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		db.Close()
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		db.Close()
		return nil, err
	}
	parent := filepath.Dir(canonical)
	info, err := os.Stat(parent)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, guardRoot: parent, guardRootIdentity: Identity(info)}, nil
}
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Create(ctx context.Context, fingerprint string, d Definition) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := createJobTx(ctx, tx, fingerprint, d)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// Shared by ordinary creation and atomic completion of a reviewed reservation.
func createJobTx(ctx context.Context, tx *sql.Tx, fingerprint string, d Definition) (string, error) {
	if d.Sync != nil {
		if d.Sync.Actor == "" || d.Sync.ScopeIdentity == "" || len(d.Sync.GraphDigest) != 64 || d.Sync.Record.Rename < 0 || d.Sync.Record.Rename > 1 || d.Unknown != nil || d.History != nil || d.LegacyOrigin != nil || !validRelative(d.Sync.Scope) {
			return "", ErrState
		}
		for _, entry := range d.Entries {
			if d.Sync.Record.Rename == 0 && (entry.Kind != "file" || entry.Source != entry.Target || entry.TMDBID != "") || d.Sync.Record.Rename == 1 && (entry.Kind == "file" || entry.TMDBID == "") {
				return "", ErrState
			}
		}
	}
	if d.LegacyTargetOverride && d.Sync == nil && (d.LegacyOrigin == nil || d.LegacySelection == nil && d.Unknown == nil && d.History == nil) {
		return "", ErrState
	}
	if d.LegacyRename != nil && (d.LegacyOrigin == nil || d.LegacyOrigin.Action != "rename" || d.LegacySelection == nil || d.Unknown == nil && d.History == nil || d.LegacyRename.PreserveUnknown && d.Unknown == nil) {
		return "", ErrState
	}
	if d.LegacyRename != nil {
		if d.LegacyOrigin.Actor == "" || d.LegacyRename.ExpandedScope && (d.LegacySelection.EpisodeFormat == "" || d.Unknown != nil && !d.LegacyRename.PreserveUnknown) {
			return "", ErrState
		}
		if d.Unknown != nil && (d.LegacyOrigin.Kind != "unidentification" || d.LegacyOrigin.RecordID != strconv.FormatInt(d.Unknown.ID, 10)) || d.History != nil && (d.LegacyOrigin.Kind != "history" || d.LegacyOrigin.RecordID != strconv.FormatInt(d.History.ID, 10)) {
			return "", ErrState
		}
	}
	if len(fingerprint) != 64 || !SupportedMode(d.Mode) || len(d.Entries) == 0 || len(d.Entries) > 1000 || d.SourceIdentity == "" || d.TargetIdentity == "" || !filepath.IsAbs(d.SourceRoot) || !filepath.IsAbs(d.TargetRoot) {
		return "", ErrPath
	}
	for _, entry := range d.Entries {
		if !validRelative(entry.Source) || !validRelative(entry.Target) || entry.Source == "." && !IsDisc(entry) || entry.Target == "." || entry.Identity == "" || entry.Size < 0 {
			return "", ErrPath
		}
		if IsDisc(entry) {
			if entry.Disc.Expanded && !d.AllowDiscExpansion {
				return "", ErrState
			}
			if _, err := discManifestDefinition(d, entry); err != nil {
				return "", err
			}
			if d.Mode == "move" && entry.Source == "." {
				return "", ErrMode
			}
		}
	}
	if err := verifySyncRecord(ctx, tx, d); err != nil {
		return "", err
	}
	if err := verifyDownloadDefinition(ctx, tx, d, true); err != nil {
		return "", err
	}
	if d.Sync != nil && d.Sync.Record.Rename == 0 {
		for _, entry := range d.Entries {
			exists, err := syncRecorded(ctx, tx, d, entry.Source)
			if err != nil {
				return "", err
			}
			if exists {
				return "", ErrState
			}
		}
	}
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT ID FROM GO_ORGANIZATION_JOBS WHERE FINGERPRINT=? AND STATE='active' ORDER BY CREATED DESC LIMIT 1`, fingerprint).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_JOBS VALUES (?,?,?,?,'active')`, id, fingerprint, string(raw), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	if d.Unknown != nil {
		current, err := readUnknown(ctx, tx, d.Unknown.ID)
		if err != nil {
			return "", err
		}
		if current.State != "N" && !(d.UnknownAllowProcessed && current.State == "Y") || Digest(current) != Digest(*d.Unknown) {
			return "", ErrState
		}
		var claims int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE RECORD_ID=? AND STATE='pending'`, current.ID).Scan(&claims); err != nil {
			return "", err
		}
		if claims != 0 {
			return "", ErrClaimed
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_UNKNOWN_LINKS VALUES (?,?,'pending')`, id, current.ID); err != nil {
			return "", err
		}
	}
	if d.History != nil {
		if d.Unknown != nil {
			return "", ErrState
		}
		current, err := readHistory(ctx, tx, d.History.ID)
		if err != nil {
			return "", err
		}
		if Digest(current) != Digest(*d.History) {
			return "", ErrState
		}
	}
	for index, entry := range d.Entries {
		key := Digest(d.TargetIdentity + ":" + entry.Target)
		result, err := tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_TARGETS VALUES (?,?) ON CONFLICT(TARGET_KEY) DO NOTHING`, key, id)
		if err != nil {
			return "", err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return "", err
		}
		if n != 1 {
			return "", ErrClaimed
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO GO_ORGANIZATION_ITEMS (JOB_ID,ORDINAL,STATE) VALUES (?,?,'planned')`, id, index); err != nil {
			return "", err
		}
	}
	if err := reserveDiscTargets(ctx, tx, id, d); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	job := Job{ID: id, Items: []JobItem{}}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT FINGERPRINT,DEFINITION,CREATED,STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&job.Fingerprint, &raw, &job.Created, &job.State)
	if errors.Is(err, sql.ErrNoRows) {
		return job, ErrNotFound
	}
	if err != nil {
		return job, err
	}
	if err = json.Unmarshal([]byte(raw), &job.Definition); err != nil {
		return job, err
	}
	job.EpisodeSelection = job.Definition.EpisodeSelection
	job.Mode = job.Definition.Mode
	job.LegacyOrigin = job.Definition.LegacyOrigin
	job.LegacySelection = job.Definition.LegacySelection
	job.LegacyRename = job.Definition.LegacyRename
	job.LegacyTargetOverride = job.Definition.LegacyTargetOverride
	job.Sync = job.Definition.Sync
	job.Download = job.Definition.Download
	if job.Definition.History != nil {
		job.HistoryRecordID = job.Definition.History.ID
	}
	job.SourceRoot, job.TargetRoot = job.Definition.SourceRoot, job.Definition.TargetRoot
	rows, err := s.db.QueryContext(ctx, `SELECT ORDINAL,STATE,REASON,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? ORDER BY ORDINAL`, id)
	if err != nil {
		return job, err
	}
	defer rows.Close()
	for rows.Next() {
		var item JobItem
		var proof string
		if err = rows.Scan(&item.Index, &item.State, &item.Reason, &proof); err != nil {
			return job, err
		}
		if item.Index < 0 || item.Index >= len(job.Definition.Entries) {
			return job, ErrState
		}
		item.Entry = job.Definition.Entries[item.Index]
		if err = json.Unmarshal([]byte(proof), &item.Proof); err != nil {
			return job, err
		}
		job.Items = append(job.Items, item)
	}
	if err = rows.Err(); err != nil {
		return job, err
	}
	if len(job.Items) != len(job.Definition.Entries) {
		return job, ErrState
	}
	rows.Close()
	if job.Definition.Unknown != nil {
		job.UnknownRecordID = job.Definition.Unknown.ID
		if err = s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE JOB_ID=? AND RECORD_ID=?`, job.ID, job.UnknownRecordID).Scan(&job.UnknownState); err != nil {
			return job, err
		}
		if job.UnknownState == "detached" {
			var raw string
			if err = s.db.QueryRowContext(ctx, `SELECT RECEIPT FROM GO_ORGANIZATION_UNKNOWN_DETACH WHERE JOB_ID=?`, job.ID).Scan(&raw); err != nil {
				return job, err
			}
			job.UnknownDetach = &UnknownDetachReceipt{}
			if err = json.Unmarshal([]byte(raw), job.UnknownDetach); err != nil {
				return job, err
			}
		}
	}
	if job.State == SourceRestoring || job.State == SourceRestored {
		doc, err := s.sourceRestoreDocument(ctx, job)
		if err != nil {
			return job, err
		}
		if doc == nil {
			return job, ErrState
		}
		job.SourceRestore = &doc.Receipt
		return job, nil
	}
	if job.State == "cancelled" || job.State == "abandoning" || job.State == "abandoned" {
		return job, nil
	}
	job.State = "completed"
	for _, item := range job.Items {
		if item.State == "planned" {
			job.State = "ready"
		}
	}
	for _, item := range job.Items {
		if item.State != "planned" && item.State != "completed" || item.State == "completed" && item.Reason != "" {
			job.State = "needs_review"
		}
	}
	if job.State == "completed" && job.UnknownState == "pending" {
		job.State = "needs_review"
	}
	return job, nil
}

func (s *Store) List(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ID FROM GO_ORGANIZATION_JOBS ORDER BY CREATED DESC LIMIT 30`)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	jobs := []Job{}
	for _, id := range ids {
		job, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (s *Store) Claim(ctx context.Context, id string, index int) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='running' WHERE JOB_ID=? AND ORDINAL=? AND STATE='planned' AND EXISTS (SELECT 1 FROM GO_ORGANIZATION_JOBS WHERE ID=? AND STATE='active')`, id, index, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) Prepare(ctx context.Context, id string, index int, p Proof) error {
	if p.Incomplete {
		return ErrState
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, previous string
	if err = tx.QueryRowContext(ctx, `SELECT I.STATE,I.PROOF FROM GO_ORGANIZATION_ITEMS I JOIN GO_ORGANIZATION_JOBS J ON J.ID=I.JOB_ID WHERE I.JOB_ID=? AND I.ORDINAL=? AND J.STATE='active'`, id, index).Scan(&state, &previous); err != nil {
		return err
	}
	if state != "running" && state != "staging" {
		return ErrState
	}
	if state == "staging" {
		var saved Proof
		if json.Unmarshal([]byte(previous), &saved) != nil || !saved.Incomplete {
			return ErrState
		}
		saved.Incomplete = false
		if Digest(saved) != Digest(p) {
			return ErrState
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='prepared',PROOF=? WHERE JOB_ID=? AND ORDINAL=? AND STATE=?`, string(raw), id, index, state)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Save the exact newly created object and FULL original-source digest before
// copying any bytes. This is not complete/publication proof or a retry lease.
func (s *Store) Stage(ctx context.Context, id string, index int, p Proof) error {
	if !p.Incomplete || !p.Anchor || p.Temp != TempName(id, index) || p.Identity == "" || p.ParentIdentity == "" || p.StagingIdentity == "" || len(p.Digest) != 64 || p.SourceHold != "" || p.SourceHoldIdentity != "" || p.SourceParentIdentity != "" {
		return ErrState
	}
	if _, err := hex.DecodeString(p.Digest); err != nil {
		return ErrState
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='staging',PROOF=? WHERE JOB_ID=? AND ORDINAL=? AND STATE='running' AND EXISTS (SELECT 1 FROM GO_ORGANIZATION_JOBS WHERE ID=? AND STATE='active')`, string(raw), id, index, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	return err
}

// Read-only publication permission; this is not a lease, timeout, or reset.
// Concurrent publication is bounded by atomic no-overwrite filesystem linking.
func (s *Store) VerifyPrepared(ctx context.Context, id string, index int, p Proof) error {
	if p.Incomplete {
		return ErrState
	}
	var state, raw string
	if err := s.db.QueryRowContext(ctx, `SELECT I.STATE,I.PROOF FROM GO_ORGANIZATION_ITEMS I JOIN GO_ORGANIZATION_JOBS J ON J.ID=I.JOB_ID WHERE I.JOB_ID=? AND I.ORDINAL=? AND J.STATE='active'`, id, index).Scan(&state, &raw); err != nil {
		return err
	}
	var saved Proof
	if state != "prepared" || json.Unmarshal([]byte(raw), &saved) != nil || Digest(saved) != Digest(p) {
		return ErrState
	}
	return nil
}

func (s *Store) RecordError(ctx context.Context, id string, index int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET REASON='Execution interrupted or failed; verify the saved proof before any retry' WHERE JOB_ID=? AND ORDINAL=? AND STATE IN ('running','staging','prepared','moving','quarantined')`, id, index)
	return err
}

func (s *Store) Cancel(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND STATE!='planned'`, id).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrState
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_JOBS SET STATE='cancelled' WHERE ID=? AND STATE='active'`, id)
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
	if _, err = tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='cancelled' WHERE JOB_ID=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM GO_ORGANIZATION_TARGETS WHERE JOB_ID=?`, id); err != nil {
		return err
	}
	if err := releaseDiscTargets(ctx, tx, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE='cancelled' WHERE JOB_ID=? AND STATE='pending'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Publication and the database cannot be one transaction. The prepared proof
// remains reserved across crashes; Complete atomically writes state + history.
func (s *Store) Complete(ctx context.Context, job Job, item JobItem) error {
	if item.Proof.Incomplete {
		return ErrState
	}
	if !SupportedMode(job.Definition.Mode) {
		return ErrMode
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, rawProof string
	var jobState string
	if err = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job.ID).Scan(&jobState); err != nil {
		return err
	}
	if jobState != "active" {
		return ErrState
	}
	if err := verifySyncRecord(ctx, tx, job.Definition); err != nil {
		return err
	}
	if err := verifyDownloadDefinition(ctx, tx, job.Definition, false); err != nil {
		return err
	}
	if job.AutomaticGeneration != "" {
		if err := verifyMonitorControl(ctx, tx, job.AutomaticGeneration); err != nil {
			return err
		}
	}
	if job.DownloadGeneration != "" {
		if err := verifyDownloadMonitor(ctx, tx, job.DownloadGeneration); err != nil {
			return err
		}
	}
	if job.Definition.History != nil {
		current, err := readHistory(ctx, tx, job.Definition.History.ID)
		if err != nil {
			return err
		}
		if Digest(current) != Digest(*job.Definition.History) {
			return ErrState
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT STATE,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, job.ID, item.Index).Scan(&state, &rawProof); err != nil {
		return err
	}
	if state == "completed" {
		return nil
	}
	if job.Definition.Mode == "move" && state != "quarantined" || job.Definition.Mode != "move" && state != "prepared" {
		return ErrState
	}
	var storedProof Proof
	if err = json.Unmarshal([]byte(rawProof), &storedProof); err != nil || Digest(storedProof) != Digest(item.Proof) || item.Proof.Identity == "" {
		return ErrState
	}
	if job.Definition.Mode == "move" && (storedProof.SourceHold == "" || storedProof.SourceHoldIdentity == "" || storedProof.SourceParentIdentity == "") {
		return ErrState
	}
	if job.Definition.Sync != nil && job.Definition.Sync.Record.Rename == 0 {
		exists, err := syncRecorded(ctx, tx, job.Definition, item.Source)
		if err != nil {
			return err
		}
		if exists {
			return ErrState
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS SYNC_HISTORY(ID INTEGER PRIMARY KEY,PATH TEXT,SRC TEXT,DEST TEXT)`); err != nil {
			return err
		}
		r := job.Definition.Sync.Record
		if _, err := tx.ExecContext(ctx, `INSERT INTO SYNC_HISTORY(PATH,SRC,DEST) VALUES(?,?,?)`, filepath.Join(r.Source, item.Source), filepath.Clean(r.Source), filepath.Clean(r.Dest)); err != nil {
			return err
		}
	} else if item.Kind == "media" {
		if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS TRANSFER_HISTORY (ID INTEGER PRIMARY KEY, MODE TEXT, TYPE TEXT, CATEGORY TEXT, TMDBID INTEGER, TITLE TEXT, YEAR TEXT, SEASON_EPISODE TEXT, SOURCE TEXT, SOURCE_PATH TEXT, SOURCE_FILENAME TEXT, DEST TEXT, DEST_PATH TEXT, DEST_FILENAME TEXT, DATE TEXT)`); err != nil {
			return err
		}
		source, target := filepath.Join(job.Definition.SourceRoot, item.Source), filepath.Join(job.Definition.TargetRoot, item.Target)
		origin := "手动整理"
		if job.Definition.Download != nil {
			origin = "下载完成整理"
		}
		if job.Definition.Sync != nil {
			origin = "目录同步"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO TRANSFER_HISTORY (MODE,TYPE,CATEGORY,TMDBID,TITLE,YEAR,SEASON_EPISODE,SOURCE,SOURCE_PATH,SOURCE_FILENAME,DEST,DEST_PATH,DEST_FILENAME,DATE) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, ModeLabel(job.Definition.Mode), item.MediaType, item.Category, item.TMDBID, item.Title, item.Year, item.SeasonEpisode, origin, filepath.Dir(source), filepath.Base(source), job.Definition.TargetRoot, filepath.Dir(target), filepath.Base(target), time.Now().Format("2006-01-02 15:04:05")); err != nil {
			return err
		}
	}
	reason := ""
	if job.Definition.Mode == "move" {
		reason = MoveSourceCleanupPending
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='completed',REASON=? WHERE JOB_ID=? AND ORDINAL=?`, reason, job.ID, item.Index); err != nil {
		return err
	}
	if err := emitOrganizationEvent(ctx, tx, job.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// BeginMove saves source disposition intent only when its target-copy proof
// matches the existing prepared ledger. No mode conversion or automatic retry.
func (s *Store) BeginMove(ctx context.Context, id string, index int, p Proof) error {
	if p.SourceHold == "" || p.SourceHoldIdentity == "" || p.SourceParentIdentity == "" {
		return ErrState
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	original := p
	original.SourceHold, original.SourceHoldIdentity, original.SourceParentIdentity = "", "", ""
	previous, err := json.Marshal(original)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='moving',PROOF=? WHERE JOB_ID=? AND ORDINAL=? AND STATE='prepared' AND PROOF=? AND EXISTS (SELECT 1 FROM GO_ORGANIZATION_JOBS WHERE ID=? AND STATE='active' AND json_extract(DEFINITION,'$.mode')='move')`, string(raw), id, index, string(previous), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	return err
}

// MarkQuarantined must be called by ContinueMove after positive source capture
// verification. It cannot infer a completed move from a missing source name.
func (s *Store) MarkQuarantined(ctx context.Context, id string, index int, p Proof) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET STATE='quarantined' WHERE JOB_ID=? AND ORDINAL=? AND STATE='moving' AND PROOF=?`, id, index, string(raw))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	return err
}

// Cleanup receipts are saved separately from history: a crash after commit
// cannot hide leftover source data, or repeat source disposition after cleanup.
func (s *Store) AdvanceMoveCleanup(ctx context.Context, id string, index int, p Proof, sourceCleaned bool) error {
	from, to := MoveSourceCleanupPending, MoveTargetCleanupPending
	if !sourceCleaned {
		from, to = MoveTargetCleanupPending, ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_ITEMS SET REASON=? WHERE JOB_ID=? AND ORDINAL=? AND STATE='completed' AND REASON=? AND PROOF=? AND EXISTS (SELECT 1 FROM GO_ORGANIZATION_JOBS WHERE ID=? AND STATE='active' AND json_extract(DEFINITION,'$.mode')='move')`, to, id, index, from, string(raw), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrState
	}
	if err != nil {
		return err
	}
	if err := emitOrganizationEvent(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}
