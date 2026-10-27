package downloaderconfig

import (
	"context"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"strings"
	"testing"
)

func storeSelectionFixture(t *testing.T) (*Store, FileSelectionDefinition) {
	s, c, _ := manualFixture(t)
	d := FileSelectionDefinition{Version: 1, ID: strings.Repeat("a", 32), DownloaderID: c.ID, DownloaderType: c.Type, ConfigDigest: bus.Hash(c), InfoHash: strings.Repeat("b", 40), Title: "Show", SubscriptionID: 2, Season: 1, Episodes: []int{2}, Scope: strings.Repeat("c", 64), Options: []byte(`{"Tags":["NASTOOL"]}`), Proof: "{}", Files: []torrentfiles.File{{Index: 0, Path: "Show/S01E01.mkv", Size: 11}, {Index: 1, Path: "Show/S01E02.mkv", Size: 12, Selected: true}}}
	return s, d
}
func reserveStoreSelection(t *testing.T, s *Store, d FileSelectionDefinition) {
	t.Helper()
	tx, err := s.database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ReserveFileSelectionTx(t.Context(), tx, d); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func TestFileSelectionReadOnlyNoDDLAndHashScopeReservedAtomically(t *testing.T) {
	s, d := storeSelectionFixture(t)
	if p, err := s.FindFileSelection(t.Context(), d.DownloaderID, d.InfoHash); err != nil || p != nil {
		t.Fatal(p, err)
	}
	if p, err := s.FileSelections(t.Context(), 1); err != nil || len(p) != 0 {
		t.Fatal(p, err)
	}
	var count int
	s.database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'GO_TORRENT_FILE_%'`).Scan(&count)
	if count != 0 {
		t.Fatal("read created tables")
	}
	reserveStoreSelection(t, s, d)
	other := d
	other.ID = strings.Repeat("d", 32)
	tx, _ := s.database.BeginTx(t.Context(), nil)
	if err := ReserveFileSelectionTx(t.Context(), tx, other); err == nil {
		t.Fatal("duplicate hash scope")
	}
	tx.Rollback()
	rows, err := s.FileSelections(t.Context(), 1)
	if err != nil || len(rows) != 1 || rows[0].State != "adding" {
		t.Fatal(rows, err)
	}
}

func TestFileSelectionCohortSourceIdentityIsBoundedAndCanonical(t *testing.T) {
	_, d := storeSelectionFixture(t)
	d.Version = 2
	d.Sources = []FileSelectionSource{{SubscriptionID: 2, Season: 1, Total: 3, Episodes: []int{2}}, {SubscriptionID: 9007199254740993, Season: 2, Total: 2, Episodes: []int{1, 2}}}
	if !validSelection(d) {
		t.Fatal("valid joint definition rejected")
	}
	for _, sources := range [][]FileSelectionSource{
		nil, {d.Sources[1]}, {d.Sources[1], d.Sources[0]}, {d.Sources[0], d.Sources[0]},
		{{SubscriptionID: 2, Season: 1, Total: 3, Episodes: []int{1}}},
		{{SubscriptionID: 2, Season: 1, Total: 0, Episodes: []int{2}}},
		{{SubscriptionID: 2, Season: 1, Total: 3, Episodes: []int{2}}, {SubscriptionID: 3, Season: 2, Total: 1, Episodes: []int{2}}},
	} {
		bad := d
		bad.Sources = sources
		if validSelection(bad) {
			t.Fatal("invalid joint definition accepted", sources)
		}
	}
	d.Version = 1
	if validSelection(d) {
		t.Fatal("legacy definition adopted new joint sources")
	}
}
func TestFileSelectionIgnoredIntentOrActionCannotFakeSuccess(t *testing.T) {
	for _, table := range []string{"GO_TORRENT_FILE_SELECTIONS", "GO_TORRENT_FILE_SELECTION_SCOPES", "GO_TORRENT_FILE_SELECTION_ACTIONS"} {
		t.Run(table, func(t *testing.T) {
			s, d := storeSelectionFixture(t)
			reserveStoreSelection(t, s, d)
			if _, err := s.database.Exec(`CREATE TRIGGER ignore_file_write BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(IGNORE); END`); err != nil {
				t.Fatal(err)
			}
			if table == "GO_TORRENT_FILE_SELECTION_ACTIONS" {
				p, _ := s.FileSelection(t.Context(), d.ID)
				tx, _ := s.database.BeginTx(t.Context(), nil)
				if FileSelectionTransitionTx(t.Context(), tx, p, "added", "admin", "") == nil {
					t.Fatal("ignored action accepted")
				}
				tx.Rollback()
				p, _ = s.FileSelection(t.Context(), d.ID)
				if p.State != "adding" {
					t.Fatal("partial state committed")
				}
			} else {
				other := d
				other.ID = strings.Repeat("d", 32)
				other.InfoHash = strings.Repeat("e", 40)
				tx, _ := s.database.BeginTx(t.Context(), nil)
				if ReserveFileSelectionTx(t.Context(), tx, other) == nil {
					t.Fatal("ignored intent accepted")
				}
				tx.Rollback()
			}
		})
	}
}
func TestFileSelectionReadyProofAndSourceCommitShareExactPrivatePayload(t *testing.T) {
	s, d := storeSelectionFixture(t)
	reserveStoreSelection(t, s, d)
	p, _ := s.FileSelection(t.Context(), d.ID)
	change := func(to string) {
		tx, _ := s.database.BeginTx(context.Background(), nil)
		defer tx.Rollback()
		if err := FileSelectionTransitionTx(t.Context(), tx, p, to, "admin", "review"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		p.State = to
	}
	tx, _ := s.database.BeginTx(t.Context(), nil)
	if VerifyDownloadProfile(t.Context(), tx, d.ProfileFingerprint()) == nil {
		t.Fatal("adding considered ready")
	}
	tx.Rollback()
	for _, state := range []string{"added", "unwanted", "wanting", "selected", "starting", "ready"} {
		change(state)
	}
	tx, _ = s.database.BeginTx(t.Context(), nil)
	if CommitFileSelectionTx(t.Context(), tx, d.ID, `{"wrong":true}`) == nil {
		t.Fatal("wrong payload accepted")
	}
	tx.Rollback()
	tx, _ = s.database.BeginTx(t.Context(), nil)
	if err := CommitFileSelectionTx(t.Context(), tx, d.ID, "{}"); err != nil {
		t.Fatal(err)
	}
	if tx.Commit() != nil {
		t.Fatal("commit")
	}
	p, _ = s.FileSelection(t.Context(), d.ID)
	if !p.Committed {
		t.Fatal(p)
	}
	tx, _ = s.database.BeginTx(t.Context(), nil)
	if CommitFileSelectionTx(t.Context(), tx, d.ID, "{}") == nil {
		t.Fatal("duplicate source commit")
	}
	tx.Rollback()
}
func TestDefinitiveSelectionRejectionRetainsAttemptAndOnlyFreesItsOwnHashScope(t *testing.T) {
	s, d := storeSelectionFixture(t)
	reserveStoreSelection(t, s, d)
	p, _ := s.FileSelection(t.Context(), d.ID)
	tx, _ := s.database.BeginTx(t.Context(), nil)
	if err := FileSelectionTransitionTx(t.Context(), tx, p, "rejected", "admin", ""); err != nil {
		t.Fatal(err)
	}
	if tx.Commit() != nil {
		t.Fatal("commit")
	}
	if found, err := s.FindFileSelection(t.Context(), d.DownloaderID, d.InfoHash); err != nil || found != nil {
		t.Fatal(found, err)
	}
	d.ID = strings.Repeat("e", 32)
	reserveStoreSelection(t, s, d)
	rows, err := s.FileSelections(t.Context(), 1)
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
}
