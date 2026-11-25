package httpserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/iyuu"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type iyuuSeedClient interface {
	torrenttransfer.TargetClient
	TransferWireID(context.Context, torrentmeta.TransferMetadata) (string, error)
	AddReseedPaused(context.Context, []byte, string, torrenttransfer.AddIntent) (torrenttransfer.AddReceipt, error)
}
type iyuuJob struct {
	Receipt, Actor, Account, Scope, Directory string
	Batch                                     string `json:",omitempty"`
	Match                                     iyuuMatch
	Meta                                      torrentmeta.TransferMetadata
	TargetProof                               string
	WasIncomplete                             bool
}
type iyuuSeedView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Downloader string `json:"downloader"`
	Site       string `json:"site"`
	State      string `json:"state"`
}

func (j iyuuJob) view(state string) iyuuSeedView {
	return iyuuSeedView{j.Receipt, j.Match.Name, j.Match.Downloader, j.Match.Site, state}
}

func (api *iyuuAPI) seedDB(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.transfer.database, RawQuery: url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(5000)"}}.Encode()}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_IYUU_SEEDS (id TEXT PRIMARY KEY, receiver TEXT NOT NULL, hash TEXT NOT NULL, account TEXT NOT NULL, state TEXT NOT NULL, job TEXT NOT NULL); CREATE UNIQUE INDEX IF NOT EXISTS GO_IYUU_SEEDS_TARGET ON GO_IYUU_SEEDS(receiver,hash) WHERE state <> 'unsent'`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
func (api *iyuuAPI) listSeeds(w http.ResponseWriter, r *http.Request) {
	s, ok := api.requestScope(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	db, err := api.seedDB(ctx)
	if err != nil {
		writeAPIError(w, 503, 503, "辅种记录不可用")
		return
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT state,job FROM GO_IYUU_SEEDS WHERE account=? ORDER BY rowid DESC LIMIT 100", s.account())
	if err != nil {
		writeAPIError(w, 503, 503, "辅种记录不可用")
		return
	}
	defer rows.Close()
	out := []iyuuSeedView{}
	for rows.Next() {
		var state, raw string
		var j iyuuJob
		if rows.Scan(&state, &raw) != nil || json.Unmarshal([]byte(raw), &j) != nil {
			writeAPIError(w, 503, 503, "辅种记录无效")
			return
		}
		out = append(out, j.view(state))
	}
	if rows.Err() != nil {
		writeAPIError(w, 503, 503, "读取辅种记录失败")
		return
	}
	signinJSON(w, out)
}

type iyuuSeedScope struct {
	d      downloaderconfig.Downloader
	site   siteconfig.Site
	client iyuuSeedClient
	proof  string
}

func (api *iyuuAPI) seedScope(ctx context.Context, s iyuuScope, m iyuuMatch) (iyuuSeedScope, error) {
	var result iyuuSeedScope
	did, err := syncConfigID(m.DownloaderID)
	if err != nil || !slices.Contains(s.cfg.Downloaders, m.DownloaderID) {
		return result, iyuu.ErrInput
	}
	sid, err := syncConfigID(m.SiteID)
	if err != nil || len(s.cfg.Sites) > 0 && !slices.Contains(s.cfg.Sites, m.SiteID) {
		return result, iyuu.ErrInput
	}
	d, err := api.transfer.downloaders.Get(ctx, did)
	if err != nil || d.Enabled != 1 {
		return result, iyuu.ErrInput
	}
	site, err := api.transfer.login.sites.Get(ctx, sid)
	if err != nil {
		return result, iyuu.ErrInput
	}
	c, err := api.transfer.organizer.clientFromDownloader(d)
	if err != nil {
		return result, err
	}
	client, ok := c.transfer.(iyuuSeedClient)
	if !ok {
		return result, iyuu.ErrInput
	}
	proof := sitelogin.Hash([]any{s.actor, s.authority, s.account(), s.raw[iyuuGeneration], s.cfg.Downloaders, s.cfg.Sites, s.cfg.Excluded, d, site})
	return iyuuSeedScope{d, site, client, proof}, nil
}
func (api *iyuuAPI) seedSource(ctx context.Context, j iyuuJob, c iyuuSeedScope) (torrenttransfer.Task, error) {
	s, err := api.scope(ctx, j.Actor)
	if err != nil {
		return torrenttransfer.Task{}, err
	}
	now, err := api.seedScope(ctx, s, j.Match)
	if err != nil || c.proof != now.proof || j.Scope != now.proof || s.cfg.Permanent[j.Match.Hash] || s.cfg.Failed[j.Match.Hash] {
		return torrenttransfer.Task{}, iyuu.ErrInput
	}
	task, err := c.client.TransferTask(ctx, j.Match.SourceID)
	if err != nil {
		return task, err
	}
	if task.V1Hash != j.Match.Source || !iyuuSourceEligible(task, s.cfg) || iyuuSourceProof(task) != j.Match.SourceProof {
		return task, iyuu.ErrInput
	}
	return task, nil
}
func iyuuPayloadMatches(task torrenttransfer.Task, meta torrentmeta.TransferMetadata) bool {
	if !task.Valid() || !task.Complete || task.Error || task.Checking || !torrentfiles.MatchTransfer(meta.Files, task.Files) {
		return false
	}
	done := map[int]bool{}
	for _, id := range task.Done {
		done[id] = true
	}
	padding := map[string]bool{}
	for _, f := range meta.Files {
		padding[f.Path] = f.Padding
	}
	for _, f := range task.Files {
		if !padding[f.Path] && (!f.Selected || !done[f.Index]) {
			return false
		}
	}
	return true
}
func (api *iyuuAPI) submitSeed(w http.ResponseWriter, r *http.Request) {
	s, ok := api.requestScope(w, r)
	if !ok {
		return
	}
	if api.transfer.organizer.jobs == nil {
		writeAPIError(w, 503, 503, "当前环境无法锁定下载器任务")
		return
	}
	var in struct {
		Fingerprint string
		Candidate   iyuuMatch
	}
	if !siteLoginJSON(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	sel, err := api.selection(ctx, s)
	m := in.Candidate
	if err != nil || sel.proof != in.Fingerprint || !iyuu.Hash(m.SourceID) || !iyuu.Hash(m.Source) || !iyuu.Hash(m.Hash) || m.Source == m.Hash || !refreshID(m.SourceProof, 64) {
		writeAPIError(w, 409, 409, "候选或设置已变化，请重新匹配")
		return
	}
	result, err := api.submitCandidate(ctx, s, sel, m, "")
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errIyuuStore) {
			status = http.StatusServiceUnavailable
		}
		writeAPIError(w, status, status, "未确认添加，请核对候选、站点取种、源文件和辅种记录")
		return
	}
	signinJSON(w, result)
}

var errIyuuStore = errors.New("IYUU seed records unavailable")

func (api *iyuuAPI) submitCandidate(ctx context.Context, s iyuuScope, sel iyuuSelection, m iyuuMatch, batch string) (iyuuSeedView, error) {
	var empty iyuuSeedView
	c, err := api.seedScope(ctx, s, m)
	if err != nil {
		return empty, err
	}
	g, err := api.transfer.organizer.jobs.AcquireJob(ctx, torrentLabelGuard(c.d.ID, m.Hash))
	if err != nil {
		return empty, err
	}
	defer g.Close()
	db, err := api.seedDB(ctx)
	if err != nil {
		return empty, errors.Join(errIyuuStore, err)
	}
	defer db.Close()
	receiver := torrenttransfer.Hash(c.client.Address())
	var raw, state string
	err = db.QueryRowContext(ctx, "SELECT job,state FROM GO_IYUU_SEEDS WHERE receiver=? AND hash=? AND state<>'unsent'", receiver, m.Hash).Scan(&raw, &state)
	if err == nil {
		var j iyuuJob
		if json.Unmarshal([]byte(raw), &j) != nil || j.Account != s.account() || j.Scope != c.proof || j.Match.SourceProof != m.SourceProof {
			return empty, iyuu.ErrInput
		}
		if batch != "" && j.Batch != batch && state != "adding" && state != "unknown" {
			state = "duplicate"
		}
		return j.view(state), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, errors.Join(errIyuuStore, err)
	}
	return api.addSeed(ctx, db, s, c, sel, m, receiver, batch)
}
func (api *iyuuAPI) addSeed(ctx context.Context, db *sql.DB, s iyuuScope, c iyuuSeedScope, sel iyuuSelection, m iyuuMatch, receiver, batch string) (iyuuSeedView, error) {
	var empty iyuuSeedView
	if s.cfg.Success[m.Hash] {
		return empty, iyuu.ErrInput
	}
	receipt, err := refreshNonce()
	if err != nil {
		return empty, err
	}
	j := iyuuJob{Batch: batch, Receipt: sitelogin.Hash(receipt), Actor: s.actor, Account: s.account(), Scope: c.proof, Match: m}
	src, err := api.seedSource(ctx, j, c)
	if err != nil {
		return empty, err
	}
	j.Match.Name, j.Match.Downloader, j.Match.Site = src.Name, c.d.Name, c.site.Name
	if _, err = c.client.TransferTask(ctx, m.Hash); batch != "" && err == nil {
		return j.view("duplicate"), nil
	} else if !errors.Is(err, torrenttransfer.ErrTaskAbsent) {
		return empty, iyuu.ErrInput
	}
	guard := func(ctx context.Context) error {
		live, err := api.scope(ctx, s.actor)
		if err != nil || live.raw[iyuuControlKey] != s.raw[iyuuControlKey] || live.cfg.Success[m.Hash] || live.cfg.Failed[m.Hash] || live.cfg.Permanent[m.Hash] {
			return iyuu.ErrInput
		}
		fresh, err := api.selection(ctx, live)
		if err != nil || fresh.proof != sel.proof {
			return iyuu.ErrInput
		}
		return nil
	}
	matcher := iyuuMatcher{scope: s, sites: sel.sites, current: func() error { return guard(ctx) }}
	if err := matcher.prepare(ctx); err != nil {
		return empty, err
	}
	if matcher.targets[m.ProviderID].ID != c.site.ID {
		return empty, iyuu.ErrInput
	}
	if err := guard(ctx); err != nil {
		return empty, err
	}
	matches, err := s.client.Match(ctx, []string{m.Source}, matcher.signature, time.Now())
	if err != nil {
		return empty, err
	}
	if !slices.ContainsFunc(matches[m.Source], func(candidate iyuu.Candidate) bool {
		return candidate.SiteID.String() == m.ProviderID && candidate.TorrentID.String() == m.TorrentID && candidate.Hash == m.Hash
	}) {
		return empty, iyuu.ErrInput
	}
	body, err := api.seedContent(ctx, c.site, matcher.providers[m.ProviderID], m.TorrentID, guard)
	if err != nil {
		return empty, err
	}
	meta, err := torrentmeta.PrepareTransfer(body, nil)
	if err != nil || meta.V1Hash != m.Hash || meta.TrackerCount == 0 || !iyuuPayloadMatches(src, meta) {
		return empty, iyuu.ErrInput
	}
	id, err := c.client.TransferWireID(ctx, meta)
	if err != nil || id != m.Hash {
		return empty, iyuu.ErrInput
	}
	dir, err := torrenttransfer.NormalizeRemote(src.Directory)
	if err != nil {
		return empty, err
	}
	j.Directory = dir
	j.Meta = meta
	j.Meta.Content = nil
	j.Meta.TrackerURLs = nil
	ledger, err := torrenttransfer.NewLedger(ctx, db)
	if err != nil {
		return empty, err
	}
	digest := sha256.Sum256(meta.Content)
	intent := torrenttransfer.AddIntent{Ledger: ledger, Receipt: j.Receipt, ScopeProof: j.Scope, Receiver: receiver, Directory: dir, WireID: id, ContentDigest: hex.EncodeToString(digest[:]), Revalidate: func(ctx context.Context) error {
		if err := guard(ctx); err != nil {
			return err
		}
		src, err := api.seedSource(ctx, j, c)
		if err != nil || !iyuuPayloadMatches(src, meta) {
			return iyuu.ErrInput
		}
		return nil
	}}
	b, err := json.Marshal(j)
	if err != nil {
		return empty, err
	}
	_, err = db.ExecContext(ctx, "INSERT INTO GO_IYUU_SEEDS(id,receiver,hash,account,state,job) VALUES(?,?,?,?,'adding',?)", j.Receipt, receiver, id, j.Account, string(b))
	if err != nil {
		return empty, err
	}
	reply, sendErr := c.client.AddReseedPaused(ctx, meta.Content, dir, intent)
	state := "added"
	if sendErr != nil || reply.WireID != id {
		state = "unknown"
	} else if reply.Duplicate {
		state = "duplicate"
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if state == "unknown" {
		state = iyuuAddState(save, ledger, receiver, j)
	}
	if err := iyuuSaveStage(save, db, j, "adding", state); err != nil {
		return empty, errors.Join(errIyuuStore, err)
	}
	return j.view(state), nil
}

func iyuuSaveStage(ctx context.Context, db *sql.DB, j iyuuJob, old, next string) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	res, err := db.ExecContext(ctx, "UPDATE GO_IYUU_SEEDS SET state=?,job=? WHERE id=? AND state=?", next, string(raw), j.Receipt, old)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return iyuu.ErrInput
	}
	return nil
}

// A missing action claim proves the add RPC was never sent. A pending claim
// survives a lost response and must never be treated as permission to retry.
func iyuuAddState(ctx context.Context, l *torrenttransfer.Ledger, receiver string, j iyuuJob) string {
	key := torrenttransfer.ActionKey{Receiver: receiver, WireID: j.Match.Hash, Action: "add"}
	action, err := l.Lookup(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "unsent"
	}
	if err != nil || action.State != "acknowledged" || action.Receipt != j.Receipt {
		return "unknown"
	}
	reply, err := l.AddReply(ctx, key, j.Receipt)
	if err != nil || reply.WireID != j.Match.Hash {
		return "unknown"
	}
	if reply.Duplicate {
		return "duplicate"
	}
	return "added"
}
