package downloaderconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

type VerificationActions struct {
	Stop   bool `json:"stop"`
	Remove bool `json:"remove"`
	Move   bool `json:"move"`
}

func (a VerificationActions) Allows(action string) bool {
	switch action {
	case "stop":
		return a.Stop
	case "remove":
		return a.Remove
	case "move":
		return a.Move
	}
	return false
}
func (a VerificationActions) Valid() bool { return a.Stop || a.Remove || a.Move }

type PTVerification struct {
	ID           string              `json:"id"`
	Kind         string              `json:"kind"`
	DownloaderID int64               `json:"downloaderId"`
	InfoHash     string              `json:"infoHash"`
	ConfigDigest string              `json:"-"`
	PolicyDigest string              `json:"policyDigest"`
	PolicyIDs    []string            `json:"policyIds"`
	Actor        string              `json:"actor"`
	Note         string              `json:"note"`
	Actions      VerificationActions `json:"actions"`
	Observed     seeding.Evidence    `json:"observed"`
	Created      string              `json:"created"`
	Expires      string              `json:"expires"`
}
type PTVerificationState struct {
	Record    *PTVerification `json:"record"`
	Revoked   bool            `json:"revoked"`
	RevokedBy string          `json:"revokedBy,omitempty"`
	RevokedAt string          `json:"revokedAt,omitempty"`
}

func VerificationNoteValid(note string) bool {
	return utf8.RuneCountInString(note) >= 10 && utf8.RuneCountInString(note) <= 512 && !strings.Contains(note, "://") && !strings.ContainsAny(note, "?=&") && !strings.ContainsFunc(note, unicode.IsControl)
}
func verificationID(v PTVerification) string {
	v.ID = ""
	return bus.Hash(struct {
		Value  PTVerification
		Config string
	}{v, v.ConfigDigest})
}
func (v PTVerification) Valid() bool {
	created, e1 := time.Parse(time.RFC3339Nano, v.Created)
	expires, e2 := time.Parse(time.RFC3339Nano, v.Expires)
	if v.DownloaderID < 1 || v.Kind != "operator-attestation" || !seeding.ValidHash(v.InfoHash) || v.InfoHash != strings.ToLower(v.InfoHash) || !validSeedingDigest(v.ConfigDigest) || !validSeedingDigest(v.PolicyDigest) || len(v.PolicyIDs) < 1 || len(v.PolicyIDs) > 100 || v.Actor == "" || len(v.Actor) > 256 || !VerificationNoteValid(v.Note) || !v.Actions.Valid() || e1 != nil || e2 != nil || expires.Sub(created) < time.Minute || expires.Sub(created) > 30*time.Minute {
		return false
	}
	for i, id := range v.PolicyIDs {
		if !validSeedingDigest(id) || i > 0 && id <= v.PolicyIDs[i-1] {
			return false
		}
	}
	return seeding.EvaluateAttested(v.InfoHash, nil, v.Observed, v.Observed).Allowed
}
func (v PTVerification) Current(config, policies string, now time.Time) bool {
	created, e1 := time.Parse(time.RFC3339Nano, v.Created)
	expires, e2 := time.Parse(time.RFC3339Nano, v.Expires)
	return v.Valid() && e1 == nil && e2 == nil && !now.Before(created) && now.Before(expires) && v.ConfigDigest == config && v.PolicyDigest == policies
}
func readPTVerification(ctx context.Context, q seedingQuerier, id int64, hash string) (PTVerificationState, error) {
	state := PTVerificationState{}
	rows, err := q.QueryContext(ctx, `SELECT v.ID,v.DOCUMENT,v.CONFIG_DIGEST,COALESCE(r.ACTOR,''),COALESCE(r.CREATED,''),r.VERIFICATION_ID IS NOT NULL FROM GO_PT_VERIFICATION_SCOPES s LEFT JOIN GO_PT_VERIFICATIONS v ON v.ID=s.VERIFICATION_ID LEFT JOIN GO_PT_VERIFICATION_REVOKES r ON r.VERIFICATION_ID=v.ID WHERE s.SCOPE=?`, profileScope(id, hash))
	if err != nil {
		return state, err
	}
	defer rows.Close()
	if !rows.Next() {
		return state, rows.Err()
	}
	var v PTVerification
	var key, raw string
	if err := rows.Scan(&key, &raw, &v.ConfigDigest, &state.RevokedBy, &state.RevokedAt, &state.Revoked); err != nil {
		return state, err
	}
	if json.Unmarshal([]byte(raw), &v) != nil || v.ID != key || v.ID != verificationID(v) || !v.Valid() || v.DownloaderID != id || v.InfoHash != strings.ToLower(hash) {
		return state, ErrSeedingPolicy
	}
	state.Record = &v
	return state, rows.Err()
}
func (s *Store) PTVerification(ctx context.Context, id int64, hash string) (PTVerificationState, error) {
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_PT_VERIFICATION_SCOPES' AND type='table')`).Scan(&exists); err != nil || !exists {
		return PTVerificationState{}, err
	}
	return readPTVerification(ctx, s.database, id, hash)
}
func verificationSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_PT_VERIFICATIONS(ID TEXT PRIMARY KEY,DOWNLOADER_ID INTEGER NOT NULL,INFO_HASH TEXT NOT NULL,DOCUMENT TEXT NOT NULL,CONFIG_DIGEST TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_PT_VERIFICATION_SCOPES(SCOPE TEXT PRIMARY KEY,VERIFICATION_ID TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_PT_VERIFICATION_REVOKES(VERIFICATION_ID TEXT PRIMARY KEY,ACTOR TEXT NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// Caller holds the same cross-process task guard as all consuming operations.
func (s *Store) RecordPTVerification(ctx context.Context, v PTVerification, minutes int, expected string) (PTVerification, error) {
	if minutes < 1 || minutes > 30 {
		return v, ErrSeedingPolicy
	}
	now := time.Now().UTC()
	v.Created = now.Format(time.RFC3339Nano)
	v.Expires = now.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339Nano)
	v.InfoHash = strings.ToLower(v.InfoHash)
	v.Kind = "operator-attestation"
	if !v.Valid() {
		return v, ErrSeedingPolicy
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	if err := verificationSchema(ctx, tx); err != nil {
		return v, err
	}
	old, err := readPTVerification(ctx, tx, v.DownloaderID, v.InfoHash)
	if err != nil || bus.Hash(old) != expected {
		return v, ErrSeedingPolicy
	}
	item, err := manualDownloader(ctx, tx, v.DownloaderID)
	if err != nil || item.Enabled == 0 || bus.Hash(item) != v.ConfigDigest || item.Type != "qbittorrent" && item.Type != "transmission" {
		return v, ErrSeedingPolicy
	}
	policies, digest, err := readSeedingPolicies(ctx, tx, v.DownloaderID, v.InfoHash)
	if err != nil || digest != v.PolicyDigest || len(policies) != len(v.PolicyIDs) {
		return v, ErrSeedingPolicy
	}
	rules := []seeding.Requirements{}
	for i, p := range policies {
		if p.ID != v.PolicyIDs[i] || p.ConfigDigest != v.ConfigDigest {
			return v, ErrSeedingPolicy
		}
		rules = append(rules, p.Requirements)
	}
	if !seeding.EvaluateAttested(v.InfoHash, rules, v.Observed, v.Observed).Allowed {
		return v, ErrSeedingPolicy
	}
	v.ID = verificationID(v)
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_PT_VERIFICATIONS VALUES(?,?,?,?,?)`, v.ID, v.DownloaderID, v.InfoHash, string(raw), v.ConfigDigest); err != nil {
		return v, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_PT_VERIFICATION_SCOPES VALUES(?,?) ON CONFLICT(SCOPE) DO UPDATE SET VERIFICATION_ID=excluded.VERIFICATION_ID`, profileScope(v.DownloaderID, v.InfoHash), v.ID); err != nil {
		return v, err
	}
	stored, err := readPTVerification(ctx, tx, v.DownloaderID, v.InfoHash)
	if err != nil || stored.Record == nil || stored.Record.ID != v.ID || stored.Revoked {
		return v, ErrSeedingPolicy
	}
	current, err := manualDownloader(ctx, tx, v.DownloaderID)
	if err != nil || bus.Hash(current) != v.ConfigDigest {
		return v, ErrSeedingPolicy
	}
	_, after, err := readSeedingPolicies(ctx, tx, v.DownloaderID, v.InfoHash)
	if err != nil || after != v.PolicyDigest {
		return v, ErrSeedingPolicy
	}
	return v, tx.Commit()
}
func (s *Store) RevokePTVerification(ctx context.Context, id int64, hash, actor, expected string) error {
	if actor == "" || len(actor) > 256 {
		return ErrSeedingPolicy
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := readPTVerification(ctx, tx, id, hash)
	if err != nil || state.Record == nil || state.Revoked || bus.Hash(state) != expected {
		return ErrSeedingPolicy
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_PT_VERIFICATION_REVOKES VALUES(?,?,?)`, state.Record.ID, actor, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	stored, err := readPTVerification(ctx, tx, id, hash)
	if err != nil || stored.Record == nil || stored.Record.ID != state.Record.ID || !stored.Revoked || stored.RevokedBy != actor {
		return ErrSeedingPolicy
	}
	return tx.Commit()
}
