package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

func (s *Store) BusinessEvents() *bus.Store  { return bus.New(s.db) }
func (s *Store) Subtitles() *subtitles.Store { return subtitles.New(s.db) }
func SubtitleOriginDigest(job Job, item JobItem) string {
	return Digest([]any{job.Definition, item.Index, item.Proof.Identity, item.Proof.ParentIdentity, item.Proof.Digest, item.Proof.Kind, item.Proof.LinkTarget})
}

// Emit only after every saved entry and move-cleanup receipt is complete.
// Do not label a partial media/subtitle group or raw file sync as media intake.
func emitOrganizationEvent(ctx context.Context, tx *sql.Tx, id string) error {
	var raw, state string
	if err := tx.QueryRowContext(ctx, `SELECT DEFINITION,STATE FROM GO_ORGANIZATION_JOBS WHERE ID=?`, id).Scan(&raw, &state); err != nil {
		return err
	}
	var d Definition
	if state != "active" || json.Unmarshal([]byte(raw), &d) != nil {
		return ErrState
	}
	if d.Sync != nil && d.Sync.Record.Rename == 0 {
		return nil
	}
	var count, pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN STATE!='completed' OR REASON!='' THEN 1 ELSE 0 END),0) FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=?`, id).Scan(&count, &pending); err != nil {
		return err
	}
	if count != len(d.Entries) || pending != 0 {
		return nil
	}
	origin := "手动整理"
	if d.Download != nil {
		origin = "下载完成整理"
	}
	if d.Sync != nil {
		origin = "目录同步"
	}
	lines := []string{}
	first := ""
	var size int64
	for ordinal, e := range d.Entries {
		if e.Kind != "media" {
			continue
		}
		var proofRaw string
		var proof Proof
		if err := tx.QueryRowContext(ctx, `SELECT PROOF FROM GO_ORGANIZATION_ITEMS WHERE JOB_ID=? AND ORDINAL=?`, id, ordinal).Scan(&proofRaw); err != nil {
			return err
		}
		if json.Unmarshal([]byte(proofRaw), &proof) != nil {
			return ErrState
		}
		originDigest := SubtitleOriginDigest(Job{Definition: d}, JobItem{Index: ordinal, Proof: proof})
		if d.ScraperEnabled {
			if err := emitScrape(ctx, tx, id, ordinal, e.Title+" "+e.SeasonEpisode, originDigest); err != nil {
				return err
			}
		}
		if err := subtitles.Emit(ctx, tx, id, ordinal, e.Title+" "+e.SeasonEpisode, filepath.Join(d.TargetRoot, e.Target), e.MediaType, originDigest, IsDisc(e)); err != nil {
			return err
		}
		if first == "" {
			first = e.Title
		}
		size += e.Size
		lines = append(lines, fmt.Sprintf("%s (%s) %s %s", e.Title, e.Year, e.MediaType, e.SeasonEpisode))
	}
	if len(lines) == 0 {
		return nil
	}
	title := first + " 已入库"
	if len(lines) > 1 {
		title = fmt.Sprintf("整理完成：%d 个媒体文件", len(lines))
	}
	body := fmt.Sprintf("来自：%s；方式：%s；媒体大小：%d 字节\n%s\n任务：%s", origin, ModeLabel(d.Mode), size, strings.Join(lines, "\n"), id)
	return bus.Emit(ctx, tx, "transfer_finished", id, id, title, body)
}
