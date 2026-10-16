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
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
)

var ErrQbProfilePending = errors.New("qBittorrent PT settings require explicit review")

type QbProfileDefinition struct {
	DownloaderID    int64                  `json:"downloaderId"`
	Name            string                 `json:"name"`
	ConfigDigest    string                 `json:"configDigest"`
	InfoHash        string                 `json:"infoHash"`
	SeedingPolicyID string                 `json:"seedingPolicyId"`
	Options         qbittorrent.AddOptions `json:"options"`
}

func NormalizeQbProfile(d QbProfileDefinition) (QbProfileDefinition, error) {
	if d.DownloaderID < 1 || !validSeedingDigest(d.ConfigDigest) || !validSeedingDigest(d.SeedingPolicyID) || !qbittorrent.ValidHash(d.InfoHash) || !d.Options.ManagedPT || d.Options.RatioLimit != 0 || d.Options.SeedingTimeLimit != 0 || len(d.Options.SavePath) > 4096 || len(d.Options.Category) > 256 || len(d.Options.Tags) > 31 || d.Options.UploadLimitKB < 0 || d.Options.UploadLimitKB > 1<<30 || d.Options.DownloadLimitKB < 0 || d.Options.DownloadLimitKB > 1<<30 {
		return d, ErrQbProfilePending
	}
	d.InfoHash = strings.ToLower(d.InfoHash)
	tags := []string{}
	seen := map[string]bool{}
	for _, tag := range d.Options.Tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if len(tag) > 128 || strings.ContainsAny(tag, ",\r\n\x00") || strings.HasPrefix(tag, "NASTOOL-QB-PROFILE-") {
			return d, ErrQbProfilePending
		}
		if !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}
	d.Options.Tags = tags
	return d, nil
}
func QbFingerprint(d QbProfileDefinition) string { return "qb:" + bus.Hash(d) }

type QbProfile struct {
	Definition  QbProfileDefinition `json:"-"`
	ID          string              `json:"id"`
	Fingerprint string              `json:"fingerprint"`
	State       string              `json:"state"`
	Created     string              `json:"created"`
	Updated     string              `json:"updated"`
}

func (p QbProfile) Marker() string { return "NASTOOL-QB-PROFILE-" + p.ID }
func (p QbProfile) Owns(s qbittorrent.ProfileState) bool {
	if s.Hash != p.Definition.InfoHash || s.Category != p.Definition.Options.Category {
		return false
	}
	if dir := p.Definition.Options.SavePath; dir != "" && strings.TrimRight(s.Directory, "/") != strings.TrimRight(dir, "/") {
		return false
	}
	tags := map[string]bool{}
	for _, tag := range s.Tags {
		tags[tag] = true
	}
	if !tags[p.Marker()] {
		return false
	}
	for _, tag := range p.Definition.Options.Tags {
		if !tags[tag] {
			return false
		}
	}
	return true
}
func (p QbProfile) Matches(s qbittorrent.ProfileState) bool {
	return p.Owns(s) && s.SpeedsMatch(p.Definition.Options) && s.Unlimited()
}
func (p QbProfile) Ready(s qbittorrent.ProfileState) bool {
	return p.Matches(s) && (p.Definition.Options.Paused && s.Paused() || !p.Definition.Options.Paused && s.Running())
}

const qbProfileSelect = `SELECT ID,DOCUMENT,FINGERPRINT,STATE,CREATED,UPDATED FROM GO_QB_PROFILES`

func scanQbProfile(row interface{ Scan(...any) error }) (QbProfile, error) {
	var p QbProfile
	var raw string
	if err := row.Scan(&p.ID, &raw, &p.Fingerprint, &p.State, &p.Created, &p.Updated); err != nil {
		return p, err
	}
	if json.Unmarshal([]byte(raw), &p.Definition) != nil || QbFingerprint(p.Definition) != p.Fingerprint {
		return p, ErrQbProfilePending
	}
	if _, err := NormalizeQbProfile(p.Definition); err != nil {
		return p, err
	}
	return p, nil
}
func qbSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_QB_PROFILES(ID TEXT PRIMARY KEY,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,STATE TEXT NOT NULL,CREATED TEXT NOT NULL,UPDATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_QB_PROFILE_SCOPES(SCOPE TEXT PRIMARY KEY,PROFILE_ID TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS GO_QB_PROFILE_ACTIONS(ID INTEGER PRIMARY KEY,PROFILE_ID TEXT NOT NULL,ACTOR TEXT NOT NULL,ACTION TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) ReserveQbProfile(ctx context.Context, d QbProfileDefinition) (QbProfile, bool, error) {
	d, err := NormalizeQbProfile(d)
	if err != nil {
		return QbProfile{}, false, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return QbProfile{}, false, err
	}
	defer tx.Rollback()
	if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: d.DownloaderID, DownloaderType: "qbittorrent", ConfigDigest: d.ConfigDigest}); err != nil {
		return QbProfile{}, false, err
	}
	if err := qbSchema(ctx, tx); err != nil {
		return QbProfile{}, false, err
	}
	var id string
	err = tx.QueryRowContext(ctx, `SELECT PROFILE_ID FROM GO_QB_PROFILE_SCOPES WHERE SCOPE=?`, profileScope(d.DownloaderID, d.InfoHash)).Scan(&id)
	if err == nil {
		p, err := scanQbProfile(tx.QueryRowContext(ctx, qbProfileSelect+` WHERE ID=?`, id))
		if err != nil || p.Fingerprint != QbFingerprint(d) {
			return p, false, ErrQbProfilePending
		}
		return p, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return QbProfile{}, false, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return QbProfile{}, false, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	p := QbProfile{Definition: d, ID: hex.EncodeToString(random[:]), Fingerprint: QbFingerprint(d), State: "add_pending", Created: now, Updated: now}
	raw, err := json.Marshal(d)
	if err != nil {
		return p, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_QB_PROFILES VALUES(?,?,?,?,?,?)`, p.ID, string(raw), p.Fingerprint, p.State, now, now); err != nil {
		return p, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_QB_PROFILE_SCOPES VALUES(?,?)`, profileScope(d.DownloaderID, d.InfoHash), p.ID); err != nil {
		return p, false, err
	}
	return p, true, tx.Commit()
}
func (s *Store) QbProfile(ctx context.Context, id string) (QbProfile, error) {
	p, err := scanQbProfile(s.database.QueryRowContext(ctx, qbProfileSelect+` WHERE ID=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return p, err
}
func (s *Store) FindQbProfile(ctx context.Context, id int64, hash string) (QbProfile, error) {
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_QB_PROFILE_SCOPES' AND type='table')`).Scan(&exists); err != nil {
		return QbProfile{}, err
	}
	if !exists {
		return QbProfile{}, ErrNotFound
	}
	var key string
	err := s.database.QueryRowContext(ctx, `SELECT PROFILE_ID FROM GO_QB_PROFILE_SCOPES WHERE SCOPE=?`, profileScope(id, hash)).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return QbProfile{}, ErrNotFound
	}
	if err != nil {
		return QbProfile{}, err
	}
	return s.QbProfile(ctx, key)
}
func (s *Store) QbProfiles(ctx context.Context, page int) ([]QbProfile, error) {
	result := []QbProfile{}
	if page < 1 || page > 10000 {
		return nil, ErrQbProfilePending
	}
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_QB_PROFILES' AND type='table')`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	rows, err := s.database.QueryContext(ctx, qbProfileSelect+` ORDER BY CASE WHEN STATE NOT IN ('ready','rejected') THEN 0 ELSE 1 END,CREATED DESC,ID LIMIT 100 OFFSET ?`, (page-1)*100)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanQbProfile(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
func verifyQbSource(ctx context.Context, q seedingQuerier, d QbProfileDefinition) error {
	rows, _, err := readSeedingPolicies(ctx, q, d.DownloaderID, d.InfoHash)
	if err != nil {
		return ErrQbProfilePending
	}
	for _, p := range rows {
		if p.ID == d.SeedingPolicyID && p.ConfigDigest == d.ConfigDigest {
			return nil
		}
	}
	return ErrQbProfilePending
}
func (s *Store) VerifyQbSource(ctx context.Context, d QbProfileDefinition) error {
	return verifyQbSource(ctx, s.database, d)
}
func (s *Store) AdvanceQbProfile(ctx context.Context, p QbProfile, to, actor, fp string) error {
	if actor == "" {
		return ErrQbProfilePending
	}
	switch to {
	case "added", "configuring", "configured", "starting", "ready", "rejected":
	default:
		return ErrQbProfilePending
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	verify := func() error {
		if to == "rejected" {
			if p.State != "add_pending" {
				return ErrQbProfilePending
			}
			return nil
		}
		if err := verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: p.Definition.DownloaderID, DownloaderType: "qbittorrent", ConfigDigest: p.Definition.ConfigDigest}); err != nil {
			return err
		}
		return verifyQbSource(ctx, tx, p.Definition)
	}
	if err := verify(); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_QB_PROFILES SET STATE=?,UPDATED=? WHERE ID=? AND STATE=? AND FINGERPRINT=?`, to, time.Now().UTC().Format(time.RFC3339Nano), p.ID, p.State, p.Fingerprint)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrQbProfilePending
	}
	if to == "rejected" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM GO_QB_PROFILE_SCOPES WHERE SCOPE=? AND PROFILE_ID=?`, profileScope(p.Definition.DownloaderID, p.Definition.InfoHash), p.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_QB_PROFILE_ACTIONS(PROFILE_ID,ACTOR,ACTION,FINGERPRINT,CREATED) VALUES(?,?,?,?,?)`, p.ID, actor, to, fp, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	return tx.Commit()
}

// Typed qB fingerprints share the existing source-submission proof column;
// old unprefixed Transmission fingerprints retain their exact encoding.
func VerifyDownloadProfile(ctx context.Context, tx *sql.Tx, fp string) error {
	if !strings.HasPrefix(fp, "qb:") {
		return VerifyTransmissionProfile(ctx, tx, fp)
	}
	p, err := scanQbProfile(tx.QueryRowContext(ctx, qbProfileSelect+` WHERE FINGERPRINT=? AND STATE='ready' AND ID IN(SELECT PROFILE_ID FROM GO_QB_PROFILE_SCOPES)`, fp))
	if err != nil {
		return ErrQbProfilePending
	}
	if err := verifyQbSource(ctx, tx, p.Definition); err != nil {
		return err
	}
	return verifyManualConfig(ctx, tx, ManualDefinition{DownloaderID: p.Definition.DownloaderID, DownloaderType: "qbittorrent", ConfigDigest: p.Definition.ConfigDigest})
}
