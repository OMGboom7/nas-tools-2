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

type planReceiver struct {
	targetFixture
	sent int
}

func (c *planReceiver) TransferWireID(_ context.Context, m torrentmeta.TransferMetadata) (string, error) {
	return m.V1Hash, nil
}
func (c *planReceiver) AddReviewedTransfer(ctx context.Context, content []byte, dir string, i AddIntent) (AddReceipt, error) {
	return i.Send(ctx, c.Address(), i.WireID, dir, content, func() (AddReceipt, error) { c.sent++; return AddReceipt{WireID: i.WireID, CreatedKnown: true}, nil })
}
func TestSavedPlanChecksGrantAgainBeforeSending(t *testing.T) {
	ctx := t.Context()
	path, id, _ := transferFileFixture(t)
	settings := Settings{Enable: true, From: []string{"1"}, To: []string{"2"}, TorrentPath: path}
	src := sourceFixture{task: Task{Kind: "transmission", WireID: id, V1Hash: id, AddedAt: 1, Directory: "/library", State: "seeding", SourceSeeding: true, Complete: true, Labels: []string{}, Files: []torrentfiles.File{{Index: 0, Path: "movie.mkv", Size: 8, Selected: true}}, Done: []int{0}}}
	dst := &planReceiver{}
	root, err := OpenMetainfoRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	review, ok, err := prepareSource(ctx, settings, src, root, id)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	grant, receipt := Hash("grant"), Hash("receipt")
	plan, err := PreparePlan(ctx, review, dst, grant)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ledger, err := NewLedger(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SavePlan(ctx, receipt, plan); err != nil {
		t.Fatal(err)
	}
	calls := 0
	resolve := func(context.Context) (Settings, SourceClient, ReceiverClient, string, error) {
		calls++
		if calls == 2 {
			return settings, src, dst, "", nil
		}
		return settings, src, dst, grant, nil
	}
	if _, err := ledger.SubmitPlan(ctx, receipt, resolve); !errors.Is(err, ErrState) || dst.sent != 0 {
		t.Fatal(err, dst.sent)
	}
	if _, err := ledger.SubmitPlan(ctx, receipt, resolve); err != nil || dst.sent != 1 {
		t.Fatal(err, dst.sent)
	}
	if _, err := ledger.SubmitPlan(ctx, receipt, resolve); !errors.Is(err, ErrPriorAdd) || dst.sent != 1 {
		t.Fatal(err, dst.sent)
	}
	dst.task = src.task
	dst.task.State, dst.task.Paused, dst.task.SourceSeeding = "stopped", true, false
	dst.task.Labels = []string{"转移做种", "已整理"}
	dst.task.AddedAt++
	if _, err := ledger.ContinuePlan(ctx, receipt, resolve, nil, nil); !errors.Is(err, ErrState) {
		t.Fatal("unconfirmed target", err)
	}
	confirm := func(_ context.Context, reply AddReceipt, task Task) error {
		if !reply.CreatedKnown || !task.SameInstance(dst.task) {
			return ErrState
		}
		return nil
	}
	if stage, err := ledger.ContinuePlan(ctx, receipt, resolve, confirm, nil); err != nil || stage != "bound" {
		t.Fatal(stage, err)
	}
	if _, err := ledger.ContinuePlan(ctx, receipt, resolve, nil, nil); err != nil || dst.checks != 1 || dst.sent != 1 {
		t.Fatal(err, dst.checks, dst.sent)
	}
	status, err := ReadStatus(ctx, db, receipt)
	if err != nil || status.TargetStage != "bound" || len(status.Actions) != 2 {
		t.Fatal(status, err)
	}
	plan.GrantDigest = Hash("replacement")
	if err := ledger.SavePlan(ctx, receipt, plan); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
}
