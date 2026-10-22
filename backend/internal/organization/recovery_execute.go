package organization

import (
	"context"
	"encoding/json"
	"path"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

func recoveryNoDeletes(r RecoveryDisposal) bool {
	for _, item := range r.Items {
		if item.State != "pending" {
			return false
		}
	}
	return true
}
func recoveryAbsentDirectory(r RecoveryDisposal, want RecoveryNode, actual map[string]RecoveryNode) bool {
	if r.Driver == "" || want.Kind != "directory" {
		return false
	}
	found := false
	for _, item := range r.Items {
		if item.Node.Kind == "directory" || !sourceUnder(item.Node.Path, want.Path) || item.Node.Path == want.Path {
			continue
		}
		found = true
		if _, present := actual[item.Node.Path]; present || item.State == "pending" {
			return false
		}
	}
	return found
}
func recoveryRemaining(r RecoveryDisposal, actual []RecoveryNode) error {
	nodes := map[string]RecoveryNode{}
	for _, n := range actual {
		if _, yes := nodes[n.Path]; yes {
			return ErrState
		}
		nodes[n.Path] = n
	}
	complete := nodesForRecovery(actual)
	for _, item := range r.Items {
		n, present := nodes[item.Node.Path]
		delete(nodes, item.Node.Path)
		if item.State == "done" {
			if present {
				return ErrState
			}
			continue
		}
		if !present {
			if item.State != "intent" && !recoveryAbsentDirectory(r, item.Node, complete) {
				return ErrState
			}
			continue
		}
		if !recoveryNodeSame(n, item.Node) {
			return ErrState
		}
	}
	if len(nodes) != 0 {
		return ErrState
	}
	return nil
}
func nodesForRecovery(nodes []RecoveryNode) map[string]RecoveryNode {
	result := map[string]RecoveryNode{}
	for _, n := range nodes {
		result[n.Path] = n
	}
	return result
}
func recoveryActual(ctx context.Context, r RecoveryDisposal, c *remotestore.Client) ([]RecoveryNode, error) {
	s := r.Definition.Scope
	location := r.Path
	if r.Driver == "" {
		location = r.Capture
		n, err := recoveryLocalPresence(s, location)
		if err != nil {
			return nil, err
		}
		if n == nil {
			return []RecoveryNode{}, nil
		}
		return recoveryLocalSnapshot(ctx, s, location)
	}
	n, err := c.Entry(ctx, s.Root.Path, location)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return []RecoveryNode{}, nil
	}
	return recoveryRemoteSnapshot(ctx, s, location, c)
}
func recoveryInventoryActual(ctx context.Context, r RecoveryDisposal, c *remotestore.Client) ([]RecoveryNode, error) {
	s := r.Definition.Scope
	if r.Driver == "" {
		n, err := recoveryLocalPresence(s, r.Capture)
		if err != nil {
			return nil, err
		}
		if n == nil {
			return []RecoveryNode{}, nil
		}
		return recoveryLocalInventory(ctx, s, r.Capture)
	}
	n, err := c.Entry(ctx, s.Root.Path, r.Path)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return []RecoveryNode{}, nil
	}
	return recoveryRemoteInventory(ctx, s, r.Path, c)
}
func recoveryPresence(ctx context.Context, r RecoveryDisposal, node RecoveryNode, c *remotestore.Client) (bool, error) {
	if r.Driver == "" {
		n, err := recoveryLocalPresence(r.Definition.Scope, path.Join(r.Capture, node.Path))
		return n != nil, err
	}
	n, err := c.Entry(ctx, r.Definition.Scope.Root.Path, path.Join(r.Path, node.Path))
	return n != nil, err
}
func (s *Store) ReviewRecoveryDisposal(ctx context.Context, r RecoveryDisposal, c *remotestore.Client) (RecoveryDisposalReview, error) {
	result := RecoveryDisposalReview{Disposal: r, Evidence: "requires independent review"}
	if recoveryTerminal(r.State) {
		result.Evidence = r.State
		result.Fingerprint = Digest([]any{r.ID, r.State, r.Updated})
		return result, nil
	}
	if err := recoveryClient(r.Definition.Scope, c); err != nil {
		return result, err
	}
	var actual []RecoveryNode
	var original, held *RecoveryNode
	var err error
	if r.Driver == "" {
		original, err = recoveryLocalPresence(r.Definition.Scope, r.Path)
		if err != nil {
			return result, err
		}
		held, err = recoveryLocalPresence(r.Definition.Scope, r.Capture)
		if err != nil {
			return result, err
		}
		if r.State != "ready" && recoveryNoDeletes(r) && held != nil && original == nil {
			result.CanReturn = true
			result.ReturnNode = held
		}
		if r.State == "returning" {
			result.CanReturn = r.ReturnNode != nil && ((held == nil && original != nil && recoveryNodeSame(*original, *r.ReturnNode)) || (original == nil && held != nil && recoveryNodeSame(*held, *r.ReturnNode)))
			result.ReturnNode = r.ReturnNode
		} else if recoveryNoDeletes(r) && held == nil && original != nil {
			actual, err = recoveryLocalSnapshot(ctx, r.Definition.Scope, r.Path)
			result.CanContinue = err == nil && Digest(actual) == Digest(r.Definition.Nodes)
		} else if original == nil && r.State != "ready" {
			actual, err = recoveryActual(ctx, r, c)
			result.CanContinue = err == nil && recoveryRemaining(r, actual) == nil
		}
	} else {
		actual, err = recoveryActual(ctx, r, c)
		result.CanContinue = err == nil && recoveryRemaining(r, actual) == nil
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	owners, ownerErr := recoveryOwners(ctx, s.db, r.Definition.Scope, r.Path)
	expectedOwners := r.Frozen
	if r.State == "ready" {
		expectedOwners = r.Definition.Owners
	}
	if ownerErr != nil {
		return result, ownerErr
	}
	ownersMatch := Digest(owners) == Digest(expectedOwners)
	result.CanContinue = result.CanContinue && ownersMatch
	// An unsafe, changed, oversized or incomplete inventory is a visible
	// refusal, not a successfully reviewed subset and never deletion authority.
	if result.CanContinue {
		result.Evidence = "full remaining inventory verified; explicit single continuation required"
	} else if result.CanReturn {
		result.Evidence = "nothing deleted; captured root may only be returned without overwrite"
	} else {
		result.Evidence = "changed or incomplete evidence; objects and all old claims retained"
	}
	result.Fingerprint = Digest([]any{r.ID, r.State, r.Updated, r.LastReview, r.Items, actual, original, held, r.ReturnNode, owners})
	return result, nil
}

// This routine is never scheduled or replayed on startup. Every call needs a
// newly reviewed fingerprint and all independent irreversible-disposal grants.
func (s *Store) ExecuteRecoveryDisposal(ctx context.Context, r RecoveryDisposal, c *remotestore.Client, review string, validate func() error) (err error) {
	if validate == nil {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	started, err := s.BeginRecoveryAttempt(ctx, r, review)
	if err != nil || !started {
		return err
	}
	defer func() {
		if err != nil {
			persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = s.recoveryPhase(persist, r.ID, "needs_review", "interrupted independent disposal; no automatic retry, remaining objects retained")
		}
	}()
	current := func() error {
		if err := validate(); err != nil {
			return err
		}
		return s.CheckRecoveryRunning(ctx, r.ID)
	}
	r, err = s.RecoveryDisposal(ctx, r.ID)
	if err != nil {
		return err
	}
	if r.State == "capturing" {
		if err := recoveryLocalCapture(ctx, r.Definition, r.Capture, current); err != nil {
			return err
		}
		if err := s.recoveryPhase(ctx, r.ID, "deleting", "complete captured inventory verified; individual removals still require durable intent"); err != nil {
			return err
		}
		r.State = "deleting"
	}
	actual, err := recoveryActual(ctx, r, c)
	if err != nil {
		return err
	}
	if err := recoveryRemaining(r, actual); err != nil {
		return err
	}
	remaining := nodesForRecovery(actual)
	for i := len(r.Items) - 1; i >= 0; i-- {
		if err := current(); err != nil {
			return err
		}
		item := r.Items[i]
		if item.State == "done" {
			continue
		}
		_, wasPresent := remaining[item.Node.Path]
		present := false
		if wasPresent {
			present, err = recoveryPresence(ctx, r, item.Node, c)
			if err != nil {
				return err
			}
			if !present && item.State != "intent" && !recoveryAbsentDirectory(r, item.Node, remaining) {
				return ErrState
			}
		}
		if err := s.recoveryItemIntent(ctx, r.ID, item.Ordinal); err != nil {
			return err
		}
		if present {
			if err := current(); err != nil {
				return err
			}
			location := path.Join(r.Path, item.Node.Path)
			if r.Driver == "" {
				location = path.Join(r.Capture, item.Node.Path)
				if err := recoveryLocalRemove(ctx, r.Definition.Scope, location, item.Node, current); err != nil {
					return err
				}
			} else {
				if item.Node.Remote == nil {
					return ErrState
				}
				if err := c.DeleteReviewed(ctx, path.Join(r.Definition.Scope.Root.Path, location), *item.Node.Remote, item.Node.Digest); err != nil {
					return err
				}
			}
		}
		// A lost delete response ends the attempt before here. A later explicit
		// review may positively observe the requested absence and only receipt it.
		if present {
			exists, err := recoveryPresence(ctx, r, item.Node, c)
			if err != nil || exists {
				return ErrState
			}
		}
		if err := current(); err != nil {
			return err
		}
		if err := s.recoveryItemDone(ctx, r.ID, item.Ordinal); err != nil {
			return err
		}
		r.Items[i].State = "done"
		delete(remaining, item.Node.Path)
	}
	final, err := recoveryInventoryActual(ctx, r, c)
	if err != nil || len(final) != 0 {
		return ErrState
	}
	if err := current(); err != nil {
		return err
	}
	return s.finishRecovery(ctx, r.ID)
}
func (s *Store) ReturnRecoveryCapture(ctx context.Context, r RecoveryDisposal, review RecoveryDisposalReview, validate func() error) error {
	if validate == nil || !review.CanReturn || review.ReturnNode == nil || r.Driver != "" || !recoveryNoDeletes(r) {
		return ErrState
	}
	if err := validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(review.ReturnNode)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE GO_RECOVERY_DISPOSALS SET STATE='returning',RETURN_NODE=?,LAST_REVIEW=?,UPDATED=?,REASON='explicit no-overwrite return of whole capture; old jobs remain closed' WHERE ID=? AND STATE NOT IN('completed','returned','closed')`, string(raw), review.Fingerprint, time.Now().UTC().Format(time.RFC3339Nano), r.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrState
	}
	original, err := recoveryLocalPresence(r.Definition.Scope, r.Path)
	if err != nil {
		return err
	}
	held, err := recoveryLocalPresence(r.Definition.Scope, r.Capture)
	if err != nil {
		return err
	}
	if held == nil && original != nil && recoveryNodeSame(*original, *review.ReturnNode) {
		return s.recoveryPhase(ctx, r.ID, "returned", "whole capture already back in place; database receipt only, no second rename")
	}
	current := func() error {
		if err := validate(); err != nil {
			return err
		}
		return s.CheckRecoveryRunning(ctx, r.ID)
	}
	if err := recoveryLocalReturn(ctx, r, *review.ReturnNode, current); err != nil {
		return err
	}
	return s.recoveryPhase(ctx, r.ID, "returned", "whole undeleted capture returned without overwrite; old jobs and claims not reopened")
}

func RecoveryOwnerGuardNames(d RecoveryDefinition) []string {
	names := []string{"independent-recovery-disposal", "intake-enrichment-outbox", "library-scraper-publication", "rclone-namespace:NASTOOL"}
	seen := map[string]bool{}
	for _, name := range names {
		seen[name] = true
	}
	for _, owner := range d.Owners {
		if owner.Kind == "organization" {
			for _, name := range []string{owner.ID, "legacy-batch:" + owner.BatchID} {
				if name == "legacy-batch:" || seen[name] {
					continue
				}
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}
