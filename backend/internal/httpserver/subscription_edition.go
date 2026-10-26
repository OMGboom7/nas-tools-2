package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Quality cycles are not media inventory. TV receipts at different priorities
// cannot be merged into a claim that a complete higher-quality season exists.
type subscriptionEditionCycle struct {
	Name, Year        string
	Version           int
	ID, Kind          string
	SubscriptionID    int64
	Scope, Rule       string
	Order, TopOrder   int
	GroupID           int64
	Season, Total     int
	Target, Remaining []int
	State             string
}
type subscriptionEditionSnapshot struct {
	Cycle         subscriptionEditionCycle
	Started       bool
	PreviousOrder int
}
type subscriptionEditionView struct {
	PreviousOrder int    `json:"previousOrder"`
	Order         int    `json:"order"`
	TopOrder      int    `json:"topOrder"`
	GroupID       int64  `json:"groupId,string"`
	CycleID       string `json:"cycleId"`
	Target        []int  `json:"target"`
	Remaining     []int  `json:"remaining"`
	CycleComplete bool   `json:"cycleComplete"`
}

func editionView(s *subscriptionEditionSnapshot) *subscriptionEditionView {
	if s == nil {
		return nil
	}
	c := s.Cycle
	id := c.ID
	if !s.Started {
		id = ""
	}
	return &subscriptionEditionView{s.PreviousOrder, c.Order, c.TopOrder, c.GroupID, id, append([]int{}, c.Target...), append([]int{}, c.Remaining...), c.State == "completed"}
}
func editionRule(raw map[string]any) string {
	c := map[string]any{}
	for k, v := range raw {
		if k != "STATE" {
			c[k] = v
		}
	}
	return fuzzyDigest(c)
}

func editionIdentity(c subscriptionEditionCycle) string {
	c.State = ""
	c.Remaining = nil
	return fuzzyDigest(c)
}
func validEditionCycle(c subscriptionEditionCycle) bool {
	if c.Name == "" || len(c.Name) > 4096 || len(c.Year) > 4 {
		return false
	}
	if c.Version != 1 || !fuzzyHex(c.ID, 64) || c.Kind != "MOV" && c.Kind != "TV" || c.SubscriptionID < 1 || !fuzzyHex(c.Scope, 64) || !fuzzyHex(c.Rule, 64) || c.Order < 1 || c.Order > 100 || c.TopOrder < c.Order || c.TopOrder > 100 || c.GroupID < 1 || c.Target == nil || c.Remaining == nil {
		return false
	}
	if c.State != "active" && c.State != "completed" && c.State != "abandoned" {
		return false
	}
	if c.Kind == "MOV" {
		return c.Season == 0 && c.Total == 0 && len(c.Target) == 0 && len(c.Remaining) == 0
	}
	if c.Season < 0 || c.Season > 1000 || c.Total < 1 || c.Total > 10000 || len(c.Target) == 0 || !validReconcileEpisodes(c.Target, c.Total) || !validReconcileEpisodes(c.Remaining, c.Total) || !slices.IsSorted(c.Target) || !slices.IsSorted(c.Remaining) {
		return false
	}
	for _, n := range c.Remaining {
		if !slices.Contains(c.Target, n) {
			return false
		}
	}
	return c.State != "completed" || len(c.Remaining) == 0
}
func editionSchema(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_EDITION_CYCLES(ID TEXT PRIMARY KEY,KIND TEXT NOT NULL,SUB_ID INTEGER NOT NULL,DOCUMENT TEXT NOT NULL,STATE TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS GO_SUBSCRIPTION_EDITION_ACTIVE ON GO_SUBSCRIPTION_EDITION_CYCLES(KIND,SUB_ID) WHERE STATE='active'`,
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_EDITION_ACTIONS(ID INTEGER PRIMARY KEY,CYCLE_ID TEXT NOT NULL,ACTION TEXT NOT NULL,DOCUMENT TEXT NOT NULL,ACTOR TEXT NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_SUBSCRIPTION_EDITION_ATTEMPTS(CYCLE_ID TEXT NOT NULL,OWNER TEXT NOT NULL,RESOURCE_KEY TEXT NOT NULL,PAYLOAD TEXT NOT NULL,STATE TEXT NOT NULL,PRIMARY KEY(CYCLE_ID,OWNER))`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func readEditionCycle(ctx context.Context, r fuzzyReader, kind string, id int64) (*subscriptionEditionCycle, error) {
	yes, err := fuzzyTable(ctx, r, "GO_SUBSCRIPTION_EDITION_CYCLES")
	if err != nil || !yes {
		return nil, err
	}
	var raw, cycleID, state string
	err = r.QueryRowContext(ctx, `SELECT ID,DOCUMENT,STATE FROM GO_SUBSCRIPTION_EDITION_CYCLES WHERE KIND=? AND SUB_ID=? AND STATE='active'`, kind, id).Scan(&cycleID, &raw, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c subscriptionEditionCycle
	if decodeFuzzyDocument(raw, &c, 1<<20) != nil || !validEditionCycle(c) || c.ID != cycleID || c.Kind != kind || c.SubscriptionID != id || c.State != state {
		return nil, errSubscriptionSubmissionConflict
	}
	return &c, nil
}
func (service subscriptionService) checkEditionSnapshot(ctx context.Context, r fuzzyReader, kind string, id int64, plan subscriptionSearchPlan) error {
	if plan.edition == nil {
		return nil
	}
	s, c := plan.edition, plan.edition.Cycle
	previous := 0
	if value := text(normalizeNativeSubscriptionRow(plan.raw)["filter_order"]); value != "" {
		var err error
		previous, err = strconv.Atoi(value)
		if err != nil || previous < 0 || previous > 100 {
			return errSubscriptionSubmissionConflict
		}
	}
	if s.PreviousOrder != previous || c.Order != 0 && previous != 0 && c.Order <= previous {
		return errSubscriptionSubmissionConflict
	}
	if c.Kind != kind || c.SubscriptionID != id || c.State != "active" || c.Rule != editionRule(plan.raw) {
		return errSubscriptionSubmissionConflict
	}
	scope, err := nativeSubscriptionConfiguration(ctx, r, service)
	if err != nil || scope != c.Scope {
		return errSubscriptionSubmissionConflict
	}
	current, err := readEditionCycle(ctx, r, kind, id)
	if err != nil {
		return err
	}
	if current == nil {
		if s.Started {
			return errSubscriptionSubmissionConflict
		}
		return nil
	}
	if !validEditionCycle(c) || fuzzyDigest(*current) != fuzzyDigest(c) {
		return errSubscriptionSubmissionConflict
	}
	return nil
}
func reserveEditionCycle(ctx context.Context, tx *sql.Tx, kind string, id int64, plan subscriptionSearchPlan, candidate subscriptionCandidate) error {
	if plan.edition == nil {
		return nil
	}
	c := plan.edition.Cycle
	if !validEditionCycle(c) || candidate.Priority != c.Order || candidate.groupID != c.GroupID || candidate.topOrder != c.TopOrder {
		return errSubscriptionSubmissionConflict
	}
	if err := editionSchema(ctx, tx); err != nil {
		return err
	}
	current, err := readEditionCycle(ctx, tx, kind, id)
	if err != nil {
		return err
	}
	if current != nil {
		if fuzzyDigest(*current) != fuzzyDigest(c) {
			return errSubscriptionSubmissionConflict
		}
		return nil
	}
	if plan.edition.Started {
		return errSubscriptionSubmissionConflict
	}
	return fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_EDITION_CYCLES(ID,KIND,SUB_ID,DOCUMENT,STATE) VALUES(?,?,?,?,'active')`, c.ID, kind, id, string(mustFuzzyJSON(c))))
}

func subscriptionPlanCompleted(kind string, plan subscriptionSearchPlan, accepted bool) bool {
	if plan.edition != nil {
		return plan.edition.Cycle.State == "completed" && plan.edition.Cycle.Order >= plan.edition.Cycle.TopOrder
	}
	return plan.LibraryComplete || kind == "TV" && len(plan.needed) == 0 || kind == "MOV" && accepted
}
func closeEditionCycleTx(ctx context.Context, tx *sql.Tx, kind string, id int64, actor string) error {
	c, err := readEditionCycle(ctx, tx, kind, id)
	if err != nil || c == nil {
		return err
	}
	return writeEditionCycle(ctx, tx, *c, "abandoned", c.Remaining, actor)
}
func writeEditionCycle(ctx context.Context, tx *sql.Tx, c subscriptionEditionCycle, state string, remaining []int, actor string) error {
	original := string(mustFuzzyJSON(c))
	next := c
	next.State = state
	next.Remaining = append([]int{}, remaining...)
	if !validEditionCycle(next) {
		return errSubscriptionSubmissionConflict
	}
	if err := fuzzyOne(tx.ExecContext(ctx, `INSERT INTO GO_SUBSCRIPTION_EDITION_ACTIONS(CYCLE_ID,ACTION,DOCUMENT,ACTOR,CREATED) VALUES(?,?,?,?,?)`, c.ID, state, original, actor, time.Now().UTC().Format(time.RFC3339Nano))); err != nil {
		return err
	}
	return fuzzyOne(tx.ExecContext(ctx, `UPDATE GO_SUBSCRIPTION_EDITION_CYCLES SET DOCUMENT=?,STATE=? WHERE ID=? AND DOCUMENT=? AND STATE=?`, string(mustFuzzyJSON(next)), state, c.ID, original, c.State))
}

// Only complete quality cycles advance FILTER_ORDER. The ordinary missing
// ledger is untouched, so existing media never masquerades as a wash receipt.
func persistEditionTx(ctx context.Context, tx *sql.Tx, kind string, id int64, plan subscriptionSearchPlan, candidate *subscriptionCandidate, remaining []int) (bool, *subscriptionEditionSnapshot, error) {
	s := *plan.edition
	c := s.Cycle
	current, err := readEditionCycle(ctx, tx, kind, id)
	if err != nil {
		return false, nil, err
	}
	if candidate == nil {
		if err := fuzzyOne(tx.ExecContext(ctx, "UPDATE "+subscriptionTable(kind)+" SET STATE='R' WHERE ID=?", id)); err != nil {
			return false, nil, err
		}
		return false, &s, nil
	}
	if current == nil || fuzzyDigest(*current) != fuzzyDigest(c) {
		return false, nil, errSubscriptionSubmissionConflict
	}
	state := "active"
	complete := kind == "MOV" || len(remaining) == 0
	if complete {
		state = "completed"
	}
	if err := writeEditionCycle(ctx, tx, c, state, remaining, "native-subscription-receipt"); err != nil {
		return false, nil, err
	}
	if complete {
		if err := fuzzyOne(tx.ExecContext(ctx, "UPDATE "+subscriptionTable(kind)+" SET FILTER_ORDER=?,STATE='R' WHERE ID=?", c.Order, id)); err != nil {
			return false, nil, err
		}
	} else if err := fuzzyOne(tx.ExecContext(ctx, "UPDATE "+subscriptionTable(kind)+" SET STATE='R' WHERE ID=?", id)); err != nil {
		return false, nil, err
	}
	s.Started = true
	s.Cycle.State = state
	s.Cycle.Remaining = append([]int{}, remaining...)
	return complete && c.Order >= c.TopOrder, &s, nil
}
func prepareEdition(ctx context.Context, r fuzzyReader, service subscriptionService, kind string, id int64, plan *subscriptionSearchPlan, input *subscriptionCandidateInput) error {
	current := normalizeNativeSubscriptionRow(plan.raw)
	previous := 0
	var err error
	if value := text(current["filter_order"]); value != "" {
		previous, err = strconv.Atoi(value)
		if err != nil || previous < 0 || previous > 100 {
			return errSubscriptionSubmissionConflict
		}
	}
	input.Upgrade = true
	input.PreviousOrder = previous
	scope, err := nativeSubscriptionConfiguration(ctx, r, service)
	if err != nil {
		return err
	}
	existing, err := readEditionCycle(ctx, r, kind, id)
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.Rule != editionRule(plan.raw) || existing.Scope != scope || existing.Season != plan.season || existing.Total != plan.total || existing.Order <= previous && previous != 0 {
			return errSubscriptionSubmissionConflict
		}
		plan.edition = &subscriptionEditionSnapshot{Cycle: *existing, Started: true, PreviousOrder: previous}
		input.Order = existing.Order
		input.UpgradeGroup = existing.GroupID
		input.Missing = append([]int{}, existing.Remaining...)
	} else {
		nonce, err := refreshNonce()
		if err != nil {
			return err
		}
		target := []int{}
		if kind == "TV" {
			first := 1
			if value := text(current["current_ep"]); value != "" && value != "0" {
				first, err = strconv.Atoi(value)
				if err != nil || first < 1 || first > plan.total {
					return errSubscriptionSubmissionConflict
				}
			}
			for n := first; n <= plan.total; n++ {
				target = append(target, n)
			}
		}
		c := subscriptionEditionCycle{Version: 1, ID: fuzzyDigest([]any{"subscription-edition", kind, id, nonce}), Kind: kind, SubscriptionID: id, Scope: scope, Rule: editionRule(plan.raw), Season: plan.season, Total: plan.total, Target: target, Remaining: append([]int{}, target...), State: "active"}
		c.Name, c.Year = text(current["name"]), text(current["year"])
		plan.edition = &subscriptionEditionSnapshot{Cycle: c, PreviousOrder: previous}
		input.Missing = append([]int{}, target...)
	}
	plan.needed = append([]int{}, input.Missing...)
	plan.Upgrade = editionView(plan.edition)
	return nil
}
func finishEditionPlan(plan *subscriptionSearchPlan) error {
	if plan.edition == nil {
		return nil
	}
	if !plan.edition.Started && len(plan.Candidates) > 0 {
		c := plan.Candidates[0]
		plan.edition.Cycle.Order, plan.edition.Cycle.TopOrder, plan.edition.Cycle.GroupID = c.Priority, c.topOrder, c.groupID
	}
	if len(plan.Candidates) > 0 && !validEditionCycle(plan.edition.Cycle) {
		return errSubscriptionSubmissionConflict
	}
	plan.Upgrade = editionView(plan.edition)
	return nil
}
func copyEditionSnapshot(v *subscriptionEditionSnapshot) *subscriptionEditionSnapshot {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var out subscriptionEditionSnapshot
	if decodeFuzzyDocument(string(b), &out, 1<<20) != nil {
		return nil
	}
	return &out
}

func (api *subscriptionSearchRunner) serveEditionStatus(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.fuzzy.authorize(w, r)
	if !ok {
		return
	}
	kind, id, err := fuzzySelector(r)
	if err != nil {
		fuzzyError(w, err)
		return
	}
	service := api.planner.runner.recognition.service
	var raw map[string]any
	var c *subscriptionEditionCycle
	var scope string
	err = fuzzyReadTransaction(r.Context(), service, func(tx *sql.Tx) error {
		var err error
		raw, err = readFuzzySourceRow(r.Context(), tx, kind, id)
		if err != nil {
			return err
		}
		if raw == nil {
			return errFuzzyMissing
		}
		item := normalizeNativeSubscriptionRow(raw)
		if truthy(item["fuzzy_match"]) || !truthy(item["over_edition"]) {
			return errFuzzyScope
		}
		c, err = readEditionCycle(r.Context(), tx, kind, id)
		if err != nil {
			return err
		}
		scope, err = nativeSubscriptionConfiguration(r.Context(), tx, service)
		return err
	})
	if err != nil {
		fuzzyError(w, err)
		return
	}
	revision := fuzzyDigest([]any{raw, c, scope})
	if r.Method == "POST" {
		var in struct {
			Confirm  bool   `json:"confirm"`
			Revision string `json:"revision"`
			Reason   string `json:"reason"`
		}
		if !decodeOrganizationRequest(w, r, &in, "invalid edition cycle action") {
			return
		}
		if !in.Confirm || in.Revision != revision || c == nil || len(strings.TrimSpace(in.Reason)) == 0 || len(in.Reason) > 512 {
			writeAPIError(w, 409, 409, "fresh whole-cycle review, explicit confirmation and reason required")
			return
		}
		db, err := service.openNativeSubscriptionWriteDatabase()
		if err != nil {
			fuzzyError(w, err)
			return
		}
		defer db.Close()
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			fuzzyError(w, err)
			return
		}
		defer tx.Rollback()
		row, err := readFuzzySourceRow(r.Context(), tx, kind, id)
		if err != nil {
			fuzzyError(w, err)
			return
		}
		current, err := readEditionCycle(r.Context(), tx, kind, id)
		if err != nil {
			fuzzyError(w, err)
			return
		}
		currentScope, err := nativeSubscriptionConfiguration(r.Context(), tx, service)
		if err != nil || fuzzyDigest([]any{row, current, currentScope}) != revision {
			fuzzyError(w, errFuzzyScope)
			return
		}
		pending, err := readPendingSubscriptionSubmission(r.Context(), tx, kind, id)
		if err != nil || pending {
			fuzzyError(w, errFuzzyScope)
			return
		}
		if err := writeEditionCycle(r.Context(), tx, *current, "abandoned", current.Remaining, actor+": "+in.Reason); err != nil {
			fuzzyError(w, err)
			return
		}
		if err := tx.Commit(); err != nil {
			fuzzyError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"cycleId": c.ID, "state": "abandoned", "executionStarted": false}})
		return
	}
	previous, _ := strconv.Atoi(text(raw["FILTER_ORDER"]))
	v := subscriptionEditionView{PreviousOrder: previous, Target: []int{}, Remaining: []int{}}
	valid := true
	if c != nil {
		v = *editionView(&subscriptionEditionSnapshot{Cycle: *c, Started: true, PreviousOrder: previous})
		valid = c.Scope == scope && c.Rule == editionRule(raw)
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"type": kind, "id": strconv.FormatInt(id, 10), "name": text(raw["NAME"]), "scopeValid": valid, "upgrade": v, "revision": revision}})
}
