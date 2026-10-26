package httpserver

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
)

type editionAttemptPublic struct {
	Title    string `json:"title"`
	Order    int    `json:"order"`
	Episodes []int  `json:"episodes"`
	State    string `json:"state"`
}
type editionReceipt struct {
	ID             string                  `json:"id"`
	Kind           string                  `json:"type"`
	SubscriptionID string                  `json:"subscriptionId"`
	Name           string                  `json:"name"`
	Year           string                  `json:"year"`
	State          string                  `json:"state"`
	SourceMatches  bool                    `json:"sourceMatches"`
	Progress       subscriptionEditionView `json:"progress"`
	Attempts       []editionAttemptPublic  `json:"attempts"`
}

func readEditionReceipt(ctx context.Context, r fuzzyReader, id string) (editionReceipt, error) {
	out := editionReceipt{Attempts: []editionAttemptPublic{}}
	if !fuzzyHex(id, 64) {
		return out, errFuzzyScope
	}
	yes, err := fuzzyTable(ctx, r, "GO_SUBSCRIPTION_EDITION_CYCLES")
	if err != nil {
		return out, err
	}
	if !yes {
		return out, errFuzzyMissing
	}
	var raw, kind, state string
	var subscriptionID int64
	err = r.QueryRowContext(ctx, `SELECT KIND,SUB_ID,DOCUMENT,STATE FROM GO_SUBSCRIPTION_EDITION_CYCLES WHERE ID=?`, id).Scan(&kind, &subscriptionID, &raw, &state)
	if err == sql.ErrNoRows {
		return out, errFuzzyMissing
	}
	if err != nil {
		return out, err
	}
	var c subscriptionEditionCycle
	if decodeFuzzyDocument(raw, &c, 1<<20) != nil || !validEditionCycle(c) || c.ID != id || c.Kind != kind || c.SubscriptionID != subscriptionID || c.State != state {
		return out, errFuzzyScope
	}
	out.ID, out.Kind, out.SubscriptionID, out.Name, out.Year, out.State = id, kind, strconv.FormatInt(subscriptionID, 10), c.Name, c.Year, c.State
	out.Progress = *editionView(&subscriptionEditionSnapshot{Cycle: c, Started: true})
	source, err := readFuzzySourceRow(ctx, r, kind, subscriptionID)
	if err == nil && source != nil && c.State == "active" {
		out.SourceMatches = editionRule(source) == c.Rule
	}
	rows, err := r.QueryContext(ctx, `SELECT PAYLOAD,STATE FROM GO_SUBSCRIPTION_EDITION_ATTEMPTS WHERE CYCLE_ID=? ORDER BY rowid LIMIT 1001`, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Attempts) >= 1000 {
			return out, errFuzzyScope
		}
		var body, state string
		if err := rows.Scan(&body, &state); err != nil {
			return out, err
		}
		p, _, err := decodeSubscriptionProof(body)
		if err != nil || p.Edition == nil || p.Edition.Cycle.ID != id || editionIdentity(p.Edition.Cycle) != editionIdentity(c) || state != "intent" && state != "accepted" && state != "rejected" {
			return out, errFuzzyScope
		}
		out.Attempts = append(out.Attempts, editionAttemptPublic{p.Title, p.Priority, append([]int{}, p.Episodes...), state})
	}
	return out, rows.Err()
}
func (api *subscriptionSearchRunner) serveEditionReceipts(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.fuzzy.authorize(w, r); !ok {
		return
	}
	service := api.planner.runner.recognition.service
	if id := r.PathValue("cycle"); id != "" {
		var out editionReceipt
		err := fuzzyReadTransaction(r.Context(), service, func(tx *sql.Tx) error { var err error; out, err = readEditionReceipt(r.Context(), tx, id); return err })
		if err != nil {
			fuzzyError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": out})
		return
	}
	out := []editionReceipt{}
	err := fuzzyReadTransaction(r.Context(), service, func(tx *sql.Tx) error {
		yes, err := fuzzyTable(r.Context(), tx, "GO_SUBSCRIPTION_EDITION_CYCLES")
		if err != nil || !yes {
			return err
		}
		rows, err := tx.QueryContext(r.Context(), `SELECT ID FROM GO_SUBSCRIPTION_EDITION_CYCLES ORDER BY rowid DESC LIMIT 30`)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range ids {
			v, err := readEditionReceipt(r.Context(), tx, id)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		fuzzyError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": out})
}
func (api *subscriptionSearchRunner) editionFallback(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.fuzzy.authorize(w, r); ok {
		writeAPIError(w, 405, 405, "unsupported native edition request")
	}
}
func closeSelectedEditionCyclesTx(ctx context.Context, tx *sql.Tx, kind, predicate string, args ...any) error {
	yes, err := fuzzyTable(ctx, tx, "GO_SUBSCRIPTION_EDITION_CYCLES")
	if err != nil || !yes {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT ID FROM "+subscriptionTable(kind)+" WHERE "+predicate, args...)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if len(ids) >= 10000 {
			rows.Close()
			return errFuzzyScope
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := closeEditionCycleTx(ctx, tx, kind, id, "legacy-rule-delete"); err != nil {
			return err
		}
	}
	return nil
}
