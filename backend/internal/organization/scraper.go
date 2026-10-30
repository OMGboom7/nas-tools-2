package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

type SidecarProof struct {
	Target, Temp, Identity, ParentIdentity, StagingIdentity, Digest string
	Size                                                            int64
}
type SidecarRecord struct {
	Name, Scope, State, Reason string
	Proof                      SidecarProof
}
type ScrapeResult struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}
type ScrapeReceipt struct {
	ID                      string          `json:"id"`
	JobID                   string          `json:"jobId"`
	Ordinal                 int             `json:"ordinal"`
	Name                    string          `json:"name"`
	State                   string          `json:"state"`
	Reason                  string          `json:"reason,omitempty"`
	Updated                 string          `json:"updated"`
	Results                 []ScrapeResult  `json:"results"`
	Origin, Config, Options string          `json:"-"`
	Outputs                 []SidecarRecord `json:"-"`
}

func ScrapeConfigDigest(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return Digest(v)
	}
	return Digest(raw)
}
func scrapeSetting(ctx context.Context, tx *sql.Tx) (string, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='SYSTEM_DICT')`).Scan(&exists); err != nil || !exists {
		return "", err
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1`, scraper.ConfigKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return raw, err
}
func emitScrape(ctx context.Context, tx *sql.Tx, jobID string, ordinal int, name, origin string) error {
	raw, err := scrapeSetting(ctx, tx)
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	state, reason := "pending", ""
	if _, _, err := scraper.Parse(raw); err != nil {
		state, reason = "blocked", "invalid scraper configuration; media intake was preserved"
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SCRAPE_JOBS(ID TEXT PRIMARY KEY,JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,NAME TEXT NOT NULL,ORIGIN TEXT NOT NULL,CONFIG TEXT NOT NULL,OPTIONS TEXT NOT NULL,STATE TEXT NOT NULL,REASON TEXT NOT NULL,OUTPUTS TEXT NOT NULL DEFAULT '[]',UPDATED TEXT NOT NULL,UNIQUE(JOB_ID,ORDINAL))`); err != nil {
		return err
	}
	id := Digest([]string{jobID, strconv.Itoa(ordinal)})
	res, err := tx.ExecContext(ctx, `INSERT INTO GO_SCRAPE_JOBS(ID,JOB_ID,ORDINAL,NAME,ORIGIN,CONFIG,OPTIONS,STATE,REASON,UPDATED) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(ID) DO NOTHING`, id, jobID, ordinal, name, origin, ScrapeConfigDigest(raw), raw, state, reason, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var same bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SCRAPE_JOBS WHERE ID=? AND JOB_ID=? AND ORDINAL=? AND ORIGIN=?)`, id, jobID, ordinal, origin).Scan(&same); err != nil {
			return err
		}
		if !same {
			return ErrState
		}
	}
	return nil
}

const scrapeSelect = `SELECT ID,JOB_ID,ORDINAL,NAME,ORIGIN,CONFIG,OPTIONS,STATE,REASON,OUTPUTS,UPDATED FROM GO_SCRAPE_JOBS`

func scanScrape(row interface{ Scan(...any) error }) (ScrapeReceipt, error) {
	r := ScrapeReceipt{Results: []ScrapeResult{}}
	var outputs string
	err := row.Scan(&r.ID, &r.JobID, &r.Ordinal, &r.Name, &r.Origin, &r.Config, &r.Options, &r.State, &r.Reason, &outputs, &r.Updated)
	if err != nil {
		return r, err
	}
	if json.Unmarshal([]byte(outputs), &r.Outputs) != nil {
		return r, ErrState
	}
	for _, o := range r.Outputs {
		r.Results = append(r.Results, ScrapeResult{Name: o.Name, State: o.State, Reason: o.Reason})
	}
	return r, nil
}
func (s *Store) Scrapes(ctx context.Context, pending bool) ([]ScrapeReceipt, error) {
	result := []ScrapeReceipt{}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SCRAPE_JOBS')`).Scan(&exists); err != nil || !exists {
		return result, err
	}
	q := scrapeSelect + ` ORDER BY STATE!='completed' DESC,UPDATED DESC,ID LIMIT 100`
	if pending {
		q = scrapeSelect + ` WHERE STATE='pending' ORDER BY UPDATED,ID LIMIT 10`
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanScrape(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Store) ScrapePending(ctx context.Context, jobID string, ordinal int) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='GO_SCRAPE_JOBS')`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SCRAPE_JOBS WHERE JOB_ID=? AND ORDINAL=? AND STATE='pending')`, jobID, ordinal).Scan(&exists)
	return exists, err
}
func scrapeDigest(r ScrapeReceipt) string {
	return Digest([]any{r, r.Origin, r.Config, r.Options, r.Outputs})
}
func (s *Store) ClaimScrape(ctx context.Context, expected ScrapeReceipt) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := scanScrape(tx.QueryRowContext(ctx, scrapeSelect+` WHERE ID=?`, expected.ID))
	if err != nil {
		return err
	}
	if r.State != "pending" || scrapeDigest(r) != scrapeDigest(expected) {
		return ErrState
	}
	raw, err := scrapeSetting(ctx, tx)
	if err != nil {
		return err
	}
	state, reason := "running", ""
	if ScrapeConfigDigest(raw) != r.Config {
		state, reason = "blocked", "scraper settings changed; old intake scope was not replayed"
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_SCRAPE_JOBS SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='pending'`, state, reason, time.Now().UTC().Format(time.RFC3339Nano), r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_SCRAPE_JOBS WHERE ID=?`, r.ID).Scan(&stored); err != nil || stored != state {
		return ErrState
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if state == "blocked" {
		return ErrState
	}
	return nil
}

// Output intent is committed before sidecar bytes or publication. A running
// receipt is never automatically replayed after a crash or a lost write result.
func (s *Store) SaveScrapeOutput(ctx context.Context, id string, out SidecarRecord) error {
	if out.Name == "" || filepath.Base(out.Name) != out.Name || len(out.Name) > 1024 || out.Scope != "media" && out.Scope != "show" || out.State != "writing" && out.State != "created" && out.State != "preserved" && out.State != "blocked" && out.State != "uncertain" {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := scanScrape(tx.QueryRowContext(ctx, scrapeSelect+` WHERE ID=?`, id))
	if err != nil {
		return err
	}
	if r.State != "running" {
		return ErrState
	}
	if out.State == "writing" && strings.HasPrefix(out.Proof.Identity, "remote:") {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT DEFINITION FROM GO_ORGANIZATION_JOBS WHERE ID=?`, r.JobID).Scan(&raw); err != nil {
			return err
		}
		var d Definition
		if json.Unmarshal([]byte(raw), &d) != nil {
			return ErrState
		}
		if d.Remote != nil {
			if err := checkRemoteMetadataClaims(ctx, tx, d.TargetRoot, out.Proof.Target, "", id); err != nil {
				return err
			}
		}
	}
	found := false
	for i, old := range r.Outputs {
		if old.Name == out.Name && old.Scope == out.Scope {
			if old.State != "writing" || out.State == "writing" {
				return ErrState
			}
			r.Outputs[i] = out
			found = true
			break
		}
	}
	if !found {
		if len(r.Outputs) >= 64 {
			return ErrState
		}
		r.Outputs = append(r.Outputs, out)
	}
	b, err := json.Marshal(r.Outputs)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_SCRAPE_JOBS SET OUTPUTS=?,UPDATED=? WHERE ID=? AND STATE='running'`, string(b), time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT OUTPUTS FROM GO_SCRAPE_JOBS WHERE ID=?`, id).Scan(&stored); err != nil || stored != string(b) {
		return ErrState
	}
	return tx.Commit()
}
func (s *Store) FinishScrape(ctx context.Context, id, state, reason string) error {
	if state != "completed" && state != "partial" && state != "blocked" || len(reason) > 4000 {
		return ErrState
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	original, err := scanScrape(tx.QueryRowContext(ctx, scrapeSelect+` WHERE ID=?`, id))
	if err != nil {
		return err
	}
	if original.State != "running" {
		return ErrState
	}
	res, err := tx.ExecContext(ctx, `UPDATE GO_SCRAPE_JOBS SET STATE=?,REASON=?,UPDATED=? WHERE ID=? AND STATE='running'`, state, reason, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrState
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT STATE FROM GO_SCRAPE_JOBS WHERE ID=?`, id).Scan(&stored); err != nil || stored != state {
		return ErrState
	}
	if err := wakeSubtitleSource(ctx, tx, original.JobID, original.Ordinal); err != nil {
		return err
	}
	return tx.Commit()
}
