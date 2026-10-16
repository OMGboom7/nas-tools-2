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

func TestPTAdmissionGrantIsConfigurationScopedCASRevocableAndPrivate(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "user.db")
	s, err := Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	grant, err := s.SeedingAdmission(ctx, 1)
	if err != nil || grant != nil {
		t.Fatal(grant, err)
	}
	var exists bool
	_ = s.database.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_PT_ADMISSIONS')`).Scan(&exists)
	if exists {
		t.Fatal("read created schema")
	}
	item, err := s.Upsert(ctx, Downloader{Name: "PT", Type: "transmission", Enabled: 1, Config: `{"host":"tr.test","password":"private-password"}`, DownloadDir: `[]`})
	if err != nil {
		t.Fatal(err)
	}
	old := bus.Hash(grant)
	grant, err = s.SetSeedingAdmission(ctx, item, "0:admin", true, old)
	if err != nil || grant == nil || !grant.Enabled || grant.ConfigDigest != bus.Hash(item) {
		t.Fatal(grant, err)
	}
	if _, err := s.SetSeedingAdmission(ctx, item, "0:admin", true, old); !errors.Is(err, ErrSeedingPolicy) {
		t.Fatal("stale grant accepted", err)
	}
	raw, _ := json.Marshal(grant)
	if strings.Contains(string(raw), "private-password") || strings.Contains(string(raw), grant.ConfigDigest) {
		t.Fatal(string(raw))
	}
	before := bus.Hash(grant)
	disabled, err := s.SetSeedingAdmission(ctx, item, "0:admin", false, before)
	if err != nil || disabled.Enabled || disabled.ID != grant.ID {
		t.Fatal(disabled, err)
	}
	if _, err := s.SetSeedingAdmission(ctx, item, "0:admin", true, before); !errors.Is(err, ErrSeedingPolicy) {
		t.Fatal("revoked scope reused", err)
	}
	newGrant, err := s.SetSeedingAdmission(ctx, item, "0:admin", true, bus.Hash(disabled))
	if err != nil || newGrant.ID == grant.ID {
		t.Fatal(newGrant, err)
	}
	var count int
	_ = s.database.QueryRow(`SELECT COUNT(*) FROM GO_PT_ADMISSION_EVENTS`).Scan(&count)
	if count != 3 {
		t.Fatal(count)
	}
	s.Close()
	s, err = Open(file)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.SeedingAdmission(ctx, item.ID)
	if err != nil || loaded.ID != newGrant.ID {
		t.Fatal(loaded, err)
	}
	item.Name = "new config"
	if _, err := s.Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	if loaded.ConfigDigest == bus.Hash(item) {
		t.Fatal("grant rebound automatically")
	}
	item.Type = "aria2"
	if _, err := s.Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetSeedingAdmission(ctx, item, "0:admin", true, bus.Hash(loaded)); !errors.Is(err, ErrSeedingPolicy) {
		t.Fatal("unsupported type enabled", err)
	}
	if _, err := s.SetSeedingAdmission(ctx, item, "0:admin", false, bus.Hash(loaded)); err != nil {
		t.Fatal("cannot revoke changed scope", err)
	}
}

func TestPTSourceReferenceRequiredAtEveryProfileCommit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	item, err := s.Upsert(ctx, Downloader{Name: "PT", Type: "transmission", Enabled: 1, Config: `{"host":"tr.test"}`, DownloadDir: `[]`})
	if err != nil {
		t.Fatal(err)
	}
	seconds, ratio := int64(900), 1.5
	policy := SeedingPolicy{DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, ConfigDigest: bus.Hash(item), InfoHash: strings.Repeat("a", 40), SourceDigest: bus.Hash("source"), Requirements: seeding.Requirements{Seconds: &seconds, Ratio: &ratio}, Actor: "0:admin"}
	d := TransmissionProfileDefinition{DownloaderID: item.ID, Name: item.Name, ConfigDigest: bus.Hash(item), InfoHash: policy.InfoHash, SeedingPolicyID: SeedingPolicyIdentity(policy)}
	p, created, err := s.ReserveTransmissionProfile(ctx, d)
	if err != nil || !created {
		t.Fatal(p, created, err)
	}
	if err := s.AdvanceTransmissionProfile(ctx, p, "added", "", "native-submit", ""); !errors.Is(err, ErrProfilePending) {
		t.Fatal("missing source accepted", err)
	}
	_, digest, err := s.SeedingPolicies(ctx, item.ID, policy.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindSeedingPolicy(ctx, policy, digest); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyProfileSeeding(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceTransmissionProfile(ctx, p, "ready", "", "test", ""); err != nil {
		t.Fatal(err)
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTransmissionProfile(ctx, tx, p.Fingerprint); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if _, err := s.database.Exec(`DELETE FROM GO_SEEDING_POLICIES WHERE ID=?`, d.SeedingPolicyID); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyProfileSeeding(ctx, d); !errors.Is(err, ErrProfilePending) {
		t.Fatal("missing obligation ignored", err)
	}
	tx, err = s.database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := VerifyTransmissionProfile(ctx, tx, p.Fingerprint); !errors.Is(err, ErrProfilePending) {
		t.Fatal("local history could commit without source", err)
	}
}
