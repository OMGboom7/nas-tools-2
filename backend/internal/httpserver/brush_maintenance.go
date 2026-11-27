package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

type brushObservation struct {
	ID                                                                                  string
	Completed                                                                           bool
	SeedSeconds, AgeSeconds, InactiveSeconds, AverageUpload, Uploaded, Downloaded, Size int64
	Ratio                                                                               float64 // The legacy display-rounded ratio for a completed task.
}
type brushMaintenanceRequest struct {
	TaskID     int64
	Downloader string
	Items      []brushObservation
}
type brushDecision struct{ Action, Reason, Deadline string }
type brushRemoveLimit struct {
	key, reason, operator string
	scale, value          float64
}

func brushRemovalLimits(raw string) ([]brushRemoveLimit, error) {
	fields, err := brushRuleFields(raw)
	if err != nil {
		return nil, err
	}
	var active []brushRemoveLimit
	for _, r := range []brushRemoveLimit{
		{"time", "做种时间", "gt", 3600, 0},
		{"ratio", "分享率", "gt", 1, 0},
		{"uploadsize", "上传量", "gt", 1 << 30, 0},
		{"dltime", "下载耗时", "gt", 3600, 0},
		{"avg_upspeed", "平均上传速度", "lt", 1024, 0},
		{"iatime", "未活动时间", "gt", 3600, 0},
	} {
		raw := text(fields[r.key])
		if raw == "" || raw == "#" {
			continue
		}
		op, n, ok := strings.Cut(raw, "#")
		if ok && (op == "" || n == "") {
			continue
		}
		if !ok || op != r.operator {
			return nil, errBrushDetail
		}
		v, e := strconv.ParseFloat(n, 64)
		r.value = v * r.scale
		if e != nil || v < 0 || math.IsNaN(v) || math.IsInf(r.value, 0) {
			return nil, errBrushDetail
		}
		active = append(active, r)
	}
	return active, nil
}

func brushMaintenanceDecision(o brushObservation, limits []brushRemoveLimit, deadline string, del, throttle bool, now time.Time) brushDecision {
	// The classic downloading path required a known size to compute its ratio.
	if !o.Completed && o.Size <= 0 {
		return brushDecision{Action: "keep"}
	}
	for _, r := range limits {
		var value float64
		switch r.key {
		case "time":
			if o.Completed {
				value = float64(o.SeedSeconds)
			}
		case "ratio":
			if o.Completed {
				value = o.Ratio
			} else if o.Size > 0 {
				value = float64(o.Uploaded) / float64(o.Size)
			}
		case "uploadsize":
			if o.Completed {
				value = float64(o.Uploaded)
			}
		case "dltime":
			if !o.Completed {
				value = float64(o.AgeSeconds)
			}
		case "avg_upspeed":
			value = float64(o.AverageUpload)
		case "iatime":
			value = float64(o.InactiveSeconds)
		}
		if value > 0 && (r.operator == "gt" && value > r.value || r.operator == "lt" && value < r.value) {
			return brushDecision{Action: "remove", Reason: r.reason}
		}
	}
	if !o.Completed && (del || throttle) {
		for _, layout := range []string{"20060102_1504", time.DateTime} {
			end, e := time.ParseInLocation(layout, deadline, now.Location())
			if e == nil && !end.After(now.Add(5*time.Minute)) {
				action := "throttle"
				if del {
					action = "remove"
				}
				return brushDecision{Action: action, Reason: "限免结束", Deadline: deadline}
			}
		}
	}
	return brushDecision{Action: "keep"}
}

func BrushMaintenanceCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	reader := &io.LimitedReader{R: in, N: 8 << 20}
	d := json.NewDecoder(reader)
	d.DisallowUnknownFields()
	var q brushMaintenanceRequest
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
	result, err := planBrushMaintenance(ctx, db, q, time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func planBrushMaintenance(ctx context.Context, db *sql.DB, q brushMaintenanceRequest, now time.Time) (map[string]brushDecision, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return planBrushMaintenanceTx(ctx, tx, q, now)
}

func planBrushMaintenanceTx(ctx context.Context, tx *sql.Tx, q brushMaintenanceRequest, now time.Time) (map[string]brushDecision, error) {
	var rules, del, throttle string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(REMOVE_RULE,''),COALESCE(BRUSHTASK_FREE_DDL_DELETE,''),COALESCE(BRUSHTASK_FREE_LIMIT_SPEED,'') FROM SITE_BRUSH_TASK WHERE ID=? AND DOWNLOADER=? AND STATE IN ('Y','S')`, q.TaskID, q.Downloader).Scan(&rules, &del, &throttle)
	if err != nil {
		return nil, err
	}
	limits, err := brushRemovalLimits(rules)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DOWNLOAD_ID,COALESCE(MAX(FREE_DEADLINE),'') FROM SITE_BRUSH_TORRENTS
 WHERE DOWNLOADER=? AND DOWNLOAD_ID!='0' GROUP BY DOWNLOAD_ID HAVING COUNT(*)=1 AND MAX(TASK_ID)=?`, q.Downloader, strconv.FormatInt(q.TaskID, 10))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owned := map[string]string{}
	for rows.Next() {
		var id, end string
		if err = rows.Scan(&id, &end); err != nil {
			return nil, err
		}
		owned[id] = end
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result := map[string]brushDecision{}
	for _, o := range q.Items {
		if _, seen := result[o.ID]; seen {
			return nil, errBrushDetail
		}
		end, ok := owned[o.ID]
		if !ok {
			return nil, errBrushDetail
		}
		result[o.ID] = brushMaintenanceDecision(o, limits, end, del == "Y", throttle == "Y", now)
	}
	return result, nil
}
