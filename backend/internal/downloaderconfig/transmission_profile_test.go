package downloaderconfig

import (
	"strings"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

func storeProfileFixture(t *testing.T) (*Store, TransmissionProfileDefinition) {
	t.Helper()
	s, c, _ := manualFixture(t)
	c.Type = "transmission"
	if _, err := s.Upsert(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	c, err := s.Get(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, TransmissionProfileDefinition{DownloaderID: c.ID, Name: c.Name, ConfigDigest: bus.Hash(c), InfoHash: strings.Repeat("a", 40), Options: transmission.AddOptions{Labels: []string{"NASTOOL"}}, Limits: transmission.Limits{Ratio: 1.5}}
}
func TestTransmissionProfileReservationAndAuditMustActuallyPersist(t *testing.T) {
	for _, table := range []string{"GO_TRANSMISSION_PROFILES", "GO_TRANSMISSION_PROFILE_SCOPES"} {
		t.Run(table, func(t *testing.T) {
			s, d := storeProfileFixture(t)
			p, _, err := s.ReserveTransmissionProfile(t.Context(), d)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AdvanceTransmissionProfile(t.Context(), p, "rejected", "rejected", "actor", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.database.Exec(`CREATE TRIGGER ignore_profile BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(IGNORE); END`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.ReserveTransmissionProfile(t.Context(), d); err == nil {
				t.Fatal("ignored reservation falsely succeeded")
			}
		})
	}
	s, d := storeProfileFixture(t)
	p, _, err := s.ReserveTransmissionProfile(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.Exec(`CREATE TRIGGER ignore_profile_audit BEFORE INSERT ON GO_TRANSMISSION_PROFILE_ACTIONS BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceTransmissionProfile(t.Context(), p, "configuring", "", "actor", ""); err == nil {
		t.Fatal("unrecorded intent falsely succeeded")
	}
	current, err := s.TransmissionProfile(t.Context(), p.ID)
	if err != nil || current.State != "add_pending" {
		t.Fatal(current, err)
	}
}
func TestTransmissionProfileSourceProofRechecksConfigAndReadyScope(t *testing.T) {
	s, d := storeProfileFixture(t)
	p, _, err := s.ReserveTransmissionProfile(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyTransmissionProfile(t.Context(), tx, p.Fingerprint)
	tx.Rollback()
	if err == nil {
		t.Fatal("pending profile accepted")
	}
	if err := s.AdvanceTransmissionProfile(t.Context(), p, "ready", "", "actor", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.Exec(`UPDATE DOWNLOADER SET ENABLED=0`); err != nil {
		t.Fatal(err)
	}
	tx, err = s.database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyTransmissionProfile(t.Context(), tx, p.Fingerprint)
	tx.Rollback()
	if err == nil {
		t.Fatal("revoked config accepted")
	}
}
