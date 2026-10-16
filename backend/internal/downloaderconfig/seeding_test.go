package downloaderconfig

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

func TestSeedingPolicyAppendOnlyScopeCASPersistenceAndPrivacy(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "user.db")
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	hash := strings.Repeat("a", 40)
	p, digest, err := s.SeedingPolicies(ctx, 1, hash)
	if err != nil || len(p) != 0 {
		t.Fatal(p, err)
	}
	var exists bool
	_ = s.database.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_SEEDING_POLICIES')`).Scan(&exists)
	if exists {
		t.Fatal("readonly created schema")
	}
	item, err := s.Upsert(ctx, Downloader{Name: "Seed", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.test","password":"private-password"}`, DownloadDir: `[]`})
	if err != nil {
		t.Fatal(err)
	}
	seconds, ratio, hr := int64(900), 1.5, false
	policy := SeedingPolicy{DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: strings.ToUpper(hash), Title: "Release", Source: "Tracker", SourceDigest: bus.Hash("private-passkey"), Requirements: seeding.Requirements{Seconds: &seconds, Ratio: &ratio, HR: &hr}, Actor: "0:admin"}
	saved, err := s.BindSeedingPolicy(ctx, policy, digest)
	if err != nil || saved.ID == "" || saved.Created == "" {
		t.Fatal(saved, err)
	}
	if _, err := s.BindSeedingPolicy(ctx, policy, digest); !errors.Is(err, ErrSeedingPolicy) {
		t.Fatal("stale review accepted", err)
	}
	p, next, err := s.SeedingPolicies(ctx, item.ID, hash)
	if err != nil || len(p) != 1 || next == digest || p[0].ConfigDigest != policy.ConfigDigest {
		t.Fatal(p, err)
	}
	again, err := s.BindSeedingPolicy(ctx, policy, next)
	if err != nil || again.ID != saved.ID || again.Created != saved.Created {
		t.Fatal(again, err)
	}
	policy.Requirements = seeding.Requirements{}
	policy.SourceDigest = bus.Hash("unknown source")
	if _, err := s.BindSeedingPolicy(ctx, policy, next); err != nil {
		t.Fatal(err)
	}
	p, _, err = s.SeedingPolicies(ctx, item.ID, hash)
	if err != nil || len(p) != 2 {
		t.Fatal(p, err)
	}
	rules := []seeding.Requirements{}
	for _, p := range p {
		rules = append(rules, p.Requirements)
	}
	complete := true
	if seeding.Evaluate(hash, rules, seeding.Evidence{Hash: hash, Complete: &complete, Seconds: &seconds, Ratio: &ratio}).Allowed {
		t.Fatal("later unknown obligation ignored")
	}
	raw, _ := json.Marshal(p)
	for _, secret := range []string{"private-password", "private-passkey", policy.ConfigDigest, policy.SourceDigest} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private source exposed", string(raw))
		}
	}
	s.Close()
	s, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	p, next, err = s.SeedingPolicies(ctx, item.ID, hash)
	if err != nil || len(p) != 2 {
		t.Fatal(p, err)
	}
	item.Name = "Changed"
	if _, err := s.Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindSeedingPolicy(ctx, policy, next); !errors.Is(err, ErrSeedingPolicy) {
		t.Fatal("changed config accepted", err)
	}
	if _, err := s.database.Exec(`UPDATE GO_SEEDING_POLICIES SET SOURCE_DIGEST=?`, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SeedingPolicies(ctx, item.ID, hash); !errors.Is(err, ErrSeedingPolicy) {
		t.Fatal("corrupt scope accepted", err)
	}
}
