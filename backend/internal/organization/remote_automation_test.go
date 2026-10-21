//go:build linux || darwin

package organization

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func remoteAutoJournal(t *testing.T) (*Store, Definition, string, Proof, MonitorLease) {
	t.Helper()
	d, e, s, id, _ := remoteUnitFixture(t)
	d.TargetID = "target-fixture"
	lease := MonitorLease{Generation: strings.Repeat("a", 32), Actor: "admin", IDs: []string{"7"}, Remote: &RemoteAutomation{Protocol: 1, Profiles: map[string]RemoteProfileSeal{d.TargetID: {Digest: Digest(d.Remote), Directories: d.Remote.Directories}}, Exclusive: true, RecoveryCopy: true, ApprovedAt: time.Now().UTC().Format(time.RFC3339Nano)}}
	d.Sync = &SyncOrigin{MonitorGeneration: lease.Generation, Actor: "monitor:admin", Record: SyncRecord{ID: 7}}
	raw, _ := json.Marshal(d)
	if _, err := s.db.Exec(`UPDATE GO_ORGANIZATION_JOBS SET DEFINITION=? WHERE ID=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
	if err := monitorTables(t.Context(), s.db); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(lease)
	if _, err := s.db.Exec(`INSERT INTO GO_SYNC_MONITOR_CONTROL VALUES(1,?,'enabled','')`, string(raw)); err != nil {
		t.Fatal(err)
	}
	p, err := BuildRemoteProof(t.Context(), d, e, TempName(id, 0), lease.Remote.Approval(lease.Actor, "sync-monitor", lease.Generation))
	if err != nil {
		t.Fatal(err)
	}
	return s, d, id, p, lease
}
func TestRemoteAutomaticJournalRejectsRevokedOrReplacedAuthorityAtEveryTransition(t *testing.T) {
	for _, change := range []string{"paused", "generation", "profile", "source-scope"} {
		t.Run(change, func(t *testing.T) {
			s, d, id, p, lease := remoteAutoJournal(t)
			ctx := AutomaticRemoteContext(t.Context(), "sync-monitor", lease.Generation)
			if ok, err := s.Claim(ctx, id, 0); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := s.Stage(ctx, id, 0, p); err != nil {
				t.Fatal(err)
			}
			if change == "paused" {
				if err := s.StopSyncMonitor(t.Context(), "admin"); err != nil {
					t.Fatal(err)
				}
			} else {
				if change == "generation" {
					lease.Generation = strings.Repeat("b", 32)
				}
				if change == "profile" {
					lease.Remote.Profiles[d.TargetID] = RemoteProfileSeal{Digest: strings.Repeat("b", 64)}
				}
				if change == "source-scope" {
					lease.IDs = []string{"8"}
				}
				raw, _ := json.Marshal(lease)
				if _, err := s.db.Exec(`UPDATE GO_SYNC_MONITOR_CONTROL SET DOCUMENT=?`, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.VerifyRemotePhase(ctx, id, 0, p, "staging"); !errors.Is(err, ErrState) {
				t.Fatal("old remote phase still authorized", err)
			}
			p.Incomplete = false
			p.Remote.StageReady = true
			if err := s.PrepareRemote(ctx, id, 0, p); !errors.Is(err, ErrState) {
				t.Fatal("revoked stage advanced", err)
			}
			// Fresh manual recovery is separate from renewing a background lease.
			if err := s.PrepareRemote(t.Context(), id, 0, p); err != nil {
				t.Fatal("manual recovery incorrectly inherits revoked lease", err)
			}
		})
	}
}
