package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type LibraryJob struct {
	Remote     *remotestore.Summary `json:"remote,omitempty"`
	ID         string               `json:"id"`
	State      string               `json:"state"`
	Mode       string               `json:"mode"`
	RootID     string               `json:"rootId"`
	Path       string               `json:"path"`
	Created    string               `json:"created"`
	Updated    string               `json:"updated"`
	Reason     string               `json:"reason"`
	Total      int                  `json:"total"`
	Done       int                  `json:"done"`
	Blocked    int                  `json:"blocked"`
	Definition LibraryDefinition    `json:"-"`
}
type LibraryItem struct {
	Disc    *DiscInfo    `json:"disc,omitempty"`
	Ordinal int          `json:"ordinal"`
	Path    string       `json:"path"`
	State   string       `json:"state"`
	Reason  string       `json:"reason"`
	Media   LibraryMedia `json:"-"`
}
type LibraryOutput struct {
	Remote *remotestore.Summary `json:"remote,omitempty"`
	ID     string               `json:"id"`
	JobID  string               `json:"jobId"`
	Target string               `json:"target"`
	State  string               `json:"state"`
	Reason string               `json:"reason"`
	Owner  string               `json:"-"`
	Proof  LibraryOutputProof   `json:"-"`
}

func LibraryOutputDigest(out LibraryOutput) string {
	return Digest([]any{out.ID, out.JobID, out.Target, out.Owner, out.State, out.Proof, out.Reason})
}

// Physical parents bind aliases and overlapping roots to the same uncertain
// claim. Folding may conservatively block case-distinct Linux filenames but
// must not let an alias bypass a claim on a case-insensitive mount.
func libraryTargetClaim(target string, proof LibraryOutputProof) string {
	return Digest([]string{proof.ParentIdentity, cases.Fold().String(norm.NFD.String(filepath.Base(target)))})
}

func librarySchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_LIBRARY_SCRAPE_JOBS(ID TEXT PRIMARY KEY,REVIEW_KEY TEXT NOT NULL UNIQUE,DEFINITION TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS GO_LIBRARY_SCRAPE_ITEMS(JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,PATH TEXT NOT NULL,MEDIA TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',PRIMARY KEY(JOB_ID,ORDINAL))`,
		`CREATE TABLE IF NOT EXISTS GO_LIBRARY_SCRAPE_OUTPUTS(ID TEXT PRIMARY KEY,JOB_ID TEXT NOT NULL,TARGET TEXT NOT NULL,OWNER TEXT NOT NULL,STATE TEXT NOT NULL,PROOF TEXT NOT NULL,REASON TEXT NOT NULL,UNIQUE(JOB_ID,TARGET))`,
		`CREATE TABLE IF NOT EXISTS GO_LIBRARY_SCRAPE_CLAIMS(TARGET_KEY TEXT PRIMARY KEY,OUTPUT_ID TEXT NOT NULL)`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func libraryExists(ctx context.Context, db *sql.DB) (bool, error) {
	var b bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_LIBRARY_SCRAPE_JOBS')`).Scan(&b)
	return b, err
}
func (s *Store) CreateLibraryJob(ctx context.Context, fingerprint string, d LibraryDefinition, files []LibraryMedia) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	id, err := createLibraryJob(ctx, tx, fingerprint, d, files)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}
func createLibraryJob(ctx context.Context, tx *sql.Tx, fingerprint string, d LibraryDefinition, files []LibraryMedia) (string, error) {
	locations := []string{}
	for _, m := range files {
		protected, err := libraryProtectedLocations(d, m)
		if err != nil {
			return "", err
		}
		locations = append(locations, protected...)
	}
	if err := mediaRemovalClaimed(ctx, tx, locations, nil, ""); err != nil {
		return "", err
	}
	if d.Remote != nil && !LibraryRemoteApproved(d) {
		return "", ErrState
	}
	if len(fingerprint) != 64 || len(files) == 0 || len(files) > 50_000 || d.Count != len(files) || !LibraryMode(d.Mode) || d.Actor == "" || len(d.RequestID) != 32 {
		return "", ErrState
	}
	definition, err := json.Marshal(d)
	if err != nil || len(definition) > 1<<20 {
		return "", ErrState
	}
	if err := librarySchema(ctx, tx); err != nil {
		return "", err
	}
	key := fingerprint
	id := Digest([]string{d.Actor, d.RequestID})
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT REVIEW_KEY FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&existing)
	if err == nil {
		if existing != key {
			return "", ErrState
		}
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_LIBRARY_SCRAPE_JOBS(ID,REVIEW_KEY,DEFINITION,STATE,CREATED,UPDATED) VALUES(?,?,?,'ready',?,?)`, id, key, string(definition), now, now); err != nil {
		return "", err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO GO_LIBRARY_SCRAPE_ITEMS(JOB_ID,ORDINAL,PATH,MEDIA,STATE) VALUES(?,?,?,?,'pending')`)
	if err != nil {
		return "", err
	}
	defer stmt.Close()
	worklistBytes := 0
	for i, m := range files {
		if d.Remote != nil && m.Blocked == "" && (m.Remote == nil || d.OpenSubtitle == nil && m.Remote.Digest != "" || d.OpenSubtitle != nil && !LibraryRemotePrepared(m)) {
			return "", ErrState
		}
		if m.Disc != nil {
			if err := libraryDiscDefinition(d, m); err != nil {
				return "", err
			}
		}
		data, err := libraryMediaDocument(m)
		if err != nil {
			return "", err
		}
		worklistBytes += len(data)
		if worklistBytes > LibraryWorklistBytes {
			return "", ErrLimit
		}
		if _, err := stmt.ExecContext(ctx, id, i, m.Path, string(data)); err != nil {
			return "", err
		}
		var savedPath, savedMedia, savedState string
		if err := tx.QueryRowContext(ctx, `SELECT PATH,MEDIA,STATE FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, id, i).Scan(&savedPath, &savedMedia, &savedState); err != nil || savedPath != m.Path || savedMedia != string(data) || savedState != "pending" {
			return "", ErrState
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=?`, id).Scan(&count); err != nil || count != len(files) {
		return "", ErrState
	}
	var savedDefinition, savedState string
	if err := tx.QueryRowContext(ctx, `SELECT DEFINITION,STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&savedDefinition, &savedState); err != nil || savedDefinition != string(definition) || savedState != "ready" {
		return "", ErrState
	}
	return id, nil
}

func (s *Store) ExistingLibraryRequest(ctx context.Context, actor, request, fingerprint string) (LibraryJob, error) {
	exists, err := libraryExists(ctx, s.db)
	if err != nil {
		return LibraryJob{}, err
	}
	if !exists {
		return LibraryJob{}, ErrNotFound
	}
	id := Digest([]string{actor, request})
	var key string
	err = s.db.QueryRowContext(ctx, `SELECT REVIEW_KEY FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return LibraryJob{}, ErrNotFound
	}
	if err != nil {
		return LibraryJob{}, err
	}
	if key != fingerprint {
		return LibraryJob{}, ErrState
	}
	return s.LibraryJob(ctx, id)
}
func (s *Store) LibraryJob(ctx context.Context, id string) (LibraryJob, error) {
	j := LibraryJob{ID: id}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT DEFINITION,STATE,CREATED,UPDATED,REASON FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&raw, &j.State, &j.Created, &j.Updated, &j.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	if json.Unmarshal([]byte(raw), &j.Definition) != nil {
		return j, ErrState
	}
	j.Mode = j.Definition.Mode
	if j.Definition.Remote != nil {
		j.Remote = j.Definition.Remote.Summary()
	}
	j.RootID, j.Path = j.Definition.Root.ID, j.Definition.Selection
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(STATE='completed'),0),COALESCE(SUM(STATE IN('partial','blocked')),0) FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=?`, id).Scan(&j.Total, &j.Done, &j.Blocked)
	return j, err
}
func (s *Store) LibraryJobs(ctx context.Context, ready bool) ([]LibraryJob, error) {
	result := []LibraryJob{}
	exists, err := libraryExists(ctx, s.db)
	if err != nil || !exists {
		return result, err
	}
	query := `SELECT ID FROM GO_LIBRARY_SCRAPE_JOBS WHERE json_type(DEFINITION,'$.openSubtitle') IS NULL ORDER BY STATE NOT IN('completed','cancelled') DESC,UPDATED DESC LIMIT 50`
	if ready {
		query = `SELECT ID FROM GO_LIBRARY_SCRAPE_JOBS WHERE STATE='ready' AND json_type(DEFINITION,'$.openSubtitle') IS NULL ORDER BY CREATED LIMIT 2`
	}
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
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
	for _, id := range ids {
		j, err := s.LibraryJob(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, j)
	}
	return result, nil
}
func (s *Store) LibraryItems(ctx context.Context, id string, page int) ([]LibraryItem, error) {
	result := []LibraryItem{}
	if page < 1 || page > 1000 {
		return nil, ErrLimit
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ORDINAL,PATH,MEDIA,STATE,REASON FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? ORDER BY ORDINAL LIMIT 100 OFFSET ?`, id, (page-1)*100)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r LibraryItem
		var raw string
		if err := rows.Scan(&r.Ordinal, &r.Path, &raw, &r.State, &r.Reason); err != nil {
			return nil, err
		}
		if len(raw) > libraryDiscDocument || json.Unmarshal([]byte(raw), &r.Media) != nil {
			return nil, ErrState
		}
		if r.Media.Path != r.Path {
			return nil, ErrState
		}
		if r.Media.Disc != nil {
			if err := libraryDiscInventory(r.Media); err != nil {
				return nil, err
			}
		} else if len(raw) > 64<<10 {
			return nil, ErrLimit
		}
		r.Disc = LibraryDiscSummary(r.Media)
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) ClaimLibraryJob(ctx context.Context, id string) error {
	return s.libraryChange(ctx, id, "ready", "running", "")
}
func (s *Store) libraryChange(ctx context.Context, id, from, to, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_JOBS SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE=?`, to, reason, time.Now().UTC().Format(time.RFC3339Nano), id, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&state); err != nil || state != to {
		return ErrState
	}
	if err := finishLibraryEvent(ctx, tx, id, to, reason); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CheckLibraryRunning(ctx context.Context, id string) error {
	var state string
	if err := s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, id).Scan(&state); err != nil {
		return err
	}
	if state != "running" {
		return ErrState
	}
	return nil
}
func (s *Store) CancelLibraryJob(ctx context.Context, id string) error {
	j, err := s.LibraryJob(ctx, id)
	if err != nil {
		return err
	}
	if j.State == "cancelled" {
		return nil
	}
	if j.State != "ready" && j.State != "running" && j.State != "needs_review" {
		return ErrState
	}
	return s.libraryChange(ctx, id, j.State, "cancelled", "stop requested; existing metadata, backups and uncertain receipts retained")
}
func (s *Store) FinishLibraryJob(ctx context.Context, id, state, reason string) error {
	if state != "completed" && state != "partial" && state != "needs_review" {
		return ErrState
	}
	return s.libraryChange(ctx, id, "running", state, reason)
}
func (s *Store) LibraryItemState(ctx context.Context, id string, ordinal int, from, to, reason string) error {
	if len(reason) > 2000 {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_ITEMS SET STATE=?,REASON=? WHERE JOB_ID=? AND ORDINAL=? AND STATE=? AND EXISTS(SELECT 1 FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=? AND STATE='running')`, to, reason, id, ordinal, from, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, id, ordinal).Scan(&stored); err != nil || stored != to {
		return ErrState
	}
	return tx.Commit()
}
func scanLibraryOutput(row interface{ Scan(...any) error }) (LibraryOutput, error) {
	var r LibraryOutput
	var raw string
	err := row.Scan(&r.ID, &r.JobID, &r.Target, &r.Owner, &r.State, &raw, &r.Reason)
	if err == nil && json.Unmarshal([]byte(raw), &r.Proof) != nil {
		err = ErrState
	}
	if err == nil && r.Proof.Remote != nil {
		r.Remote = (&remotestore.Binding{Driver: r.Proof.Remote.Driver}).Summary()
	}
	return r, err
}

const libraryOutputSelect = `SELECT ID,JOB_ID,TARGET,OWNER,STATE,PROOF,REASON FROM GO_LIBRARY_SCRAPE_OUTPUTS`

func (s *Store) LibraryOutput(ctx context.Context, id string) (LibraryOutput, error) {
	return scanLibraryOutput(s.db.QueryRowContext(ctx, libraryOutputSelect+` WHERE ID=?`, id))
}
func (s *Store) LibraryOutputs(ctx context.Context, job string, page int) ([]LibraryOutput, error) {
	result := []LibraryOutput{}
	if page < 1 || page > 10000 {
		return nil, ErrLimit
	}
	rows, err := s.db.QueryContext(ctx, libraryOutputSelect+` WHERE JOB_ID=? ORDER BY TARGET LIMIT 100 OFFSET ?`, job, (page-1)*100)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanLibraryOutput(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) FindLibraryOutput(ctx context.Context, job, target string) (LibraryOutput, error) {
	return scanLibraryOutput(s.db.QueryRowContext(ctx, libraryOutputSelect+` WHERE JOB_ID=? AND TARGET=?`, job, target))
}
func (s *Store) SaveLibraryOutput(ctx context.Context, job string, root LibraryRoot, target, owner, state, reason string, proof LibraryOutputProof, validators ...func(*sql.Tx) error) error {
	if !validRelative(target) || len(owner) != 64 || len(reason) > 2000 {
		return ErrState
	}
	switch state {
	case "writing", "created", "replaced", "preserved", "blocked", "uncertain":
	default:
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(validators) > 1 {
		return ErrState
	}
	if len(validators) == 1 && (validators[0] == nil || validators[0](tx) != nil) {
		return ErrState
	}
	var js, definition string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,DEFINITION FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, job).Scan(&js, &definition); err != nil {
		return err
	}
	if js == RecoveryClosed {
		return ErrState
	}
	if proof.Remote != nil {
		var d LibraryDefinition
		if json.Unmarshal([]byte(definition), &d) != nil {
			return ErrState
		}
		sameRoot := d.Root.ID == root.ID && d.Root.Path == root.Path && d.Root.Identity == root.Identity && (d.Root.Kind == root.Kind || d.Root.Kind == "auto" && (root.Kind == "movie" || root.Kind == "tv"))
		if !sameRoot || !LibraryRemoteApproved(d) || !libraryRemoteOutputValid(d, proof) {
			return ErrState
		}
	}
	if js != "running" && state == "writing" {
		return ErrState
	}
	id := Digest([]string{job, target})
	if state == "writing" && proof.Remote != nil {
		if err := checkRemoteMetadataClaims(ctx, tx, root.Path, target, id, ""); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	claimKey := libraryTargetClaim(target, proof)
	old, err := scanLibraryOutput(tx.QueryRowContext(ctx, libraryOutputSelect+` WHERE ID=?`, id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		if old.Owner != owner || old.State != "writing" || state == "writing" {
			return ErrState
		}
		if Digest(old.Proof) != Digest(proof) {
			return ErrState
		}
		var holder string
		if err := tx.QueryRowContext(ctx, `SELECT OUTPUT_ID FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=?`, claimKey).Scan(&holder); err != nil || holder != id {
			return ErrClaimed
		}
		res, e := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_OUTPUTS SET STATE=?,PROOF=?,REASON=? WHERE ID=? AND STATE='writing'`, state, string(raw), reason, id)
		if e != nil {
			return e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrState
		}
	} else {
		if state == "created" || state == "replaced" || state == "uncertain" {
			return ErrState
		}
		if _, e := tx.ExecContext(ctx, `INSERT INTO GO_LIBRARY_SCRAPE_OUTPUTS(ID,JOB_ID,TARGET,OWNER,STATE,PROOF,REASON) VALUES(?,?,?,?,?,?,?)`, id, job, target, owner, state, string(raw), reason); e != nil {
			return e
		}
	}
	if state == "writing" {
		if proof.Target != target || proof.NewIdentity == "" || proof.ParentIdentity == "" || proof.StageIdentity == "" || len(proof.NewDigest) != 64 || proof.NewSize < 1 {
			return ErrState
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_LIBRARY_SCRAPE_CLAIMS(TARGET_KEY,OUTPUT_ID) VALUES(?,?)`, claimKey, id); err != nil {
			return ErrClaimed
		}
		var holder string
		if err := tx.QueryRowContext(ctx, `SELECT OUTPUT_ID FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=?`, claimKey).Scan(&holder); err != nil || holder != id {
			return ErrClaimed
		}
	} else if state != "uncertain" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=? AND OUTPUT_ID=?`, claimKey, id); err != nil {
			return err
		}
	}
	var savedState, savedProof string
	if err := tx.QueryRowContext(ctx, `SELECT STATE,PROOF FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE ID=?`, id).Scan(&savedState, &savedProof); err != nil || savedState != state || savedProof != string(raw) {
		return ErrState
	}
	saved, e := scanLibraryOutput(tx.QueryRowContext(ctx, libraryOutputSelect+` WHERE ID=?`, id))
	if e != nil || saved.ID != id || saved.JobID != job || saved.Target != target || saved.Owner != owner || saved.State != state || saved.Reason != reason || Digest(saved.Proof) != Digest(proof) {
		return ErrState
	}
	if len(validators) == 1 && validators[0](tx) != nil {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) LibraryRestoreIntent(ctx context.Context, id, expectedProof string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	out, err := scanLibraryOutput(tx.QueryRowContext(ctx, libraryOutputSelect+` WHERE ID=?`, id))
	if err != nil {
		return err
	}
	if Digest(out.Proof) != expectedProof || out.State != "replaced" && out.State != "uncertain" && out.State != "writing" && out.State != "restore_running" {
		return ErrState
	}
	if err := libraryRecoveryAuthority(ctx, tx, out.JobID); err != nil {
		return err
	}
	key := libraryTargetClaim(out.Target, out.Proof)
	if out.Proof.Remote != nil {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_LIBRARY_SCRAPE_JOBS WHERE ID=?`, out.JobID).Scan(&raw); err != nil {
			return err
		}
		var d LibraryDefinition
		if json.Unmarshal([]byte(raw), &d) != nil || !libraryRemoteOutputValid(d, out.Proof) {
			return ErrState
		}
		if err := checkRemoteMetadataClaims(ctx, tx, d.Root.Path, out.Target, id, ""); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO GO_LIBRARY_SCRAPE_CLAIMS(TARGET_KEY,OUTPUT_ID) VALUES(?,?)`, key, id); err != nil {
		return err
	}
	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT OUTPUT_ID FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=?`, key).Scan(&owner); err != nil || owner != id {
		return ErrClaimed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_OUTPUTS SET STATE='restore_running' WHERE ID=?`, id); err != nil {
		return err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE ID=?`, id).Scan(&state); err != nil || state != "restore_running" {
		return ErrState
	}
	return tx.Commit()
}

// Reconciliation only closes an existing intent after filesystem evidence has
// been checked by the caller under the same cross-process publication lock.
// It never retries a write or moves a file.
func (s *Store) ReconcileLibraryOutput(ctx context.Context, id, expected, state string, root LibraryRoot) error {
	if state != "created" && state != "replaced" && state != "not_published" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	out, err := scanLibraryOutput(tx.QueryRowContext(ctx, libraryOutputSelect+` WHERE ID=?`, id))
	if err != nil || LibraryOutputDigest(out) != expected || out.State != "writing" && out.State != "uncertain" {
		return ErrState
	}
	if err := libraryRecoveryAuthority(ctx, tx, out.JobID); err != nil {
		return err
	}
	key := libraryTargetClaim(out.Target, out.Proof)
	var holder string
	if err := tx.QueryRowContext(ctx, `SELECT OUTPUT_ID FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=?`, key).Scan(&holder); err != nil || holder != id {
		return ErrClaimed
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_OUTPUTS SET STATE=?,REASON='readonly reconciliation; no filesystem action replayed' WHERE ID=?`, state, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE TARGET_KEY=? AND OUTPUT_ID=?`, key, id); err != nil {
		return err
	}
	var saved string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE ID=?`, id).Scan(&saved); err != nil || saved != state {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) FinishLibraryRestore(ctx context.Context, id string, root LibraryRoot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	out, err := scanLibraryOutput(tx.QueryRowContext(ctx, libraryOutputSelect+` WHERE ID=?`, id))
	if err != nil || out.State != "restore_running" {
		return ErrState
	}
	if err := libraryRecoveryAuthority(ctx, tx, out.JobID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE GO_LIBRARY_SCRAPE_OUTPUTS SET STATE='restored',REASON='original metadata restored; replacement retained in private backup' WHERE ID=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE OUTPUT_ID=?`, id); err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_LIBRARY_SCRAPE_CLAIMS WHERE OUTPUT_ID=?`, id).Scan(&remaining); err != nil || remaining != 0 {
		return ErrState
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_LIBRARY_SCRAPE_OUTPUTS WHERE ID=?`, id).Scan(&state); err != nil || state != "restored" {
		return ErrState
	}
	return tx.Commit()
}
