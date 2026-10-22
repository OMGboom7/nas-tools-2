//go:build linux || darwin

package organization

import (
	"context"
	"testing"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func TestCompletedRemoteReaderRejectsUnfinishedUnprovenOrDiscMediaBeforeDecoding(t *testing.T) {
	d, e, _, _, p := remoteUnitFixture(t)
	p.Incomplete = false
	p.Remote.StageReady = true
	p.Remote.PublicationIntent = true
	p.Remote.TargetVerified = true
	job := Job{State: "completed", Mode: d.Mode, Definition: d, TargetRoot: d.TargetRoot}
	item := JobItem{Entry: e, State: "completed", Proof: p}
	for _, mode := range []string{"no-bound-account", "unfinished-job", "unfinished-item", "unknown-item", "no-proof", "wrong-proof", "incomplete-proof", "no-stage", "no-intent", "local-mode", "disc", "no-current"} {
		t.Run(mode, func(t *testing.T) {
			j, it := job, item
			remote := *p.Remote
			it.Proof.Remote = &remote
			current := func(ctx context.Context) error { return ctx.Err() }
			switch mode {
			case "unfinished-job":
				j.State = "ready"
			case "unfinished-item":
				it.State = "publishing"
			case "unknown-item":
				it.Kind = "subtitle"
			case "no-proof":
				it.Proof.Remote = nil
			case "wrong-proof":
				it.Proof.Digest = "not a full digest"
			case "incomplete-proof":
				it.Proof.Incomplete = true
			case "no-stage":
				it.Proof.Remote.StageReady = false
			case "no-intent":
				it.Proof.Remote.PublicationIntent = false
			case "local-mode":
				j.Mode = "copy"
			case "disc":
				it.Entry.Disc = &DiscInfo{}
			case "no-current":
				current = nil
			}
			if _, err := ReadCompletedRemoteMedia(t.Context(), j, it, current, func(context.Context, *remotestore.VerifiedObject) ([]byte, error) {
				t.Fatal("unapproved input reached decoder")
				return nil, nil
			}); err == nil {
				t.Fatal("unproven input accepted", mode)
			}
		})
	}
}
