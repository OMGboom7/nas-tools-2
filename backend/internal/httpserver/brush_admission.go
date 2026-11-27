package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/pan115"
	"github.com/0xforee/nas-tools/backend/internal/pikpak"
)

// A submission on another task can consume the last slot while the client is
// answering. Invalidate that observation even if its receipt already finished.
const brushRevisionSchema = `
CREATE TABLE IF NOT EXISTS GO_BRUSH_PENDING_TAGS(token TEXT PRIMARY KEY,tags TEXT NOT NULL);
CREATE TRIGGER IF NOT EXISTS go_brush_tags_done AFTER DELETE ON GO_BRUSH_SUBMISSION BEGIN DELETE FROM GO_BRUSH_PENDING_TAGS WHERE token=OLD.token; END;
CREATE TABLE IF NOT EXISTS GO_BRUSH_REVISION(downloader TEXT PRIMARY KEY, revision INTEGER NOT NULL);
CREATE TRIGGER IF NOT EXISTS go_brush_revision_add AFTER INSERT ON GO_BRUSH_SUBMISSION BEGIN
 INSERT INTO GO_BRUSH_REVISION VALUES(NEW.downloader,1) ON CONFLICT(downloader) DO UPDATE SET revision=revision+1; END;
CREATE TRIGGER IF NOT EXISTS go_brush_revision_done AFTER DELETE ON GO_BRUSH_SUBMISSION BEGIN
 INSERT INTO GO_BRUSH_REVISION VALUES(OLD.downloader,1) ON CONFLICT(downloader) DO UPDATE SET revision=revision+1; END;`

type brushLimits struct {
	downloader, rules, up, down, seed, label, note string
	revision                                       int64
}

func readBrushLimits(ctx context.Context, db brushBudgetReader, id int64) (b brushLimits, err error) {
	err = db.QueryRowContext(ctx, `SELECT t.DOWNLOADER,COALESCE(t.RSS_RULE,''),COALESCE(t.UP_LIMIT,''),COALESCE(t.DL_LIMIT,''),COALESCE(t.SEED_SIZE,''),
 COALESCE(t.LABEL,''),COALESCE(s.NOTE,''),COALESCE(r.revision,0)
 FROM SITE_BRUSH_TASK t JOIN CONFIG_SITE s ON s.ID=t.SITE LEFT JOIN GO_BRUSH_REVISION r ON r.downloader=t.DOWNLOADER
 WHERE t.ID=? AND t.STATE='Y' AND instr(s.INCLUDE,'S')>0`, id).Scan(&b.downloader, &b.rules, &b.up, &b.down, &b.seed, &b.label, &b.note, &b.revision)
	return
}

type brushLoad struct {
	up, down, count, siteActive, siteAll int64
	active, all                          map[string][]string
}

func brushLimit(raw string) (int64, error) {
	if raw == "" {
		return -1, nil
	}
	n, e := strconv.ParseUint(strings.TrimSpace(raw), 10, 62)
	return int64(n), e
}

func (s downloadService) brushLoad(ctx context.Context, item downloaderconfig.Downloader, rates, active, all, tagged bool) (v brushLoad, err error) {
	if !rates && !active && !all {
		return
	}
	var cfg map[string]any
	if json.Unmarshal([]byte(item.Config), &cfg) != nil {
		return v, errBrushDetail
	}
	switch item.Type {
	case "qbittorrent":
		c, e := s.qbProfileClient(item)
		if e != nil {
			return v, e
		}
		if rates {
			v.up, v.down, err = c.TransferRates(ctx)
		}
		if err == nil && active {
			v.active, err = c.BrushLabels(ctx, true)
		}
		if err == nil && all {
			v.all, err = c.BrushLabels(ctx, false)
		}
	case "transmission":
		c, e := s.profileClient(item)
		if e != nil {
			return v, e
		}
		if rates {
			v.up, v.down, err = c.TransferRates(ctx)
		}
		if err == nil && (active || all) {
			v.all, v.active, err = c.BrushLabels(ctx)
		}
	case "aria2":
		if tagged {
			return v, errDownloadOptionsUnsupported
		}
		c, e := aria2.NewClassic(text(cfg["host"]), text(cfg["port"]), text(cfg["secret"]), s.client.Transport)
		if e != nil {
			return v, e
		}
		v.up, v.down, v.count, err = c.BrushStats(ctx)
	case "pan115", "pikpak":
		if tagged || rates {
			return v, errDownloadOptionsUnsupported
		}
		v.active = map[string][]string{}
		if item.Type == "pan115" {
			c, e := pan115.New(text(cfg["cookie"]), s.client.Transport)
			if e != nil {
				return v, e
			}
			rows, e := c.Tasks(ctx)
			if e != nil {
				return v, e
			}
			for _, r := range rows {
				v.active[r.Hash] = nil
			}
		} else {
			c, e := pikpak.New(text(cfg["username"]), text(cfg["password"]), text(cfg["proxy"]), s.client.Transport)
			if e != nil {
				return v, e
			}
			rows, e := c.Tasks(ctx)
			if e != nil {
				return v, e
			}
			for _, r := range rows {
				if r.Phase != "PHASE_TYPE_ERROR" {
					v.active[r.ID] = nil
				}
			}
		}
	default:
		return v, errDownloadOptionsUnsupported
	}
	v.count += int64(len(v.active))
	return
}

func brushLabels(raw string) []string {
	tags := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' })
	var labels []string
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			labels = append(labels, t)
		}
	}
	return labels
}

func brushMatches(tags, required []string) bool {
	for _, t := range required {
		if !slices.Contains(tags, t) {
			return false
		}
	}
	return true
}

func (s downloadService) brushAdmission(ctx context.Context, db *sql.DB, task, size int64, partial bool, item downloaderconfig.Downloader, slots *int64) (*brushLimits, error) {
	b, err := readBrushLimits(ctx, db, task)
	if err != nil {
		return nil, err
	}
	if b.downloader != strconv.FormatInt(item.ID, 10) {
		return nil, errBrushDetail
	}
	fields, err := brushRuleFields(b.rules)
	if err != nil {
		return nil, err
	}
	var note struct{ Tags string }
	if b.note != "" && json.Unmarshal([]byte(b.note), &note) != nil {
		return nil, errBrushDetail
	}
	labels := brushLabels(b.label + "," + note.Tags)
	var limit [5]int64
	for i, raw := range []string{b.up, b.down, text(fields["dlcount"]), text(fields["current_site_dlcount"]), text(fields["current_site_count"])} {
		limit[i], err = brushLimit(raw)
		if err != nil {
			return nil, errBrushDetail
		}
	}
	if len(labels) == 0 {
		limit[3], limit[4] = -1, -1
	}
	tagged := limit[3] >= 0 || limit[4] >= 0
	v, err := s.brushLoad(ctx, item, limit[0] >= 0 || limit[1] >= 0, limit[2] >= 0 || limit[3] >= 0, limit[4] >= 0, tagged)
	if err != nil {
		return nil, err
	}
	if limit[0] >= 0 && v.up/1024 >= limit[0] || limit[1] >= 0 && v.down/1024 >= limit[1] {
		return nil, nil
	}
	for _, tags := range v.active {
		if brushMatches(tags, labels) {
			v.siteActive++
		}
	}
	for _, tags := range v.all {
		if brushMatches(tags, labels) {
			v.siteAll++
		}
	}
	if tagged || limit[2] >= 0 {
		rows, e := db.QueryContext(ctx, `SELECT COALESCE(remote_id,''),t.tags FROM GO_BRUSH_SUBMISSION LEFT JOIN GO_BRUSH_PENDING_TAGS t USING(token) WHERE downloader=?`, b.downloader)
		if e != nil {
			return nil, e
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var raw *string
			if e = rows.Scan(&id, &raw); e != nil {
				return nil, e
			}
			// Unconfirmed requests may already be consuming a slot. Their site
			// labels cannot be inferred until the downloader identifies the task.
			var pendingTags []string
			if raw != nil && json.Unmarshal([]byte(*raw), &pendingTags) != nil {
				return nil, errBrushDetail
			}
			matches := raw == nil || brushMatches(pendingTags, labels)
			if _, known := v.active[id]; !known {
				v.count++
				if matches {
					v.siteActive++
				}
			}
			if _, known := v.all[id]; !known && matches {
				v.siteAll++
			}
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}
	for i, count := range []int64{v.count, v.siteActive, v.siteAll} {
		if limit[i+2] >= 0 && count >= limit[i+2] {
			return nil, nil
		}
	}
	if b.seed != "" {
		gb, e := strconv.ParseFloat(b.seed, 64)
		n := gb * (1 << 30)
		if e != nil || math.IsNaN(n) || n < 0 || n >= float64(math.MaxInt64-5*(1<<30)) {
			return nil, errBrushDetail
		}
		var used int64
		err = db.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(CAST(TORRENT_SIZE AS INTEGER)),0) FROM SITE_BRUSH_TORRENTS WHERE TASK_ID=? AND DOWNLOAD_ID!='0')+
  (SELECT COALESCE(SUM(COALESCE(f.size,s.size)),0) FROM GO_BRUSH_SUBMISSION s LEFT JOIN GO_BRUSH_FRACTION f USING(token) WHERE s.task_id=?)`, task, task).Scan(&used)
		if err != nil {
			return nil, err
		}
		if used < 0 || used >= int64(n) || !partial && size >= int64(n)-used+5*(1<<30) {
			return nil, nil
		}
	}
	*slots = -1
	if limit[2] >= 0 {
		*slots = limit[2] - v.count
	}
	return &b, nil
}
