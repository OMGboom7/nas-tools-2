package torrenttransfer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
)

func TestAddIntentBindsPayloadAndPreservesDuplicateReplyAcrossCycles(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger, err := NewLedger(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("fixture-torrent")
	digest := sha256.Sum256(raw)
	addr := "http://downloader.fixture"
	id := Hash("task")[:40]
	i := AddIntent{Ledger: ledger, Receipt: Hash("first"), ScopeProof: Hash("scope"), Receiver: Hash(addr), WireID: id, Directory: "/library", ContentDigest: hex.EncodeToString(digest[:]), Revalidate: func(context.Context) error { return nil }}
	calls := 0
	send := func() (AddReceipt, error) {
		calls++
		return AddReceipt{WireID: id, Duplicate: true, CreatedKnown: true}, nil
	}
	if _, err := i.Send(ctx, addr, id, "/library", []byte("other"), send); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	reply, err := i.Send(ctx, addr, id, "/library", raw, send)
	if err != nil || !reply.Duplicate {
		t.Fatal(reply, err)
	}
	key := ActionKey{Receiver: i.Receiver, WireID: id, Action: "add"}
	stored, err := ledger.AddReply(ctx, key, i.Receipt)
	if err != nil || stored != reply {
		t.Fatal(stored, err)
	}
	i.Receipt = Hash("another cycle")
	if _, err := i.Send(ctx, addr, id, "/library", raw, send); !errors.Is(err, ErrPriorAdd) || calls != 1 {
		t.Fatal(calls, err)
	}
}
