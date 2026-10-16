package downloaderconfig

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

var ErrProfilePending = errors.New("Transmission profile requires explicit review")

type TransmissionProfileDefinition struct {
	SeedingPolicyID string                  `json:"seedingPolicyId,omitempty"`
	DownloaderID    int64                   `json:"downloaderId"`
	Name            string                  `json:"name"`
	ConfigDigest    string                  `json:"configDigest"`
	InfoHash        string                  `json:"infoHash"`
	Options         transmission.AddOptions `json:"options"`
	Limits          transmission.Limits     `json:"limits"`
}

func NormalizeTransmissionProfile(d TransmissionProfileDefinition) (TransmissionProfileDefinition, error) {
	if d.SeedingPolicyID != "" && !validSeedingDigest(d.SeedingPolicyID) {
		return d, ErrProfilePending
	}
	if d.DownloaderID < 1 || len(d.ConfigDigest) != 64 || !transmission.ValidHash(d.InfoHash) || !d.Limits.Valid() || len(d.Options.DownloadDir) > 4096 || len(d.Options.Labels) > 31 {
		return d, ErrProfilePending
	}
	d.InfoHash = strings.ToLower(d.InfoHash)
	labels := []string{}
	seen := map[string]bool{}
	for _, label := range d.Options.Labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		if len(label) > 128 || strings.ContainsAny(label, "\r\n\x00") || strings.HasPrefix(label, "NASTOOL-PROFILE-") {
			return d, ErrProfilePending
		}
		if !seen[label] {
			labels = append(labels, label)
			seen[label] = true
		}
	}
	d.Options.Labels = labels
	return d, nil
}

type TransmissionProfile struct {
	Definition  TransmissionProfileDefinition `json:"-"`
	ID          string                        `json:"id"`
	Fingerprint string                        `json:"fingerprint"`
	State       string                        `json:"state"`
	Reason      string                        `json:"reason,omitempty"`
	Created     string                        `json:"created"`
	Updated     string                        `json:"updated"`
}

func (p TransmissionProfile) Marker() string { return "NASTOOL-PROFILE-" + p.ID }
func (p TransmissionProfile) Owns(s transmission.ProfileState) bool {
	if s.Hash != p.Definition.InfoHash {
		return false
	}
	if dir := p.Definition.Options.DownloadDir; dir != "" && strings.TrimRight(s.Directory, "/") != strings.TrimRight(dir, "/") {
		return false
	}
	seen := map[string]bool{}
	for _, label := range s.Labels {
		seen[label] = true
	}
	if !seen[p.Marker()] {
		return false
	}
	for _, label := range p.Definition.Options.Labels {
		if !seen[label] {
			return false
		}
	}
	return true
}
func (p TransmissionProfile) Ready(s transmission.ProfileState) bool {
	return p.Owns(s) && s.Matches(p.Definition.Limits) && (p.Definition.Options.Paused && s.Status == 0 || !p.Definition.Options.Paused && s.Status != 0)
}

const profileSelect = `SELECT ID,DOCUMENT,FINGERPRINT,STATE,REASON,CREATED,UPDATED FROM GO_TRANSMISSION_PROFILES`

func scanProfile(row interface{ Scan(...any) error }) (TransmissionProfile, error) {
	var p TransmissionProfile
	var raw string
	err := row.Scan(&p.ID, &raw, &p.Fingerprint, &p.State, &p.Reason, &p.Created, &p.Updated)
	if err != nil {
		return p, err
	}
	if json.Unmarshal([]byte(raw), &p.Definition) != nil || bus.Hash(p.Definition) != p.Fingerprint {
		return p, ErrProfilePending
	}
	return p, nil
}
func profileSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_TRANSMISSION_PROFILES(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_TRANSMISSION_PROFILE_SCOPES(SCOPE TEXT PRIMARY KEY,PROFILE_ID TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS GO_TRANSMISSION_PROFILE_ACTIONS(ID INTEGER PRIMARY KEY,PROFILE_ID TEXT NOT NULL,ACTOR TEXT NOT NULL,ACTION TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func profileScope(id int64, hash string) string { return bus.Hash([]any{id, strings.ToLower(hash)}) }
func (s *Store) ReserveTransmissionProfile(ctx context.Context, d TransmissionProfileDefinition) (TransmissionProfile, bool, error) {
	d, err := NormalizeTransmissionProfile(d)
	if err != nil {
		return TransmissionProfile{}, false, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return TransmissionProfile{}, false, err
	}
	defer tx.Rollback()
	if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: d.DownloaderID, DownloaderType: "transmission", ConfigDigest: d.ConfigDigest}); err != nil {
		return TransmissionProfile{}, false, err
	}
	if err := profileSchema(ctx, tx); err != nil {
		return TransmissionProfile{}, false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT PROFILE_ID FROM GO_TRANSMISSION_PROFILE_SCOPES WHERE SCOPE=?`, profileScope(d.DownloaderID, d.InfoHash)).Scan(&id)
	if err == nil {
		p, err := scanProfile(tx.QueryRowContext(ctx, profileSelect+` WHERE ID=?`, id))
		if err != nil {
			return p, false, err
		}
		if p.Fingerprint != bus.Hash(d) {
			return p, false, ErrProfilePending
		}
		return p, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TransmissionProfile{}, false, err
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return TransmissionProfile{}, false, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	p := TransmissionProfile{ID: hex.EncodeToString(bytes[:]), Definition: d, Fingerprint: bus.Hash(d), State: "add_pending", Created: now, Updated: now}
	raw, _ := json.Marshal(d)
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_TRANSMISSION_PROFILES(ID,DOCUMENT,FINGERPRINT,STATE,CREATED,UPDATED) VALUES(?,?,?,'add_pending',?,?)`, p.ID, string(raw), p.Fingerprint, now, now)
	if err != nil {
		return p, false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return p, false, ErrProfilePending
	}
	result, err = tx.ExecContext(ctx, `INSERT INTO GO_TRANSMISSION_PROFILE_SCOPES VALUES(?,?)`, profileScope(d.DownloaderID, d.InfoHash), p.ID)
	if err != nil {
		return p, false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return p, false, ErrProfilePending
	}
	return p, true, tx.Commit()
}
func (s *Store) TransmissionProfile(ctx context.Context, id string) (TransmissionProfile, error) {
	return scanProfile(s.database.QueryRowContext(ctx, profileSelect+` WHERE ID=?`, id))
}
func (s *Store) FindTransmissionProfile(ctx context.Context, downloaderID int64, hash string) (TransmissionProfile, error) {
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_TRANSMISSION_PROFILE_SCOPES' AND type='table')`).Scan(&exists); err != nil {
		return TransmissionProfile{}, err
	}
	if !exists {
		return TransmissionProfile{}, ErrNotFound
	}
	var id string
	err := s.database.QueryRowContext(ctx, `SELECT PROFILE_ID FROM GO_TRANSMISSION_PROFILE_SCOPES WHERE SCOPE=?`, profileScope(downloaderID, hash)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return TransmissionProfile{}, ErrNotFound
	}
	if err != nil {
		return TransmissionProfile{}, err
	}
	return s.TransmissionProfile(ctx, id)
}
func (s *Store) TransmissionProfiles(ctx context.Context) ([]TransmissionProfile, error) {
	return s.TransmissionProfilesPage(ctx, 1)
}
func (s *Store) TransmissionProfilesPage(ctx context.Context, page int) ([]TransmissionProfile, error) {
	if page < 1 || page > 10000 {
		return nil, ErrProfilePending
	}
	result := []TransmissionProfile{}
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_TRANSMISSION_PROFILES' AND type='table')`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	rows, err := s.database.QueryContext(ctx, profileSelect+` ORDER BY CASE WHEN STATE NOT IN ('ready','rejected') THEN 0 ELSE 1 END,CREATED DESC,ID LIMIT 100 OFFSET ?`, (page-1)*100)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
func (s *Store) AdvanceTransmissionProfile(ctx context.Context, p TransmissionProfile, to, reason, actor, fp string) error {
	allowed := map[string]bool{"added": true, "configuring": true, "configured": true, "starting": true, "ready": true, "rejected": true}
	if !allowed[to] || actor == "" {
		return ErrProfilePending
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if to != "rejected" {
		if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: p.Definition.DownloaderID, DownloaderType: "transmission", ConfigDigest: p.Definition.ConfigDigest}); err != nil {
			return err
		}
		if err := verifyProfileSeeding(ctx, tx, p.Definition); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_TRANSMISSION_PROFILES SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE=? AND FINGERPRINT=?`, to, reason, time.Now().UTC().Format(time.RFC3339Nano), p.ID, p.State, p.Fingerprint)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrProfilePending
	}
	if to == "rejected" {
		if p.State != "add_pending" {
			return ErrProfilePending
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM GO_TRANSMISSION_PROFILE_SCOPES WHERE SCOPE=? AND PROFILE_ID=?`, profileScope(p.Definition.DownloaderID, p.Definition.InfoHash), p.ID); err != nil {
			return err
		}
	}
	result, err = tx.ExecContext(ctx, `INSERT INTO GO_TRANSMISSION_PROFILE_ACTIONS(PROFILE_ID,ACTOR,ACTION,FINGERPRINT,CREATED) VALUES(?,?,?,?,?)`, p.ID, actor, to, fp, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrProfilePending
	}
	if to != "rejected" {
		if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: p.Definition.DownloaderID, DownloaderType: "transmission", ConfigDigest: p.Definition.ConfigDigest}); err != nil {
			return err
		}
		if err := verifyProfileSeeding(ctx, tx, p.Definition); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func VerifyTransmissionProfile(ctx context.Context, tx *sql.Tx, fp string) error {
	if fp == "" {
		return nil
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT p.DOCUMENT FROM GO_TRANSMISSION_PROFILES p JOIN GO_TRANSMISSION_PROFILE_SCOPES s ON s.PROFILE_ID=p.ID WHERE p.FINGERPRINT=? AND p.STATE='ready'`, fp).Scan(&raw); err != nil {
		return ErrProfilePending
	}
	var d TransmissionProfileDefinition
	if json.Unmarshal([]byte(raw), &d) != nil || bus.Hash(d) != fp {
		return ErrProfilePending
	}
	if err := verifyProfileSeeding(ctx, tx, d); err != nil {
		return err
	}
	return verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: d.DownloaderID, DownloaderType: "transmission", ConfigDigest: d.ConfigDigest})
}
