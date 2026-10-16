package downloaderconfig

import (
	"context"
	"encoding/json"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

// A configuration-scoped administrator grant. Disabling it affects future
// admissions, never removes obligations already recorded for existing tasks.
type SeedingAdmission struct {
	ID           string `json:"id"`
	DownloaderID int64  `json:"downloaderId"`
	ConfigDigest string `json:"-"`
	Actor        string `json:"actor"`
	Created      string `json:"created"`
	Enabled      bool   `json:"enabled"`
}

func admissionID(p SeedingAdmission) string {
	return bus.Hash([]any{p.DownloaderID, p.ConfigDigest, p.Actor, p.Created})
}

func readSeedingAdmission(ctx context.Context, q seedingQuerier, id int64) (*SeedingAdmission, error) {
	rows, err := q.QueryContext(ctx, `SELECT DOCUMENT,CONFIG_DIGEST FROM GO_PT_ADMISSIONS WHERE DOWNLOADER_ID=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var p SeedingAdmission
	var raw string
	if err := rows.Scan(&raw, &p.ConfigDigest); err != nil {
		return nil, err
	}
	if json.Unmarshal([]byte(raw), &p) != nil || p.DownloaderID != id || p.ID != admissionID(p) || !validSeedingDigest(p.ConfigDigest) {
		return nil, ErrSeedingPolicy
	}
	return &p, rows.Err()
}

func (s *Store) SeedingAdmission(ctx context.Context, id int64) (*SeedingAdmission, error) {
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_PT_ADMISSIONS' AND type='table')`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	return readSeedingAdmission(ctx, s.database, id)
}

func (s *Store) SetSeedingAdmission(ctx context.Context, item Downloader, actor string, enabled bool, expected string) (*SeedingAdmission, error) {
	if item.ID < 1 || actor == "" || len(actor) > 256 || enabled && (item.Type != "transmission" && item.Type != "qbittorrent" || item.Enabled == 0) {
		return nil, ErrSeedingPolicy
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS GO_PT_ADMISSIONS(DOWNLOADER_ID INTEGER PRIMARY KEY,DOCUMENT TEXT NOT NULL,CONFIG_DIGEST TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_PT_ADMISSION_EVENTS(ID INTEGER PRIMARY KEY,DOWNLOADER_ID INTEGER NOT NULL,GRANT_ID TEXT NOT NULL,ACTOR TEXT NOT NULL,ACTION TEXT NOT NULL,CREATED TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return nil, err
		}
	}
	current, err := manualDownloader(ctx, tx, item.ID)
	if err != nil || bus.Hash(current) != bus.Hash(item) {
		return nil, ErrSeedingPolicy
	}
	p, err := readSeedingAdmission(ctx, tx, item.ID)
	if err != nil || bus.Hash(p) != expected {
		return nil, ErrSeedingPolicy
	}
	if !enabled && p == nil {
		return nil, ErrSeedingPolicy
	}
	if enabled {
		p = &SeedingAdmission{DownloaderID: item.ID, ConfigDigest: bus.Hash(item), Actor: actor, Created: time.Now().UTC().Format(time.RFC3339Nano), Enabled: true}
		p.ID = admissionID(*p)
	} else {
		p.Enabled = false
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_PT_ADMISSIONS VALUES(?,?,?) ON CONFLICT(DOWNLOADER_ID) DO UPDATE SET DOCUMENT=excluded.DOCUMENT,CONFIG_DIGEST=excluded.CONFIG_DIGEST`, item.ID, string(raw), p.ConfigDigest); err != nil {
		return nil, err
	}
	action := "disable"
	if enabled {
		action = "enable"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO GO_PT_ADMISSION_EVENTS(DOWNLOADER_ID,GRANT_ID,ACTOR,ACTION,CREATED) VALUES(?,?,?,?,?)`, item.ID, p.ID, actor, action, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// The ID is also frozen into the downloader-settings receipt before adding.
func SeedingPolicyIdentity(p SeedingPolicy) string {
	return seedingPolicyID(p)
}

func verifyProfileSeeding(ctx context.Context, q seedingQuerier, d TransmissionProfileDefinition) error {
	if d.SeedingPolicyID == "" {
		return nil
	}
	policies, _, err := readSeedingPolicies(ctx, q, d.DownloaderID, d.InfoHash)
	if err != nil {
		return ErrProfilePending
	}
	for _, p := range policies {
		if p.ID == d.SeedingPolicyID && p.ConfigDigest == d.ConfigDigest {
			return nil
		}
	}
	return ErrProfilePending
}

func (s *Store) VerifyProfileSeeding(ctx context.Context, d TransmissionProfileDefinition) error {
	return verifyProfileSeeding(ctx, s.database, d)
}
