package downloaderconfig

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
)

func TestQbProfilesRequireSourceAndReadyProofAtCommitPreserveScopeAndPrivacy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if rows, err := s.QbProfiles(ctx, 1); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	item, err := s.Upsert(ctx, Downloader{Name: "qB", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.test","password":"private-password"}`, DownloadDir: `[]`})
	if err != nil {
		t.Fatal(err)
	}
	policy := SeedingPolicy{DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, InfoHash: strings.Repeat("a", 40), ConfigDigest: bus.Hash(item), SourceDigest: bus.Hash("private-passkey"), Actor: "0:admin"}
	d := QbProfileDefinition{DownloaderID: item.ID, Name: item.Name, ConfigDigest: bus.Hash(item), InfoHash: policy.InfoHash, SeedingPolicyID: SeedingPolicyIdentity(policy), Options: qbittorrent.AddOptions{ManagedPT: true, Tags: []string{"user"}}}
	p, created, err := s.ReserveQbProfile(ctx, d)
	if err != nil || !created {
		t.Fatal(p, err)
	}
	if err := s.AdvanceQbProfile(ctx, p, "added", "worker", ""); err == nil {
		t.Fatal("source-less profile advanced")
	}
	_, digest, _ := s.SeedingPolicies(ctx, item.ID, d.InfoHash)
	if _, err := s.BindSeedingPolicy(ctx, policy, digest); err != nil {
		t.Fatal(err)
	}
	state := qbittorrent.ProfileState{Hash: d.InfoHash, Category: "", Tags: []string{"user", p.Marker()}, RatioLimit: -1, SeedLimit: -1, IdleLimit: -1, EffectiveRatio: -1, EffectiveSeed: -1, EffectiveIdle: -1, State: "downloading"}
	if !p.Ready(state) {
		t.Fatal(state)
	}
	state.Tags = []string{"user"}
	if p.Owns(state) {
		t.Fatal("unowned adopted")
	}
	tx, _ := s.database.BeginTx(ctx, nil)
	if VerifyDownloadProfile(ctx, tx, p.Fingerprint) == nil {
		t.Fatal("unready source committed")
	}
	tx.Rollback()
	if err := s.AdvanceQbProfile(ctx, p, "ready", "worker", ""); err != nil {
		t.Fatal(err)
	}
	tx, _ = s.database.BeginTx(ctx, nil)
	if err := VerifyDownloadProfile(ctx, tx, p.Fingerprint); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	d.Options.Category = "changed"
	if _, _, err := s.ReserveQbProfile(ctx, d); err == nil {
		t.Fatal("scope silently replaced")
	}
	loaded, err := s.FindQbProfile(ctx, item.ID, d.InfoHash)
	if err != nil || loaded.ID != p.ID || loaded.State != "ready" {
		t.Fatal(loaded, err)
	}
	raw, _ := json.Marshal(loaded)
	if strings.Contains(string(raw), "private-password") || strings.Contains(string(raw), "private-passkey") || strings.Contains(string(raw), d.ConfigDigest) {
		t.Fatal("private fields exposed", string(raw))
	}
	if _, err := s.database.Exec(`DELETE FROM GO_SEEDING_POLICIES`); err != nil {
		t.Fatal(err)
	}
	tx, _ = s.database.BeginTx(ctx, nil)
	defer tx.Rollback()
	if VerifyDownloadProfile(ctx, tx, p.Fingerprint) == nil {
		t.Fatal("missing source could commit")
	}
}
