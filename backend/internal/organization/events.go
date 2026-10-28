package organization

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/mediasyncdel"
	"github.com/0xforee/nas-tools/backend/internal/speedlimit"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

func (s *Store) BusinessEvents() *bus.Store               { return bus.New(s.db) }
func (s *Store) Subtitles() *subtitles.Store              { return subtitles.New(s.db) }
func (s *Store) MediaRefresh() *mediaserver.RefreshStore  { return mediaserver.NewRefreshStore(s.db) }
func (s *Store) MediaWebhooks() *mediaserver.WebhookStore { return mediaserver.NewWebhookStore(s.db) }
func (s *Store) SpeedLimiter() *speedlimit.Store          { return speedlimit.New(s.db) }
func (s *Store) MediaSyncDelete() *mediasyncdel.Store     { return mediasyncdel.New(s.db) }

func mediaRefreshItems(d Definition) []mediaserver.RefreshItem {
	items := []mediaserver.RefreshItem{}
	for _, e := range d.Entries {
		if e.Kind != "media" {
			continue
		}
		kind := e.MediaType
		switch kind {
		case "电影":
			kind = "MOV"
		case "电视剧":
			kind = "TV"
		case "动漫":
			kind = "ANIME"
		}
		items = append(items, mediaserver.RefreshItem{Title: e.Title, Year: e.Year, Type: kind, TMDBID: e.TMDBID, Category: e.Category, TargetRoot: d.TargetRoot, TargetPath: filepath.ToSlash(filepath.Join(d.TargetRoot, e.Target)), Directory: IsDisc(e)})
	}
	return items
}

// An explicit old-event refresh is derived from the saved successful native
// event, never from client-uploaded paths or a rendered notification sentence.
func (s *Store) MediaRefreshSource(ctx context.Context, id string) ([]mediaserver.RefreshItem, error) {
	if !recoveryHex(id, 32) {
		return nil, ErrPath
	}
	yes, err := recoveryTable(ctx, s.db, "GO_BUSINESS_EVENTS")
	if err != nil {
		return nil, err
	}
	if !yes {
		return nil, ErrNotFound
	}
	var exists bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM GO_BUSINESS_EVENTS WHERE ID=? AND KIND='transfer_finished' AND REFERENCE=?)`, bus.Hash([]string{"transfer_finished", id}), id).Scan(&exists)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	job, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(job.Items) != len(job.Definition.Entries) || job.Definition.Sync != nil && job.Definition.Sync.Record.Rename == 0 {
		return nil, ErrState
	}
	for _, item := range job.Items {
		if item.State != "completed" || item.Reason != "" {
			return nil, ErrState
		}
	}
	items := mediaRefreshItems(job.Definition)
	if !mediaserver.ValidRefreshItems(items) {
		return nil, ErrState
	}
	return items, nil
}
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
		if err := bus.PublishSignal(ctx, tx, "transfer.finished", fmt.Sprintf("%s:%d", id, ordinal), id, map[string]any{
			"in_path": d.SourceRoot, "file": filepath.Join(d.SourceRoot, e.Source), "target_path": d.TargetRoot, "dest": filepath.Join(d.TargetRoot, e.Target),
			"media_info":   map[string]any{"title": e.Title, "year": e.Year, "type": e.MediaType, "tmdb_id": e.TMDBID, "season_episode": e.SeasonEpisode},
			"native_event": map[string]any{"stage": "committed", "job_id": id, "ordinal": ordinal, "mode": d.Mode, "size": e.Size},
		}); err != nil {
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
	if err := bus.Emit(ctx, tx, "transfer_finished", id, id, title, body); err != nil {
		return err
	}
	return mediaserver.EnqueueTransferRefresh(ctx, tx, id, mediaRefreshItems(d))
}
