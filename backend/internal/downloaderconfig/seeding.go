package downloaderconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

var ErrSeedingPolicy = errors.New("PT seeding requirements need verification")

type SeedingPolicy struct {
	ID             string               `json:"id"`
	DownloaderID   int64                `json:"downloaderId"`
	DownloaderName string               `json:"downloaderName"`
	DownloaderType string               `json:"downloaderType"`
	ConfigDigest   string               `json:"-"`
	InfoHash       string               `json:"infoHash"`
	Title          string               `json:"title"`
	Source         string               `json:"source"`
	SourceDigest   string               `json:"-"`
	Requirements   seeding.Requirements `json:"requirements"`
	Actor          string               `json:"actor"`
	Created        string               `json:"created"`
}

func seedingPolicyID(p SeedingPolicy) string {
	return bus.Hash(struct {
		ID                   int64
		Hash, Config, Source string
		Requirements         seeding.Requirements
	}{p.DownloaderID, p.InfoHash, p.ConfigDigest, p.SourceDigest, p.Requirements})
}

type seedingQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) SeedingPolicies(ctx context.Context, id int64, hash string) ([]SeedingPolicy, string, error) {
	policies := []SeedingPolicy{}
	var exists bool
	if err := s.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SEEDING_POLICIES' AND type='table')`).Scan(&exists); err != nil || !exists {
		return policies, bus.Hash(policies), err
	}
	return readSeedingPolicies(ctx, s.database, id, hash)
}

func readSeedingPolicies(ctx context.Context, q seedingQuerier, id int64, hash string) ([]SeedingPolicy, string, error) {
	policies := []SeedingPolicy{}
	rows, err := q.QueryContext(ctx, `SELECT ID,DOCUMENT,CONFIG_DIGEST,SOURCE_DIGEST FROM GO_SEEDING_POLICIES WHERE DOWNLOADER_ID=? AND INFO_HASH=? ORDER BY ID LIMIT 101`, id, strings.ToLower(hash))
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var p SeedingPolicy
		var key, raw string
		if err := rows.Scan(&key, &raw, &p.ConfigDigest, &p.SourceDigest); err != nil {
			return nil, "", err
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, "", err
		}
		if p.DownloaderID != id || p.InfoHash != strings.ToLower(hash) || !p.Requirements.Valid() || len(policies) >= 100 || p.ID != key || p.ID != seedingPolicyID(p) || !validSeedingDigest(p.ConfigDigest) || !validSeedingDigest(p.SourceDigest) {
			return nil, "", ErrSeedingPolicy
		}
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return policies, bus.Hash(policies), nil
}
func validSeedingDigest(value string) bool { return len(value) == 64 && seeding.ValidHash(value) }

func (s *Store) BindSeedingPolicy(ctx context.Context, p SeedingPolicy, expected string) (SeedingPolicy, error) {
	if p.DownloaderID < 1 || !seeding.ValidHash(p.InfoHash) || !p.Requirements.Valid() || !validSeedingDigest(p.ConfigDigest) || !validSeedingDigest(p.SourceDigest) || p.Actor == "" || len(p.Actor) > 256 || len(p.Title) > 2400 || len(p.Source) > 640 || p.DownloaderType != "qbittorrent" && p.DownloaderType != "transmission" {
		return p, ErrSeedingPolicy
	}
	p.InfoHash = strings.ToLower(p.InfoHash)
	// Serialize with every Go task control under the shared per-hash execution
	// guard. Append only: old unknown/stricter obligations are never overwritten.
	for _, statement := range []string{`CREATE TABLE IF NOT EXISTS GO_SEEDING_POLICIES(ID TEXT PRIMARY KEY,DOWNLOADER_ID INTEGER NOT NULL,INFO_HASH TEXT NOT NULL,DOCUMENT TEXT NOT NULL,CONFIG_DIGEST TEXT NOT NULL,SOURCE_DIGEST TEXT NOT NULL)`, `CREATE INDEX IF NOT EXISTS GO_SEEDING_POLICY_SCOPE ON GO_SEEDING_POLICIES(DOWNLOADER_ID,INFO_HASH)`} {
		if _, err := s.database.ExecContext(ctx, statement); err != nil {
			return p, err
		}
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	current, digest, err := readSeedingPolicies(ctx, tx, p.DownloaderID, p.InfoHash)
	if err != nil || digest != expected || len(current) >= 100 {
		return p, ErrSeedingPolicy
	}
	var item Downloader
	if err := tx.QueryRowContext(ctx, `SELECT ID,COALESCE(NAME,''),COALESCE(ENABLED,0),COALESCE(TYPE,''),COALESCE(TRANSFER,0),COALESCE(ONLY_NASTOOL,0),COALESCE(MATCH_PATH,0),COALESCE(RMT_MODE,''),COALESCE(CONFIG,'{}'),COALESCE(DOWNLOAD_DIR,'[]') FROM DOWNLOADER WHERE ID=?`, p.DownloaderID).Scan(&item.ID, &item.Name, &item.Enabled, &item.Type, &item.Transfer, &item.OnlyNastool, &item.MatchPath, &item.RmtMode, &item.Config, &item.DownloadDir); err != nil || item.Enabled == 0 || bus.Hash(item) != p.ConfigDigest {
		return p, ErrSeedingPolicy
	}
	if item.Type != p.DownloaderType {
		return p, ErrSeedingPolicy
	}
	p.ID = seedingPolicyID(p)
	var old string
	err = tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_SEEDING_POLICIES WHERE ID=?`, p.ID).Scan(&old)
	if err == nil {
		var found SeedingPolicy
		if json.Unmarshal([]byte(old), &found) != nil {
			return p, ErrSeedingPolicy
		}
		found.ConfigDigest = p.ConfigDigest
		found.SourceDigest = p.SourceDigest
		return found, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	p.Created = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_SEEDING_POLICIES VALUES(?,?,?,?,?,?)`, p.ID, p.DownloaderID, p.InfoHash, string(raw), p.ConfigDigest, p.SourceDigest)
	if err != nil {
		return p, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return p, ErrSeedingPolicy
	}
	return p, tx.Commit()
}
