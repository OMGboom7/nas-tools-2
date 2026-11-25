package torrenttransfer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

type targetFixture struct {
	task           Task
	checks, starts int
}

func (*targetFixture) Address() string                                      { return "http://target.fixture" }
func (c *targetFixture) TransferTask(context.Context, string) (Task, error) { return c.task, nil }
func (c *targetFixture) RecheckTransfer(context.Context, Task, torrentmeta.TransferMetadata) error {
	c.checks++
	return nil
}
func (c *targetFixture) StartTransfer(context.Context, Task, torrentmeta.TransferMetadata) error {
	c.starts++
	return nil
}

func TestTargetRequiresOriginalGrantAndObservedCheckBeforeStart(t *testing.T) {
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
	id := Hash("fixture")[:40]
	files := []torrentfiles.File{{Index: 0, Path: "movie.mkv", Size: 8, Selected: true}}
	m := torrentmeta.TransferMetadata{Content: []byte("fixture"), V1Hash: id, Files: files}
	c := &targetFixture{task: Task{Kind: "transmission", WireID: id, V1Hash: id, AddedAt: 42,
		Directory: "/library", State: "stopped", Paused: true, Complete: true, HasTracker: true,
		Labels: []string{"转移做种", "已整理"}, Files: files, Done: []int{0}}}
	i := AddIntent{Ledger: ledger, Receipt: Hash("receipt"), ScopeProof: Hash("grant"), Receiver: Hash(c.Address()),
		WireID: id, Directory: "/library", ContentDigest: contentDigest(m.Content), Revalidate: func(context.Context) error { return nil }}
	_, err = i.Send(ctx, c.Address(), id, i.Directory, m.Content, func() (AddReceipt, error) { return AddReceipt{WireID: id, CreatedKnown: true}, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := i.BindTarget(ctx, c, m, nil); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	if err := i.BindTarget(ctx, c, m, func(context.Context, AddReceipt, Task) error { return nil }); err != nil {
		t.Fatal(err)
	}
	changed := i
	changed.ScopeProof = Hash("another grant")
	if _, err := changed.advanceTarget(ctx, c, m, nil); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	step := func(start bool, want string) {
		t.Helper()
		var authorize func(context.Context) error
		if start {
			authorize = func(context.Context) error { return nil }
		}
		got, err := i.advanceTarget(ctx, c, m, authorize)
		if err != nil || got != want {
			t.Fatalf("stage %q, want %q: %v", got, want, err)
		}
	}
	step(true, "acknowledged")
	step(true, "bound")
	c.task.State, c.task.Checking, c.task.Paused, c.task.Complete = "checking", true, false, false
	step(true, "checking")
	c.task.State, c.task.Checking, c.task.Paused, c.task.Complete = "stopped", false, true, true
	c.task.Done = nil
	step(true, "checking")
	c.task.Done = []int{0}
	step(true, "verified")
	step(false, "verified")
	if c.starts != 0 || c.checks != 1 {
		t.Fatal(c.starts, c.checks)
	}
	step(true, "acknowledged")
	c.task.State, c.task.Paused, c.task.SourceSeeding = "seeding", false, true
	step(true, "acknowledged")
	if c.starts != 1 || c.checks != 1 {
		t.Fatal(c.starts, c.checks)
	}
}
