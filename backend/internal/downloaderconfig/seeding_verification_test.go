package downloaderconfig

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/seeding"
)

func TestPTVerificationImmutableScopedHistoryCASRevocationAndExpiry(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	hash := strings.Repeat("a", 40)
	state, err := s.PTVerification(ctx, 1, hash)
	if err != nil || state.Record != nil {
		t.Fatal(state, err)
	}
	empty := bus.Hash(state)
	var exists bool
	_ = s.database.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_PT_VERIFICATIONS')`).Scan(&exists)
	if exists {
		t.Fatal("read created schema")
	}
	item, err := s.Upsert(ctx, Downloader{Name: "PT", Type: "qbittorrent", Enabled: 1, Config: `{"host":"qb.test","password":"private-password"}`, DownloadDir: `[]`})
	if err != nil {
		t.Fatal(err)
	}
	policy := SeedingPolicy{DownloaderID: item.ID, DownloaderName: item.Name, DownloaderType: item.Type, InfoHash: hash, ConfigDigest: bus.Hash(item), SourceDigest: bus.Hash("private-passkey"), Actor: "0:admin"}
	_, digest, _ := s.SeedingPolicies(ctx, item.ID, hash)
	policy, err = s.BindSeedingPolicy(ctx, policy, digest)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err = s.SeedingPolicies(ctx, item.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	complete, seconds, ratio, epoch := true, int64(1800), 2.0, int64(1700000000)
	v := PTVerification{DownloaderID: item.ID, InfoHash: hash, ConfigDigest: bus.Hash(item), PolicyDigest: digest, PolicyIDs: []string{policy.ID}, Actor: "0:admin", Note: "已在站点账户核实此任务全部做种义务", Actions: VerificationActions{Stop: true}, Observed: seeding.Evidence{Hash: hash, State: "seeding", Complete: &complete, Seconds: &seconds, Ratio: &ratio, AddedAt: &epoch}}
	v, err = s.RecordPTVerification(ctx, v, 15, empty)
	if err != nil || !v.Valid() || !v.Current(bus.Hash(item), digest, time.Now()) || v.Kind != "operator-attestation" {
		t.Fatal(v, err)
	}
	if v.Current(bus.Hash(item), digest, time.Now().Add(16*time.Minute)) || v.Current(bus.Hash("changed"), digest, time.Now()) || v.Actions.Allows("remove") {
		t.Fatal("scope or expiry escaped")
	}
	if _, err := s.RecordPTVerification(ctx, v, 15, empty); err == nil {
		t.Fatal("stale record replayed")
	}
	state, err = s.PTVerification(ctx, item.ID, hash)
	if err != nil || state.Record.ID != v.ID || state.Revoked {
		t.Fatal(state, err)
	}
	before := bus.Hash(state)
	if err := s.RevokePTVerification(ctx, item.ID, hash, "0:admin", before); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePTVerification(ctx, item.ID, hash, "0:admin", before); err == nil {
		t.Fatal("stale revocation replayed")
	}
	state, err = s.PTVerification(ctx, item.ID, hash)
	if err != nil || !state.Revoked || state.RevokedBy != "0:admin" {
		t.Fatal(state, err)
	}
	if _, err := s.database.Exec(`UPDATE GO_PT_VERIFICATION_REVOKES SET CREATED='' WHERE VERIFICATION_ID=?`, v.ID); err != nil {
		t.Fatal(err)
	}
	state, err = s.PTVerification(ctx, item.ID, hash)
	if err != nil || !state.Revoked {
		t.Fatal("damaged audit timestamp undid revocation", state, err)
	}
	next, err := s.RecordPTVerification(ctx, v, 5, bus.Hash(state))
	if err != nil || next.ID == v.ID {
		t.Fatal(next, err)
	}
	var count int
	_ = s.database.QueryRow(`SELECT COUNT(*) FROM GO_PT_VERIFICATIONS`).Scan(&count)
	if count != 2 {
		t.Fatal("audit history replaced", count)
	}
	policies, _, err := s.SeedingPolicies(ctx, item.ID, hash)
	if err != nil || len(policies) != 1 || policies[0].Requirements.HR != nil {
		t.Fatal("original obligations changed", policies, err)
	}
	raw, _ := json.Marshal(next)
	if strings.Contains(string(raw), "private-password") || strings.Contains(string(raw), "private-passkey") || strings.Contains(string(raw), next.ConfigDigest) {
		t.Fatal("private configuration exposed", string(raw))
	}
	if _, err := s.database.Exec(`UPDATE GO_PT_VERIFICATIONS SET CONFIG_DIGEST=?`, bus.Hash("wrong")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PTVerification(ctx, item.ID, hash); err == nil {
		t.Fatal("corrupt scope accepted")
	}
}

func TestPTVerificationRejectsUnsafeNotesAndActionExpansion(t *testing.T) {
	for _, note := range []string{"short", "账户已核实 https://tracker.test?id=private", "账户已核实\nCookie=private"} {
		if VerificationNoteValid(note) {
			t.Fatal(note)
		}
	}
	if !(VerificationActions{Move: true}).Allows("move") || (VerificationActions{Move: true}).Allows("stop") {
		t.Fatal("action scope")
	}
}
