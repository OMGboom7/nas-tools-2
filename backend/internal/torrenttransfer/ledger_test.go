package torrenttransfer

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestInterruptedAddClaimSurvivesReopenAndNewCycle(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "user.db")
	open := func() (*sql.DB, *Ledger) {
		db, err := sql.Open("sqlite", file)
		if err != nil {
			t.Fatal(err)
		}
		l, err := NewLedger(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		return db, l
	}
	k := ActionKey{Receiver: Hash("receiver"), WireID: Hash("task")[:40], Action: "add"}
	receipt, proof := Hash("first cycle"), Hash("frozen plan")
	db, l := open()
	if _, send, err := l.Claim(ctx, k, receipt, proof); !send || err != nil {
		t.Fatal(send, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, l = open()
	defer db.Close()
	row, send, err := l.Claim(ctx, k, Hash("later cycle"), Hash("changed plan"))
	if err != nil || send || row.Receipt != receipt || row.Proof != proof || row.State != "pending" {
		t.Fatal(row, send, err)
	}
	if l.Finish(ctx, k, Hash("later cycle"), proof, "acknowledged") == nil {
		t.Fatal("foreign receipt consumed intent")
	}
	if err := l.Finish(ctx, k, receipt, proof, "unknown"); err != nil {
		t.Fatal(err)
	}
	if l.Finish(ctx, k, receipt, proof, "acknowledged") == nil {
		t.Fatal("unknown outcome overwritten")
	}
	if _, send, err := l.Claim(ctx, k, receipt, proof); send || err != nil {
		t.Fatal(send, err)
	}
}
