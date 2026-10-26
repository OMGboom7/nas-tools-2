package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

type fuzzyBatchDefinition struct {
	Version          int
	Kind             string
	SubscriptionID   int64
	Actor, RequestID string
	Grant            fuzzyGrant
	Raw              map[string]any
	Candidates       []fuzzyCandidateRecord
}
type fuzzyBatchItem struct {
	Index        int                `json:"index"`
	Candidate    fuzzyCandidateView `json:"candidate"`
	State        string             `json:"state"`
	Reason       string             `json:"reason"`
	owner, proof string
}
type fuzzyBatch struct {
	ID             string           `json:"id"`
	Kind           string           `json:"kind"`
	SubscriptionID int64            `json:"subscriptionId,string"`
	Name           string           `json:"name"`
	Actor          string           `json:"actor"`
	RequestID      string           `json:"requestId"`
	Fingerprint    string           `json:"fingerprint"`
	State          string           `json:"state"`
	Reason         string           `json:"reason"`
	Created        string           `json:"created"`
	SourceResult   string           `json:"sourceResult"`
	Revision       string           `json:"revision"`
	AuthorizedBy   string           `json:"authorizedBy"`
	Items          []fuzzyBatchItem `json:"items"`
	definition     fuzzyBatchDefinition
	authorization  fuzzyGrant
}
type fuzzyItemProof struct {
	Version int
	BatchID string
	Index   int
	Native  subscriptionSubmissionProof
}

func fuzzyBatchID(d fuzzyBatchDefinition) string {
	return fuzzyDigest([]any{"fuzzy-rss-batch", d.Kind, d.SubscriptionID, d.Actor, d.RequestID})
}
func validFuzzyDefinition(d fuzzyBatchDefinition) bool {
	if d.Version != 1 || d.Grant.Version != 1 || d.Kind != "MOV" && d.Kind != "TV" || d.SubscriptionID < 1 || d.Actor == "" || d.Actor != d.Grant.Actor || d.Kind != d.Grant.Kind || d.SubscriptionID != d.Grant.ID || !fuzzyHex(d.RequestID, 32) || !d.Grant.Authorized || !fuzzyHex(d.Grant.Epoch, 32) || !fuzzyHex(d.Grant.Scope, 64) || !fuzzyHex(d.Grant.Authority, 64) || fuzzyDigest(d.Raw) != d.Grant.Source || text(d.Raw["ID"]) != strconv.FormatInt(d.SubscriptionID, 10) || text(d.Raw["STATE"]) != "R" || !truthy(normalizeNativeSubscriptionRow(d.Raw)["fuzzy_match"]) || d.Candidates == nil || len(d.Candidates) > fuzzyCandidateLimit {
		return false
	}
	raw, err := json.Marshal(d.Raw)
	if err != nil || len(raw) > 1<<20 || len(d.Raw) > 256 {
		return false
	}
	for _, value := range d.Raw {
		if _, err := fuzzySQLValue(value); err != nil {
			return false
		}
	}
	urls, identities := map[string]bool{}, map[string]bool{}
	for _, c := range d.Candidates {
		v := c.View
		key := fuzzyDigest([]string{v.Kind, v.Name, v.Year, v.Season, v.Episodes})
		if v.Priority > 1<<53-1 || v.Priority < -(1<<53-1) || v.TopOrder > 1<<53-1 || v.TopOrder < -(1<<53-1) || v.GroupID < 0 {
			return false
		}
		if v.Kind != d.Kind || v.Name == "" || v.Title != c.Resource.Title || v.Site != c.Resource.Indexer || len(v.Title) > 4096 || len(v.Name) > 1024 || len(v.Year) > 4 || len(v.Season) > 32 || len(v.Episodes) > 64<<10 || c.SiteID < 1 || !fuzzyHex(c.SiteDigest, 64) || !rssDownloadURLAllowed(c.Resource.DownloadURL) || urls[c.Resource.DownloadURL] || identities[key] {
			return false
		}
		urls[c.Resource.DownloadURL], identities[key] = true, true
	}
	b, err := json.Marshal(d)
	return err == nil && len(b) <= 8<<20
}
func fuzzySchema(ctx context.Context, tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_SUBSCRIPTION_GRANTS(KIND TEXT NOT NULL,SUB_ID INTEGER NOT NULL,DOCUMENT TEXT NOT NULL,PRIMARY KEY(KIND,SUB_ID))`,
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_SUBSCRIPTION_BATCHES(ID TEXT PRIMARY KEY,KIND TEXT NOT NULL,SUB_ID INTEGER NOT NULL,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,AUTH_DOCUMENT TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',CREATED TEXT NOT NULL,SOURCE_RESULT TEXT NOT NULL DEFAULT 'none')`,
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_SUBSCRIPTION_ITEMS(BATCH_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL DEFAULT '',OWNER TEXT NOT NULL DEFAULT '',PROOF TEXT NOT NULL DEFAULT '',PRIMARY KEY(BATCH_ID,ORDINAL))`,
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_ITEM_ATTEMPTS(BATCH_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,OWNER TEXT NOT NULL,PAYLOAD TEXT NOT NULL,STATE TEXT NOT NULL,PRIMARY KEY(BATCH_ID,ORDINAL,OWNER))`,
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_SOURCE_ARCHIVES(BATCH_ID TEXT NOT NULL,TABLE_NAME TEXT NOT NULL,DOCUMENT TEXT NOT NULL,PRIMARY KEY(BATCH_ID,TABLE_NAME))`,
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_AUTH_HISTORY(BATCH_ID TEXT NOT NULL,EPOCH TEXT NOT NULL,DOCUMENT TEXT NOT NULL,PRIMARY KEY(BATCH_ID,EPOCH))`,
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_HISTORY_LINKS(HISTORY_ID INTEGER PRIMARY KEY,BATCH_ID TEXT NOT NULL,HISTORY_FINGERPRINT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS GO_FUZZY_RESTORES(REQUEST_ID TEXT PRIMARY KEY,BATCH_ID TEXT NOT NULL,NEW_ID INTEGER NOT NULL,CREATED TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS GO_FUZZY_PENDING ON GO_FUZZY_SUBSCRIPTION_BATCHES(STATE,CREATED,ID)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS GO_FUZZY_ACTIVE_SOURCE ON GO_FUZZY_SUBSCRIPTION_BATCHES(KIND,SUB_ID) WHERE STATE IN('pending','running','blocked','uncertain')`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
func readFuzzyBatch(ctx context.Context, r fuzzyReader, id string) (fuzzyBatch, error) {
	j := fuzzyBatch{Items: []fuzzyBatchItem{}}
	if !fuzzyHex(id, 64) {
		return j, errFuzzyScope
	}
	yes, err := fuzzyTable(ctx, r, "GO_FUZZY_SUBSCRIPTION_BATCHES")
	if err != nil {
		return j, err
	}
	if !yes {
		return j, errFuzzyMissing
	}
	var document, authorization string
	err = r.QueryRowContext(ctx, `SELECT ID,KIND,SUB_ID,DOCUMENT,FINGERPRINT,AUTH_DOCUMENT,STATE,REASON,CREATED,SOURCE_RESULT FROM GO_FUZZY_SUBSCRIPTION_BATCHES WHERE ID=?`, id).Scan(&j.ID, &j.Kind, &j.SubscriptionID, &document, &j.Fingerprint, &authorization, &j.State, &j.Reason, &j.Created, &j.SourceResult)
	if errors.Is(err, sql.ErrNoRows) {
		return j, errFuzzyMissing
	}
	if err != nil {
		return j, err
	}
	if decodeFuzzyDocument(document, &j.definition, 8<<20) != nil || !validFuzzyDefinition(j.definition) || fuzzyBatchID(j.definition) != id || fuzzyDigest(j.definition) != j.Fingerprint || j.Kind != j.definition.Kind || j.SubscriptionID != j.definition.SubscriptionID {
		return j, errFuzzyScope
	}
	if decodeFuzzyDocument(authorization, &j.authorization, 64<<10) != nil || !validFuzzyBatchAuthorization(j.definition, j.authorization) {
		return j, errFuzzyScope
	}
	j.AuthorizedBy = j.authorization.Actor
	switch j.State {
	case "pending", "running", "completed", "blocked", "uncertain", "stopped":
	default:
		return j, errFuzzyScope
	}
	if j.SourceResult != "none" && j.SourceResult != "retained" && j.SourceResult != "finished" {
		return j, errFuzzyScope
	}
	if j.State != "completed" && j.SourceResult != "none" {
		return j, errFuzzyScope
	}
	j.Name, j.Actor, j.RequestID = text(j.definition.Raw["NAME"]), j.definition.Actor, j.definition.RequestID
	rows, err := r.QueryContext(ctx, `SELECT ORDINAL,STATE,REASON,OWNER,PROOF FROM GO_FUZZY_SUBSCRIPTION_ITEMS WHERE BATCH_ID=? ORDER BY ORDINAL`, id)
	if err != nil {
		return j, err
	}
	defer rows.Close()
	for rows.Next() {
		v := fuzzyBatchItem{}
		if err := rows.Scan(&v.Index, &v.State, &v.Reason, &v.owner, &v.proof); err != nil {
			return j, err
		}
		if v.Index != len(j.Items) || v.Index >= len(j.definition.Candidates) {
			return j, errFuzzyScope
		}
		switch v.State {
		case "pending", "intent", "accepted", "uncertain", "rejected":
		default:
			return j, errFuzzyScope
		}
		v.Candidate = j.definition.Candidates[v.Index].View
		if v.State == "pending" && (v.owner != "" || v.proof != "") {
			return j, errFuzzyScope
		}
		if v.State == "intent" || v.State == "accepted" || v.State == "uncertain" || v.State == "rejected" {
			if !fuzzyHex(v.owner, 32) {
				return j, errFuzzyScope
			}
			if _, err := decodeFuzzyItemProof(v.proof, j, v.Index); err != nil {
				return j, err
			}
		}
		if j.State == "completed" && v.State != "accepted" {
			return j, errFuzzyScope
		}
		if j.State == "pending" && v.State != "pending" && v.State != "accepted" {
			return j, errFuzzyScope
		}
		j.Items = append(j.Items, v)
	}
	if err := rows.Err(); err != nil {
		return j, err
	}
	if len(j.Items) != len(j.definition.Candidates) {
		return j, errFuzzyScope
	}
	if j.State == "completed" && (j.SourceResult == "none" || j.SourceResult == "finished" && len(j.Items) == 0) {
		return j, errFuzzyScope
	}
	private := []any{}
	for _, v := range j.Items {
		private = append(private, []any{v.Index, v.State, v.Reason, v.owner, v.proof})
	}
	j.Revision = fuzzyDigest([]any{j.Fingerprint, j.authorization, j.State, j.Reason, j.SourceResult, private})
	return j, nil
}
func validFuzzyBatchAuthorization(d fuzzyBatchDefinition, g fuzzyGrant) bool {
	return g.Version == 1 && g.Authorized && g.Kind == d.Kind && g.ID == d.SubscriptionID && g.Actor != "" && fuzzyHex(g.Authority, 64) && fuzzyHex(g.Epoch, 32) && g.Source == d.Grant.Source && g.Scope == d.Grant.Scope
}
func decodeFuzzyItemProof(raw string, j fuzzyBatch, index int) (fuzzyItemProof, error) {
	p := fuzzyItemProof{}
	if decodeFuzzyDocument(raw, &p, 3<<20) != nil || p.Version != 2 || p.BatchID != j.ID || p.Index != index || index < 0 || index >= len(j.definition.Candidates) {
		return p, errFuzzyScope
	}
	d, c := j.definition, j.definition.Candidates[index]
	n := p.Native
	if n.Version != 1 || n.DownloaderID < 1 || !fuzzyHex(n.ConfigHash, 64) || n.MediaID != "fuzzy:"+d.Kind+":"+strconv.FormatInt(d.SubscriptionID, 10) || n.Title != c.View.Title || n.DownloadURL != c.Resource.DownloadURL || len(n.Needed) != 0 || len(n.StoredMissing) != 0 || len(n.Episodes) != 0 || n.Total != 0 || n.Season != 0 {
		return p, errFuzzyScope
	}
	var source map[string]any
	if decodeFuzzyDocument(string(n.Raw), &source, 1<<20) != nil || fuzzyDigest(source) != d.Grant.Source {
		return p, errFuzzyScope
	}
	return p, nil
}
func fuzzyReadTransaction(ctx context.Context, service subscriptionService, read func(*sql.Tx) error) error {
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return read(tx)
}
func (api *fuzzySubscriptionAPI) get(ctx context.Context, id string) (fuzzyBatch, error) {
	var j fuzzyBatch
	err := fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error { var err error; j, err = readFuzzyBatch(ctx, tx, id); return err })
	return j, err
}
func (api *fuzzySubscriptionAPI) list(ctx context.Context, kind string, id int64, pending bool) ([]fuzzyBatch, error) {
	out := []fuzzyBatch{}
	err := fuzzyReadTransaction(ctx, api.service(), func(tx *sql.Tx) error {
		yes, err := fuzzyTable(ctx, tx, "GO_FUZZY_SUBSCRIPTION_BATCHES")
		if err != nil || !yes {
			return err
		}
		query, args := `SELECT ID FROM GO_FUZZY_SUBSCRIPTION_BATCHES WHERE KIND=? AND SUB_ID=? ORDER BY CREATED DESC,ID LIMIT 30`, []any{kind, id}
		if pending {
			query, args = `SELECT ID FROM GO_FUZZY_SUBSCRIPTION_BATCHES WHERE STATE='pending' ORDER BY CREATED,ID LIMIT 5`, nil
		} else if kind == "" && id == 0 {
			query, args = `SELECT ID FROM GO_FUZZY_SUBSCRIPTION_BATCHES ORDER BY CREATED DESC,ID LIMIT 30`, nil
		}
		rows, err := tx.QueryContext(ctx, query, args...)
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
			j, err := readFuzzyBatch(ctx, tx, id)
			if err != nil {
				return err
			}
			out = append(out, j)
		}
		return nil
	})
	return out, err
}
func (api *fuzzySubscriptionAPI) current(ctx context.Context, r fuzzyReader, g fuzzyGrant) error {
	s, err := api.source(ctx, r, g.Kind, g.ID, g.Actor)
	if err != nil {
		return err
	}
	if !fuzzyGrantCurrent(s, g) {
		return errFuzzyScope
	}
	return nil
}
func (api *fuzzySubscriptionAPI) create(ctx context.Context, d fuzzyBatchDefinition) (fuzzyBatch, error) {
	if !validFuzzyDefinition(d) {
		return fuzzyBatch{}, errFuzzyScope
	}
	db, err := api.service().openNativeSubscriptionWriteDatabase()
	if err != nil {
		return fuzzyBatch{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fuzzyBatch{}, err
	}
	defer tx.Rollback()
	id := fuzzyBatchID(d)
	if old, err := readFuzzyBatch(ctx, tx, id); err == nil {
		if old.Fingerprint != fuzzyDigest(d) {
			return old, errFuzzyScope
		}
		return old, nil
	} else if !errors.Is(err, errFuzzyMissing) {
		return fuzzyBatch{}, err
	}
	if err := api.current(ctx, tx, d.Grant); err != nil {
		return fuzzyBatch{}, err
	}
	if err := verifyFuzzySource(ctx, tx, d.Kind, d.SubscriptionID, d.Raw); err != nil {
		return fuzzyBatch{}, err
	}
	if err := fuzzySchema(ctx, tx); err != nil {
		return fuzzyBatch{}, err
	}
	document, err := json.Marshal(d)
	if err != nil {
		return fuzzyBatch{}, err
	}
	authorization, _ := json.Marshal(d.Grant)
	result, err := tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_SUBSCRIPTION_BATCHES(ID,KIND,SUB_ID,DOCUMENT,FINGERPRINT,AUTH_DOCUMENT,STATE,CREATED) VALUES(?,?,?,?,?,?,'pending',?)`, id, d.Kind, d.SubscriptionID, string(document), fuzzyDigest(d), string(authorization), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fuzzyBatch{}, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return fuzzyBatch{}, errFuzzyScope
	}
	if result, err := tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_AUTH_HISTORY(BATCH_ID,EPOCH,DOCUMENT) VALUES(?,?,?)`, id, d.Grant.Epoch, string(authorization)); err != nil {
		return fuzzyBatch{}, err
	} else if n, e := result.RowsAffected(); e != nil || n != 1 {
		return fuzzyBatch{}, errFuzzyScope
	}
	for i := range d.Candidates {
		result, err = tx.ExecContext(ctx, `INSERT INTO GO_FUZZY_SUBSCRIPTION_ITEMS(BATCH_ID,ORDINAL,STATE) VALUES(?,?,'pending')`, id, i)
		if err != nil {
			return fuzzyBatch{}, err
		}
		if n, e := result.RowsAffected(); e != nil || n != 1 {
			return fuzzyBatch{}, errFuzzyScope
		}
	}
	j, err := readFuzzyBatch(ctx, tx, id)
	if err != nil {
		return j, err
	}
	return j, tx.Commit()
}
func (api *fuzzySubscriptionAPI) start(ctx context.Context, id string) (fuzzyBatch, error) {
	db, err := api.service().openNativeSubscriptionWriteDatabase()
	if err != nil {
		return fuzzyBatch{}, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fuzzyBatch{}, err
	}
	defer tx.Rollback()
	j, err := readFuzzyBatch(ctx, tx, id)
	if err != nil {
		return j, err
	}
	if j.State != "pending" {
		return j, errFuzzyScope
	}
	if err := api.current(ctx, tx, j.authorization); err != nil {
		return j, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_BATCHES SET STATE='running' WHERE ID=? AND STATE='pending'`, id)
	if err != nil {
		return j, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return j, errFuzzyScope
	}
	j.State = "running"
	return j, tx.Commit()
}
func (api *fuzzySubscriptionAPI) finishState(ctx context.Context, id, state, reason string) error {
	if state != "blocked" && state != "uncertain" && state != "stopped" {
		return errFuzzyScope
	}
	db, err := api.service().openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := readFuzzyBatch(ctx, tx, id)
	if err != nil {
		return err
	}
	if j.State == "stopped" {
		return nil
	}
	if j.State == "completed" {
		return errFuzzyScope
	}
	if state != "stopped" && j.State != "running" && j.State != "pending" {
		return errFuzzyScope
	}
	if state == "stopped" {
		current, _, readErr := readFuzzyGrant(ctx, tx, j.Kind, j.SubscriptionID)
		if readErr == nil && current.Source == j.definition.Grant.Source && current.Scope == j.definition.Grant.Scope {
			if err := revokeFuzzyGrantTx(ctx, tx, j.Kind, j.SubscriptionID); err != nil {
				return err
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE GO_FUZZY_SUBSCRIPTION_BATCHES SET STATE=?,REASON=? WHERE ID=? AND STATE IN('pending','running','blocked','uncertain')`, state, reason, id)
	if err != nil {
		return err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		return errFuzzyScope
	}
	return tx.Commit()
}
