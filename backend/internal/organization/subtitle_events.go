package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func subtitleScrapeSource(ctx context.Context, r recoveryReader, enabled bool, job string, ordinal int, origin string) (string, *ScrapeReceipt, error) {
	yes, e := recoveryTable(ctx, r, "GO_SCRAPE_JOBS")
	if e != nil {
		return "", nil, e
	}
	if !yes {
		if enabled {
			return "", nil, ErrState
		}
		return "", nil, nil
	}
	var count int
	if e = r.QueryRowContext(ctx, `SELECT COUNT(*) FROM GO_SCRAPE_JOBS WHERE JOB_ID=? AND ORDINAL=?`, job, ordinal).Scan(&count); e != nil {
		return "", nil, e
	}
	if !enabled {
		if count != 0 {
			return "", nil, ErrState
		}
		return "", nil, nil
	}
	if count != 1 {
		return "", nil, ErrState
	}
	var outputs, options int
	if e = r.QueryRowContext(ctx, `SELECT LENGTH(CAST(OUTPUTS AS BLOB)),LENGTH(CAST(OPTIONS AS BLOB)) FROM GO_SCRAPE_JOBS WHERE JOB_ID=? AND ORDINAL=?`, job, ordinal).Scan(&outputs, &options); e != nil || outputs > 8<<20 || options > 64<<10 {
		return "", nil, ErrState
	}
	p, e := scanScrape(r.QueryRowContext(ctx, scrapeSelect+` WHERE JOB_ID=? AND ORDINAL=?`, job, ordinal))
	if e != nil {
		return "", nil, e
	}
	if p.ID != Digest([]string{job, strconv.Itoa(ordinal)}) || p.JobID != job || p.Ordinal != ordinal || p.Origin != origin || !recoveryHex(p.Config, 64) || p.Outputs == nil {
		return "", nil, ErrState
	}
	return Digest([]any{p.ID, p.JobID, p.Ordinal, p.Origin, p.Config, p.Options}), &p, nil
}
func freezeSubtitleSource(ctx context.Context, tx *sql.Tx, d Definition, job string, ordinal int, origin string) (bus.EventWebhookSource, error) {
	enabled := d.ScraperEnabled
	if enabled {
		raw, e := scrapeSetting(ctx, tx)
		if e != nil {
			return bus.EventWebhookSource{}, e
		}
		enabled = raw != ""
	}
	binding, _, e := subtitleScrapeSource(ctx, tx, enabled, job, ordinal, origin)
	if e != nil {
		return bus.EventWebhookSource{}, e
	}
	source := bus.EventWebhookSource{Kind: "subtitle_intake", Reference: job, Ordinal: ordinal, Origin: origin, ScraperBinding: binding}
	if !source.Valid() {
		return source, ErrState
	}
	if _, e = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_SUBTITLE_INTAKE_SOURCES(JOB_ID TEXT NOT NULL,ORDINAL INTEGER NOT NULL,DOCUMENT TEXT NOT NULL,FINGERPRINT TEXT NOT NULL,PRIMARY KEY(JOB_ID,ORDINAL))`); e != nil {
		return source, e
	}
	raw, _ := json.Marshal(source)
	res, e := tx.ExecContext(ctx, `INSERT INTO GO_SUBTITLE_INTAKE_SOURCES(JOB_ID,ORDINAL,DOCUMENT,FINGERPRINT)VALUES(?,?,?,?) ON CONFLICT(JOB_ID,ORDINAL)DO NOTHING`, job, ordinal, string(raw), Digest(source))
	if e != nil {
		return source, e
	}
	if _, e = res.RowsAffected(); e != nil {
		return source, e
	}
	saved, e := subtitleSavedSource(ctx, tx, job, ordinal)
	if e != nil || saved == nil || Digest(*saved) != Digest(source) {
		return source, ErrState
	}
	return source, nil
}
func subtitleSavedSource(ctx context.Context, r recoveryReader, job string, ordinal int) (*bus.EventWebhookSource, error) {
	yes, e := recoveryTable(ctx, r, "GO_SUBTITLE_INTAKE_SOURCES")
	if e != nil || !yes {
		return nil, e
	}
	var raw, fp string
	e = r.QueryRowContext(ctx, `SELECT DOCUMENT,FINGERPRINT FROM GO_SUBTITLE_INTAKE_SOURCES WHERE JOB_ID=? AND ORDINAL=?`, job, ordinal).Scan(&raw, &fp)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	p := bus.EventWebhookSource{}
	if len(raw) > 16<<10 || json.Unmarshal([]byte(raw), &p) != nil || !p.Valid() || p.Reference != job || p.Ordinal != ordinal || Digest(p) != fp {
		return nil, ErrState
	}
	return &p, nil
}

// Both the native subtitle sender and the independent event sender check
// these original prerequisites in their actual intent transaction.
func SubtitleIntakeReady(ctx context.Context, tx *sql.Tx, job string, ordinal int, origin string, expected *bus.EventWebhookSource, required ...bool) error {
	if !recoveryHex(job, 32) || ordinal < 0 || !recoveryHex(origin, 64) {
		return ErrState
	}
	saved, e := subtitleSavedSource(ctx, tx, job, ordinal)
	if e != nil {
		return e
	}
	if len(required) > 1 || len(required) == 1 && required[0] && saved == nil {
		return ErrState
	}
	if expected != nil && (saved == nil || Digest(*saved) != Digest(*expected)) {
		return ErrState
	}
	var raw, state string
	if e = tx.QueryRowContext(ctx, `SELECT CASE WHEN LENGTH(CAST(DEFINITION AS BLOB))<=67108864 THEN DEFINITION ELSE NULL END,STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, job).Scan(&raw, &state); e != nil {
		return e
	}
	d := Definition{}
	if state != "active" || json.Unmarshal([]byte(raw), &d) != nil || ordinal >= len(d.Entries) || d.Entries[ordinal].Kind != "media" {
		return ErrState
	}
	var count, incomplete int
	if e = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN STATE!='completed' OR REASON!='' THEN 1 ELSE 0 END),0) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, job).Scan(&count, &incomplete); e != nil || incomplete != 0 || count != len(d.Entries) {
		return ErrState
	}
	var proofRaw string
	var proof Proof
	if e = tx.QueryRowContext(ctx, `SELECT CASE WHEN LENGTH(CAST(PROOF AS BLOB))<=33554432 THEN PROOF ELSE NULL END FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, job, ordinal).Scan(&proofRaw); e != nil || json.Unmarshal([]byte(proofRaw), &proof) != nil || SubtitleOriginDigest(Job{Definition: d}, JobItem{Index: ordinal, Proof: proof}) != origin {
		return ErrState
	}
	enabled := d.ScraperEnabled
	if saved != nil {
		enabled = saved.ScraperBinding != ""
	} else if enabled {
		exists, e := recoveryTable(ctx, tx, "GO_SCRAPE_JOBS")
		if e != nil {
			return e
		}
		if !exists {
			enabled = false
		} else {
			if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_SCRAPE_JOBS WHERE JOB_ID=? AND ORDINAL=?)`, job, ordinal).Scan(&enabled); e != nil {
				return e
			}
		}
	}
	if enabled && !d.ScraperEnabled {
		return ErrState
	}
	binding, scrape, e := subtitleScrapeSource(ctx, tx, enabled, job, ordinal, origin)
	if e != nil {
		return e
	}
	// Older native subtitle rows retain their own actual original intake and
	// scrape receipts; reading them never creates a source or new webhook.
	if saved != nil && (saved.Origin != origin || saved.ScraperBinding != binding) {
		return ErrState
	}
	if scrape == nil {
		return nil
	}
	switch scrape.State {
	case "pending", "running":
		return bus.ErrEventWaiting
	case "completed", "partial", "blocked":
		for _, out := range scrape.Outputs {
			switch out.State {
			case "created", "preserved", "blocked":
			default:
				return ErrState
			}
		}
		return nil
	default:
		return ErrState
	}
}
func (s *Store) SubtitleIntakeReady(ctx context.Context, job string, ordinal int, origin string, required ...bool) error {
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	return SubtitleIntakeReady(ctx, tx, job, ordinal, origin, nil, required...)
}
func wakeSubtitleSource(ctx context.Context, tx *sql.Tx, job string, ordinal int) error {
	if e := wakeOpenSubtitle(ctx, tx, job, ordinal); e != nil {
		return e
	}
	for _, name := range []string{"GO_SUBTITLE_WAITS", "GO_EVENT_WEBHOOK_WAITS"} {
		yes, e := recoveryTable(ctx, tx, name)
		if e != nil {
			return e
		}
		if !yes {
			continue
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM `+name+` WHERE JOB_ID=? AND ORDINAL=?`, job, ordinal); e != nil {
			return e
		}
		var remains bool
		if e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+name+` WHERE JOB_ID=? AND ORDINAL=?)`, job, ordinal).Scan(&remains); e != nil || remains {
			return ErrState
		}
	}
	return nil
}
