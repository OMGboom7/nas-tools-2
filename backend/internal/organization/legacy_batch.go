package organization

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type LegacyBatchRef struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
}
type legacyBatchDocument struct {
	Definitions []Definition
	Jobs        []string
}
type LegacyBatch struct {
	ID                string                    `json:"id"`
	Fingerprint       string                    `json:"fingerprint"`
	State             string                    `json:"state"`
	Reason            string                    `json:"reason,omitempty"`
	Actor             string                    `json:"actor"`
	Created           string                    `json:"created"`
	Jobs              []string                  `json:"jobs"`
	Mode              string                    `json:"mode"`
	SourceRoot        string                    `json:"sourceRoot"`
	Scope             string                    `json:"scope"`
	Targets           []string                  `json:"targets"`
	Origin            *LegacyOrganizationOrigin `json:"origin"`
	UnknownState      string                    `json:"unknownState,omitempty"`
	ReviewFingerprint string                    `json:"reviewFingerprint"`
	document          legacyBatchDocument
}

func legacyBatchHeader(d Definition) string {
	d.Entries = nil
	d.Discs = nil
	d.TargetID = ""
	d.TargetRoot = ""
	d.TargetIdentity = ""
	d.LegacyBatch = nil
	if d.Remote != nil {
		b := *d.Remote
		b.Directories = false
		d.Remote = &b
	}
	return Digest(d)
}
func readLegacyBatch(ctx context.Context, r unknownReader, id string) (LegacyBatch, error) {
	var b LegacyBatch
	var raw string
	err := r.QueryRowContext(ctx, `SELECT ID,FINGERPRINT,STATE,REASON,ACTOR,CREATED,DOCUMENT FROM GO_LEGACY_ORGANIZATION_BATCHES WHERE ID=?`, id).Scan(&b.ID, &b.Fingerprint, &b.State, &b.Reason, &b.Actor, &b.Created, &raw)
	if err != nil {
		return b, err
	}
	if json.Unmarshal([]byte(raw), &b.document) != nil || len(b.document.Definitions) < 2 || len(b.document.Definitions) > 128 {
		return b, ErrState
	}
	b.Jobs = append([]string{}, b.document.Jobs...)
	b.Mode = b.document.Definitions[0].Mode
	b.Origin = b.document.Definitions[0].LegacyOrigin
	b.SourceRoot = b.document.Definitions[0].SourceRoot
	b.Scope = b.document.Definitions[0].LegacyScope
	for _, d := range b.document.Definitions {
		b.Targets = append(b.Targets, d.TargetRoot)
	}
	return b, nil
}
func verifyLegacyMember(ctx context.Context, r unknownReader, d Definition, reserving bool) error {
	if d.LegacyBatch == nil {
		return nil
	}
	b, err := readLegacyBatch(ctx, r, d.LegacyBatch.ID)
	if err != nil {
		return err
	}
	i := d.LegacyBatch.Index
	if i < 0 || i >= len(b.document.Definitions) || Digest(b.document.Definitions[i]) != Digest(d) {
		return ErrState
	}
	if b.State != "ready" && b.State != "executing" && b.State != "needs_review" && b.State != "completed" && !(reserving && b.State == "reserving") {
		return ErrState
	}
	return nil
}
func (s *Store) VerifyLegacyMember(ctx context.Context, d Definition) error {
	return verifyLegacyMember(ctx, s.db, d, false)
}
func verifyLegacyJob(ctx context.Context, r unknownReader, id string) error {
	var raw string
	var d Definition
	if err := r.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&raw); err != nil {
		return err
	}
	if json.Unmarshal([]byte(raw), &d) != nil {
		return ErrState
	}
	return verifyLegacyMember(ctx, r, d, false)
}
func verifyLegacyDisposition(ctx context.Context, r unknownReader, id string) error {
	var raw string
	var d Definition
	if err := r.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&raw); err != nil {
		return err
	}
	if json.Unmarshal([]byte(raw), &d) != nil {
		return ErrState
	}
	if d.LegacyBatch == nil {
		return nil
	}
	b, err := readLegacyBatch(ctx, r, d.LegacyBatch.ID)
	if err != nil {
		return err
	}
	if b.State != "closing" {
		return ErrState
	}
	return nil
}
func legacyOwner(ctx context.Context, r unknownReader, d Definition, jobID string) (string, error) {
	if d.LegacyBatch == nil {
		return jobID, nil
	}
	b, err := readLegacyBatch(ctx, r, d.LegacyBatch.ID)
	if err != nil {
		return "", err
	}
	if len(b.Jobs) != len(b.document.Definitions) || d.LegacyBatch.Index < 0 || d.LegacyBatch.Index >= len(b.Jobs) || b.Jobs[d.LegacyBatch.Index] != jobID || Digest(b.document.Definitions[d.LegacyBatch.Index]) != Digest(d) {
		return "", ErrState
	}
	return b.Jobs[0], nil
}

// Only the batch completion/closure transaction may release its owner link.
// Individual cancellation/restoration never grants a second writer to a sibling.
func cancelUnknownLink(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE='cancelled' WHERE JOB_ID=? AND STATE='pending' AND EXISTS(SELECT 1 FROM GO_ORGANIZATION_JOBS WHERE ID=? AND json_extract(DEFINITION,'$.legacyBatch') IS NULL)`, id, id)
	return err
}
func (s *Store) CreateLegacyBatch(ctx context.Context, fp, actor string, defs []Definition) (LegacyBatch, error) {
	var empty LegacyBatch
	if len(fp) != 64 || actor == "" || len(defs) < 2 || len(defs) > 128 {
		return empty, ErrState
	}
	header := legacyBatchHeader(defs[0])
	seen := map[string]bool{}
	for _, d := range defs {
		if d.LegacyBatch != nil || d.LegacyOrigin == nil || d.LegacyOrigin.Actor != actor || d.Download != nil || d.Sync != nil || !validRelative(d.LegacyScope) || legacyBatchHeader(d) != header {
			return empty, ErrState
		}
		for _, e := range d.Entries {
			relative, err := filepath.Rel(d.LegacyScope, e.Source)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || seen[e.Source] {
				return empty, ErrState
			}
			seen[e.Source] = true
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT ID FROM GO_LEGACY_ORGANIZATION_BATCHES WHERE FINGERPRINT=? AND STATE!='closed' ORDER BY CREATED DESC LIMIT 1`, fp).Scan(&id)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return empty, err
		}
		return s.GetLegacyBatch(ctx, id)
	}
	if err != sql.ErrNoRows {
		return empty, err
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return empty, err
	}
	id = hex.EncodeToString(nonce[:])
	defs = append([]Definition{}, defs...)
	for i := range defs {
		defs[i].LegacyBatch = &LegacyBatchRef{ID: id, Index: i}
	}
	doc := legacyBatchDocument{Definitions: defs, Jobs: []string{}}
	raw, err := json.Marshal(doc)
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO GO_LEGACY_ORGANIZATION_BATCHES VALUES(?,?,?,'reserving','',?,?)`, id, string(raw), fp, actor, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return empty, err
	}
	for _, d := range defs {
		jobID, err := createJobTx(ctx, tx, Digest(d), d)
		if err != nil {
			return empty, err
		}
		doc.Jobs = append(doc.Jobs, jobID)
	}
	raw, err = json.Marshal(doc)
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_LEGACY_ORGANIZATION_BATCHES SET DOCUMENT=?,STATE='ready' WHERE ID=? AND STATE='reserving'`, string(raw), id); err != nil {
		return empty, err
	}
	if err = tx.Commit(); err != nil {
		return empty, err
	}
	return s.GetLegacyBatch(ctx, id)
}

func legacyFrame(ctx context.Context, r unknownReader, b LegacyBatch) (string, error) {
	if len(b.Jobs) != len(b.document.Definitions) {
		return "", ErrState
	}
	parts := []any{b.ID, b.State, b.Reason, b.Fingerprint, Digest(b.document)}
	for i, id := range b.Jobs {
		var raw, state string
		if err := r.QueryRowContext(ctx, `SELECT DEFINITION,STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&raw, &state); err != nil {
			return "", err
		}
		var d Definition
		if json.Unmarshal([]byte(raw), &d) != nil || Digest(d) != Digest(b.document.Definitions[i]) {
			return "", ErrState
		}
		parts = append(parts, []string{id, state, raw})
		for n := range d.Entries {
			var phase, reason, proof string
			if err := r.QueryRowContext(ctx, `SELECT STATE,REASON,PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, id, n).Scan(&phase, &reason, &proof); err != nil {
				return "", err
			}
			parts = append(parts, []string{strconv.Itoa(n), phase, reason, proof})
		}
	}
	return Digest(parts), nil
}
func (s *Store) GetLegacyBatch(ctx context.Context, id string) (LegacyBatch, error) {
	b, err := readLegacyBatch(ctx, s.db, id)
	if err != nil {
		return b, err
	}
	b.ReviewFingerprint, err = legacyFrame(ctx, s.db, b)
	if err != nil {
		return b, err
	}
	if d := b.document.Definitions[0]; d.Unknown != nil {
		if len(b.Jobs) == 0 {
			return b, ErrState
		}
		err = s.db.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_UNKNOWN_LINKS WHERE JOB_ID=? AND RECORD_ID=?`, b.Jobs[0], d.Unknown.ID).Scan(&b.UnknownState)
	}
	return b, err
}
func (s *Store) LegacyBatchDefinitions(ctx context.Context, id string) ([]Definition, error) {
	b, err := readLegacyBatch(ctx, s.db, id)
	return b.document.Definitions, err
}
func (s *Store) LegacyBatchJobs(ctx context.Context, b LegacyBatch) ([]Job, error) {
	jobs := []Job{}
	for i, id := range b.Jobs {
		j, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if i >= len(b.document.Definitions) || Digest(j.Definition) != Digest(b.document.Definitions[i]) {
			return nil, ErrState
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}
func legacyFilesDone(j Job) bool {
	if j.State == SourceRestoring || j.State == SourceRestored || j.State == "cancelled" || j.State == "abandoned" || j.State == "abandoning" || j.State == "remote_closed" || j.State == RecoveryClosed {
		return false
	}
	if len(j.Items) == 0 {
		return false
	}
	for _, item := range j.Items {
		if item.State != "completed" || item.Reason != "" {
			return false
		}
	}
	return true
}
func LegacyFilesDone(j Job) bool { return legacyFilesDone(j) }
func (s *Store) VerifyLegacyBatchScope(ctx context.Context, b LegacyBatch, jobs []Job) error {
	if len(jobs) != len(b.document.Definitions) || len(jobs) < 2 {
		return ErrState
	}
	whole := jobs[0]
	whole.Items = nil
	whole.Definition = jobs[0].Definition
	whole.Definition.Entries = nil
	whole.Definition.Discs = map[string]DiscManifest{}
	for i, j := range jobs {
		if !legacyFilesDone(j) || Digest(j.Definition) != Digest(b.document.Definitions[i]) {
			return fmt.Errorf("legacy scope member %d does not match its saved completed definition: %w", i, ErrState)
		}
		whole.Items = append(whole.Items, j.Items...)
		whole.Definition.Entries = append(whole.Definition.Entries, j.Definition.Entries...)
		if whole.Definition.Discs == nil {
			whole.Definition.Discs = map[string]DiscManifest{}
		}
		for key, m := range j.Definition.Discs {
			whole.Definition.Discs[key] = m
		}
	}
	if whole.Definition.Unknown != nil && (whole.Definition.LegacyRename == nil || !whole.Definition.LegacyRename.PreserveUnknown) {
		if err := verifyUnknownScope(ctx, whole); err != nil {
			return fmt.Errorf("legacy source tree proof: %w", err)
		}
		return nil
	}
	return nil
}
func verifyLegacyOriginal(ctx context.Context, r unknownReader, d Definition) error {
	if d.Unknown != nil {
		current, err := readUnknown(ctx, r, d.Unknown.ID)
		if err != nil {
			return err
		}
		if Digest(current) != Digest(*d.Unknown) {
			return ErrState
		}
	}
	if d.History != nil {
		current, err := readHistory(ctx, r, d.History.ID)
		if err != nil {
			return err
		}
		if Digest(current) != Digest(*d.History) {
			return ErrState
		}
	}
	return nil
}
func (s *Store) SetLegacyBatch(ctx context.Context, id, from, to, reason, frame string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := readLegacyBatch(ctx, tx, id)
	if err != nil {
		return err
	}
	if b.State != from {
		return ErrState
	}
	valid := from == "ready" && to == "executing" || from == "executing" && to == "needs_review" || from != "closed" && from != "completed" && to == "closing" || from == "closing" && to == "needs_review"
	if !valid {
		return ErrState
	}
	if frame != "" {
		actual, err := legacyFrame(ctx, tx, b)
		if err != nil {
			return err
		}
		if actual != frame {
			return ErrState
		}
	}
	if to == "executing" || from == "closing" && to == "needs_review" {
		if err := verifyLegacyOriginal(ctx, tx, b.document.Definitions[0]); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_LEGACY_ORGANIZATION_BATCHES SET STATE=?,REASON=? WHERE ID=? AND STATE=?`, to, reason, id, from); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) FinishLegacyBatch(ctx context.Context, b LegacyBatch, frame string, closeOnly bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := readLegacyBatch(ctx, tx, b.ID)
	if err != nil {
		return err
	}
	actual, err := legacyFrame(ctx, tx, current)
	if err != nil {
		return err
	}
	if actual != frame {
		return ErrState
	}
	if closeOnly && current.State != "closing" || !closeOnly && (current.State != "executing" && current.State != "needs_review" && current.State != "ready") {
		return ErrState
	}
	for i, id := range current.Jobs {
		var state string
		if err = tx.QueryRowContext(ctx, `SELECT STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&state); err != nil {
			return err
		}
		terminal := state == "cancelled" || state == "abandoned" || state == SourceRestored || state == "remote_closed" || state == RecoveryClosed
		if closeOnly && terminal {
			continue
		}
		if state != "active" {
			return ErrState
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND (STATE!='completed' OR REASON!='')`, id).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrState
		}
		for n := range current.document.Definitions[i].Entries {
			var raw string
			var p Proof
			if err = tx.QueryRowContext(ctx, `SELECT PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, id, n).Scan(&raw); err != nil {
				return err
			}
			if json.Unmarshal([]byte(raw), &p) != nil || p.Incomplete || len(p.Digest) != 64 {
				return ErrState
			}
		}
	}
	d := current.document.Definitions[0]
	resolution := "cancelled"
	state := "closed"
	if !closeOnly {
		if err := verifyLegacyOriginal(ctx, tx, d); err != nil {
			return err
		}
		resolution = "resolved"
		state = "completed"
		if d.LegacyRename != nil && d.LegacyRename.PreserveUnknown {
			resolution = "preserved"
		}
		if d.Unknown != nil && d.Unknown.State == "N" && resolution == "resolved" {
			if _, err = tx.ExecContext(ctx, `UPDATE TRANSFER_UNKNOWN SET STATE='Y' WHERE ID=? AND STATE='N'`, d.Unknown.ID); err != nil {
				return err
			}
		}
	}
	if d.Unknown != nil {
		result, err := tx.ExecContext(ctx, `UPDATE GO_ORGANIZATION_UNKNOWN_LINKS SET STATE=? WHERE JOB_ID=? AND RECORD_ID=? AND STATE='pending'`, resolution, current.Jobs[0], d.Unknown.ID)
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
	}
	if _, err = tx.ExecContext(ctx, `UPDATE GO_LEGACY_ORGANIZATION_BATCHES SET STATE=? WHERE ID=?`, state, b.ID); err != nil {
		return err
	}
	return tx.Commit()
}
