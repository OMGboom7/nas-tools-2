package httpserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

const brushAccountingSchema = `
CREATE TABLE IF NOT EXISTS GO_BRUSH_SCAN(task INTEGER PRIMARY KEY,token TEXT NOT NULL,scope TEXT NOT NULL,phase TEXT NOT NULL,choices TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS GO_BRUSH_USAGE(row_id INTEGER PRIMARY KEY, identity TEXT NOT NULL, client TEXT NOT NULL, uploaded INTEGER NOT NULL,downloaded INTEGER NOT NULL, removing INTEGER NOT NULL);
CREATE TRIGGER IF NOT EXISTS go_brush_usage_clear AFTER DELETE ON SITE_BRUSH_TORRENTS BEGIN DELETE FROM GO_BRUSH_USAGE WHERE row_id=OLD.ID; END;
CREATE TRIGGER IF NOT EXISTS go_brush_scan_clear AFTER DELETE ON SITE_BRUSH_TASK BEGIN DELETE FROM GO_BRUSH_SCAN WHERE task=OLD.ID; END;`

type brushAccountingRequest struct {
	brushMaintenanceRequest
	Token  string
	Absent []string
}
type brushAccountRow struct {
	ID             int64
	Hash, Identity string
}
type brushScanStart struct {
	Token string
	IDs   []string
}

func BrushAccountCommand(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	reader := &io.LimitedReader{R: in, N: 8 << 20}
	d := json.NewDecoder(reader)
	d.DisallowUnknownFields()
	var q brushAccountingRequest
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || reader.N == 0 || q.TaskID <= 0 || q.Downloader == "" {
		return errBrushDetail
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	db, err := (subscriptionService{databasePath: (config.Application{Path: path}).UserDatabasePath()}).openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := accountBrushScan(ctx, db, op, q)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func accountBrushScan(ctx context.Context, db *sql.DB, op string, q brushAccountingRequest) (any, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, brushAccountingSchema); err != nil {
		return nil, err
	}
	var client, site, rules, deadlineDelete, limitSpeed string
	err = tx.QueryRowContext(ctx, `SELECT d.TYPE || ':' || COALESCE(d.CONFIG,''),COALESCE(t.SITE,''),COALESCE(t.REMOVE_RULE,''),COALESCE(t.BRUSHTASK_FREE_DDL_DELETE,''),COALESCE(t.BRUSHTASK_FREE_LIMIT_SPEED,'') FROM SITE_BRUSH_TASK t JOIN DOWNLOADER d ON d.ID=t.DOWNLOADER WHERE t.ID=? AND t.DOWNLOADER=? AND t.STATE IN ('Y','S') AND d.ENABLED=1 AND d.TYPE IN ('qbittorrent','transmission')`, q.TaskID, q.Downloader).Scan(&client, &site, &rules, &deadlineDelete, &limitSpeed)
	if err != nil {
		return nil, err
	}
	client = bus.Hash(client)
	rows, err := tx.QueryContext(ctx, `SELECT ID,DOWNLOAD_ID,COALESCE(ENCLOSURE,''),COALESCE(LST_MOD_DATE,''),COALESCE(TORRENT_SIZE,''),COALESCE(FREE_DEADLINE,''), owners FROM (SELECT *,count(*) OVER(PARTITION BY DOWNLOAD_ID) AS owners FROM SITE_BRUSH_TORRENTS WHERE DOWNLOADER=? AND DOWNLOAD_ID!='0') WHERE TASK_ID=? ORDER BY ID`, q.Downloader, q.TaskID)
	if err != nil {
		return nil, err
	}
	active := map[string]brushAccountRow{}
	ids := []string{}
	var history []string
	for rows.Next() {
		var row brushAccountRow
		var link, modified, size, deadline string
		var owners int
		if err = rows.Scan(&row.ID, &row.Hash, &link, &modified, &size, &deadline, &owners); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := active[row.Hash]; ok || row.Hash == "" || owners != 1 {
			rows.Close()
			return nil, errBrushDetail
		}
		row.Identity = bus.Hash([]any{row.ID, q.TaskID, site, q.Downloader, row.Hash, link, modified, size})
		history = append(history, row.Identity, deadline)
		active[row.Hash] = row
		ids = append(ids, row.Hash)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	scope := bus.Hash([]any{site, client, rules, deadlineDelete, limitSpeed, history})
	if op == "begin" {
		token := rand.Text()
		_, err = tx.ExecContext(ctx, `INSERT INTO GO_BRUSH_SCAN VALUES(?,?,?,'begin','[]') ON CONFLICT(task) DO UPDATE SET token=excluded.token,scope=excluded.scope,phase='begin',choices='[]'`, q.TaskID, token, scope)
		if err != nil {
			return nil, err
		}
		return brushScanStart{token, ids}, tx.Commit()
	}
	var phase, saved, choices string
	err = tx.QueryRowContext(ctx, "SELECT phase,scope,choices FROM GO_BRUSH_SCAN WHERE task=? AND token=?", q.TaskID, q.Token).Scan(&phase, &saved, &choices)
	if err != nil {
		return nil, err
	}
	if op == "finish" && phase == "done" {
		return true, nil
	}
	if saved != scope {
		return nil, errBrushDetail
	}
	var result any
	switch op {
	case "plan":
		if phase != "begin" {
			return nil, errBrushDetail
		}
		decisions, e := planBrushMaintenanceTx(ctx, tx, q.brushMaintenanceRequest, time.Now())
		if e != nil {
			return nil, e
		}
		var toRemove []string
		for _, o := range q.Items {
			r := active[o.ID]
			if o.Uploaded < 0 || o.Downloaded < 0 {
				return nil, errBrushDetail
			}
			var removing bool
			e = tx.QueryRowContext(ctx, "SELECT removing FROM GO_BRUSH_USAGE WHERE row_id=?", r.ID).Scan(&removing)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return nil, e
			}

			if removing {
				// An earlier process may have already sent the destructive request.
				// Keep its original byte counters and only reconcile confirmed absence.
				decisions[o.ID] = brushDecision{Action: "pending"}
				continue
			}
			_, e = tx.ExecContext(ctx, `INSERT INTO GO_BRUSH_USAGE VALUES(?,?,?,?,?,?) ON CONFLICT(row_id) DO UPDATE SET identity=excluded.identity,client=excluded.client,uploaded=excluded.uploaded,downloaded=excluded.downloaded`, r.ID, r.Identity, client, o.Uploaded, o.Downloaded, false)
			if e != nil {
				return nil, e
			}
			if decisions[o.ID].Action == "remove" {
				toRemove = append(toRemove, o.ID)
			}
		}
		encoded, e := json.Marshal(toRemove)
		if e != nil {
			return nil, e
		}
		choices = string(encoded)
		result = decisions
		phase = "planned"
	case "issue":
		if phase != "planned" {
			return nil, errBrushDetail
		}
		var approved []string
		if err = json.Unmarshal([]byte(choices), &approved); err != nil {
			return nil, err
		}
		for _, id := range approved {
			r, ok := active[id]
			if !ok {
				return nil, errBrushDetail
			}
			if err = fuzzyOne(tx.ExecContext(ctx, "UPDATE GO_BRUSH_USAGE SET removing=1 WHERE row_id=? AND identity=? AND client=? AND removing=0", r.ID, r.Identity, client)); err != nil {
				return nil, err
			}
		}
		result = approved
		phase = "issued"
	case "finish":
		if phase != "issued" && phase != "planned" {
			return nil, errBrushDetail
		}
		if err = finishBrushScan(ctx, tx, q, active, client); err != nil {
			return nil, err
		}
		phase = "done"
		result = true
	default:
		return nil, errBrushDetail
	}
	if _, err = tx.ExecContext(ctx, "UPDATE GO_BRUSH_SCAN SET phase=?,choices=? WHERE task=? AND token=?", phase, choices, q.TaskID, q.Token); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

func finishBrushScan(ctx context.Context, tx *sql.Tx, q brushAccountingRequest, active map[string]brushAccountRow, client string) error {
	removed := 0
	for _, id := range q.Absent {
		r, ok := active[id]
		if !ok {
			return errBrushDetail
		}
		delete(active, id)
		var up, down int64
		var identity, previousClient string
		var removing bool
		err := tx.QueryRowContext(ctx, "SELECT identity,client,uploaded,downloaded,removing FROM GO_BRUSH_USAGE WHERE row_id=?", r.ID).Scan(&identity, &previousClient, &up, &down, &removing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (r.Identity != identity || client != previousClient) {
			continue
		}
		removed++
		if removing {
			_, err = tx.ExecContext(ctx, "UPDATE SITE_BRUSH_TORRENTS SET DOWNLOAD_ID='0',TORRENT_SIZE=? WHERE ID=?", strconv.FormatInt(up, 10)+","+strconv.FormatInt(down, 10), r.ID)
			if err == nil {
				_, err = tx.ExecContext(ctx, "DELETE FROM GO_BRUSH_USAGE WHERE row_id=?", r.ID)
			}
		} else {
			_, err = tx.ExecContext(ctx, "DELETE FROM SITE_BRUSH_TORRENTS WHERE ID=?", r.ID)
		}
		if err != nil {
			return err
		}
	}
	// Historical releases stored either uploaded bytes or uploaded,downloaded.
	rows, err := tx.QueryContext(ctx, `SELECT h.DOWNLOAD_ID,COALESCE(h.TORRENT_SIZE,''),COALESCE(u.uploaded,0),COALESCE(u.downloaded,0) FROM SITE_BRUSH_TORRENTS h LEFT JOIN GO_BRUSH_USAGE u ON u.row_id=h.ID WHERE h.TASK_ID=? AND (h.DOWNLOAD_ID='0' OR h.DOWNLOADER=?)`, q.TaskID, q.Downloader)
	if err != nil {
		return err
	}
	defer rows.Close()
	var uploaded, downloaded int64
	for rows.Next() {
		var id, raw string
		var u, d int64
		if err = rows.Scan(&id, &raw, &u, &d); err != nil {
			return err
		}
		if id == "0" {
			a, b, _ := strings.Cut(raw, ",")
			u, d = 0, 0
			if a = strings.TrimSpace(a); a != "" {
				u, err = strconv.ParseInt(a, 10, 64)
				if err != nil {
					return err
				}
			}
			if b = strings.TrimSpace(b); b != "" {
				d, err = strconv.ParseInt(b, 10, 64)
				if err != nil {
					return err
				}
			}
		}
		if u < 0 || d < 0 || uploaded > math.MaxInt64-u || downloaded > math.MaxInt64-d {
			return errBrushDetail
		}
		uploaded += u
		downloaded += d
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	return fuzzyOne(tx.ExecContext(ctx, `UPDATE SITE_BRUSH_TASK SET UPLOAD_SIZE=?,DOWNLOAD_SIZE=?,REMOVE_COUNT=COALESCE(REMOVE_COUNT,0)+? WHERE ID=?`, uploaded, downloaded, removed, q.TaskID))
}
