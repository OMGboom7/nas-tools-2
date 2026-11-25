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
	settings := Settings{Enable: true, DeleteSource: true, From: []string{"1"}, To: []string{"2"}, TorrentPath: path}
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
	if _, err := db.Exec(`CREATE TABLE PLUGIN_HISTORY(ID INTEGER PRIMARY KEY,PLUGIN_ID TEXT,KEY TEXT,VALUE TEXT,DATE TEXT)`); err != nil {
		t.Fatal(err)
	}
	key := settings.From[0] + "-" + id
	if _, err := db.Exec(`INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE) VALUES(?,?,?)`, Plugin, key, "original"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ContinuePlan(ctx, receipt, resolve, confirm, nil); !errors.Is(err, ErrState) {
		t.Fatal("existing history overwritten", err)
	}
	var original string
	if err := db.QueryRow(`SELECT VALUE FROM PLUGIN_HISTORY WHERE KEY=?`, key).Scan(&original); err != nil || original != "original" {
		t.Fatal(original, err)
	}
	if _, err := db.Exec("DELETE FROM PLUGIN_HISTORY WHERE KEY=?", key); err != nil {
		t.Fatal(err)
	}
	if stage, err := ledger.ContinuePlan(ctx, receipt, resolve, confirm, nil); err != nil || stage != "bound" {
		t.Fatal(stage, err)
	}
	var to int64
	var task string
	var deleted bool
	if err := db.QueryRow(`SELECT json_extract(VALUE,'$.to_download'),json_extract(VALUE,'$.to_download_id'),json_extract(VALUE,'$.delete_source') FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=?`, Plugin, key).Scan(&to, &task, &deleted); err != nil || to != 2 || task != id || deleted {
		t.Fatal(to, task, deleted, err)
	}
	if _, err := ledger.ContinuePlan(ctx, receipt, resolve, nil, nil); err != nil || dst.checks != 1 || dst.sent != 1 {
		t.Fatal(err, dst.checks, dst.sent)
	}
	status, err := ReadStatus(ctx, db, receipt)
	if err != nil || status.TargetStage != "bound" || len(status.Actions) != 2 {
		t.Fatal(status, err)
	}
	protected := &removalFixture{sourceFixture: src}
	removeScope := func(context.Context) (Settings, SourceClient, ReceiverClient, string, error) {
		return settings, protected, dst, grant, nil
	}
	approve := func(context.Context, string, Task, Task) error { return nil }
	if err := ledger.RemoveSource(ctx, receipt, removeScope, approve); !errors.Is(err, ErrState) {
		t.Fatal("unverified target", err)
	}
	dst.task.State, dst.task.Checking, dst.task.Complete, dst.task.Paused = "checking", true, false, false
	if _, err := ledger.ContinuePlan(ctx, receipt, resolve, nil, nil); err != nil {
		t.Fatal(err)
	}
	dst.task.State, dst.task.Checking, dst.task.Complete, dst.task.Paused = "stopped", false, true, true
	if _, err := ledger.ContinuePlan(ctx, receipt, resolve, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := ledger.RemoveSource(ctx, receipt, removeScope, approve); !errors.Is(err, ErrState) || protected.sends != 0 {
		t.Fatal("blocked policy", err)
	}
	protected.allowed = true
	for range 2 {
		if err := ledger.RemoveSource(ctx, receipt, removeScope, approve); err != nil || protected.sends != 1 {
			t.Fatal(err, protected.sends)
		}
	}
	if err := db.QueryRow("SELECT json_extract(VALUE,\x27$.delete_source\x27) FROM PLUGIN_HISTORY WHERE KEY=?", key).Scan(&deleted); err != nil || !deleted {
		t.Fatal(deleted, err)
	}
	plan.GrantDigest = Hash("replacement")
	if err := ledger.SavePlan(ctx, receipt, plan); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
}

type removalFixture struct {
	sourceFixture
	allowed bool
	sends   int
}

func (f *removalFixture) CheckTransferRemoval(context.Context, Task) error {
	if !f.allowed {
		return ErrState
	}
	return nil
}
func (f *removalFixture) RemoveTransferKeepingFiles(context.Context, Task) error {
	f.sends++
	return nil
}
func (f *removalFixture) TransferTask(ctx context.Context, id string) (Task, error) {
	if f.sends > 0 {
		return Task{}, ErrTaskAbsent
	}
	return f.sourceFixture.TransferTask(ctx, id)
}
