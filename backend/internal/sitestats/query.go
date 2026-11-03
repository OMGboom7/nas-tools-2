package sitestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
)

func stringInt(v int64) string { return strconv.FormatInt(v, 10) }

const snapshotSelect = `SELECT COALESCE(SITE,''),COALESCE(URL,''),COALESCE(USERNAME,''),COALESCE(USER_LEVEL,''),COALESCE(JOIN_AT,''),COALESCE(LAST_SEEN,''),COALESCE(UPDATE_AT,''),COALESCE(UPLOAD,0),COALESCE(DOWNLOAD,0),COALESCE(RATIO,0),COALESCE(SEEDING,0),COALESCE(LEECHING,0),COALESCE(SEEDING_SIZE,0),COALESCE(BONUS,0),COALESCE(MSG_UNREAD,0) FROM SITE_USER_INFO_STATS`

func scanSnapshot(row interface{ Scan(...any) error }) (Snapshot, error) {
	var v Snapshot
	e := row.Scan(&v.Site, &v.URL, &v.Username, &v.Level, &v.Joined, &v.LastSeen, &v.Updated, &v.Upload, &v.Download, &v.Ratio, &v.Seeding, &v.Leeching, &v.SeedingSize, &v.Bonus, &v.Unread)
	v.Seeds = [][2]int64{}
	return v, e
}
func (s *Store) List(ctx context.Context, urls []string) ([]Snapshot, error) {
	rows, e := s.db.QueryContext(ctx, snapshotSelect+` ORDER BY SITE,URL`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Snapshot{}
	allowed := map[string]bool{}
	for _, u := range urls {
		allowed[u] = true
	}
	for rows.Next() {
		v, e := scanSnapshot(rows)
		if e != nil {
			return nil, e
		}
		if allowed[v.URL] {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}

type History struct {
	Site        string  `json:"site"`
	URL         string  `json:"url"`
	Date        string  `json:"date"`
	Upload      int64   `json:"upload"`
	Download    int64   `json:"download"`
	Bonus       float64 `json:"bonus"`
	Seeding     int64   `json:"seeding"`
	SeedingSize int64   `json:"seedingSize"`
}

func (s *Store) History(ctx context.Context, address string, days int) ([]History, error) {
	if days < 1 || days > 730 {
		return nil, ErrState
	}
	rows, e := s.db.QueryContext(ctx, `SELECT COALESCE(SITE,''),COALESCE(URL,''),COALESCE(DATE,''),CAST(COALESCE(UPLOAD,'0')AS INTEGER),CAST(COALESCE(DOWNLOAD,'0')AS INTEGER),COALESCE(BONUS,0),COALESCE(SEEDING,0),COALESCE(SEEDING_SIZE,0)FROM (SELECT * FROM SITE_STATISTICS_HISTORY WHERE URL=? ORDER BY DATE DESC LIMIT ?) ORDER BY DATE`, address, days)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []History{}
	for rows.Next() {
		var h History
		if e = rows.Scan(&h.Site, &h.URL, &h.Date, &h.Upload, &h.Download, &h.Bonus, &h.Seeding, &h.SeedingSize); e != nil {
			return nil, e
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s *Store) Seeds(ctx context.Context, address string) ([][2]int64, error) {
	var raw string
	e := s.db.QueryRowContext(ctx, `SELECT COALESCE(SEEDING_INFO,'[]')FROM SITE_USER_SEEDING_INFO WHERE URL=?`, address).Scan(&raw)
	if e == sql.ErrNoRows {
		return [][2]int64{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := [][2]int64{}
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil, ErrResponse
	}
	if out == nil {
		out = [][2]int64{}
	}
	return out, nil
}

type Delta struct {
	Site     string `json:"site"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
}

func deltas(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, urls []string, days int, end time.Time) ([]Delta, error) {
	if days < 1 || days > 730 {
		return nil, ErrState
	}
	rows, e := db.QueryContext(ctx, `SELECT COALESCE(SITE,''),COALESCE(URL,''),CAST(COALESCE(UPLOAD,'0')AS INTEGER),CAST(COALESCE(DOWNLOAD,'0')AS INTEGER)FROM SITE_STATISTICS_HISTORY WHERE DATE>? AND DATE<=? ORDER BY SITE,DATE`, end.AddDate(0, 0, -days).Format("2006-01-02"), end.Format("2006-01-02"))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	allowed := map[string]bool{}
	for _, u := range urls {
		allowed[u] = true
	}
	type span struct{ minU, minD, maxU, maxD int64 }
	data := map[string]span{}
	for rows.Next() {
		var site, u string
		var up, down int64
		if e = rows.Scan(&site, &u, &up, &down); e != nil {
			return nil, e
		}
		if !allowed[u] {
			continue
		}
		v, ok := data[site]
		if !ok {
			v = span{up, down, up, down}
		} else {
			v.minU = min(v.minU, up)
			v.maxU = max(v.maxU, up)
			v.minD = min(v.minD, down)
			v.maxD = max(v.maxD, down)
		}
		data[site] = v
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	out := []Delta{}
	for site, v := range data {
		if v.minU == 0 && v.minD == 0 {
			v.minU = v.maxU
			v.minD = v.maxD
		}
		out = append(out, Delta{site, max(0, v.maxU-v.minU), max(0, v.maxD-v.minD)})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Upload > out[j].Upload || (out[i].Upload == out[j].Upload && out[i].Site < out[j].Site)
	})
	return out, nil
}
func (s *Store) Deltas(ctx context.Context, urls []string, days int, end time.Time) ([]Delta, error) {
	return deltas(ctx, s.db, urls, days, end)
}
func (s *Store) Finish(ctx context.Context, id, state string, urls []string, end time.Time) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	j, e := readJob(ctx, tx, id)
	if e != nil || j.State != "running" {
		return ErrState
	}
	j.State = state
	if j.StopRequested {
		j.State = "stopped"
		state = "stopped"
	}
	if j.Summary && (state == "complete" || state == "partial") {
		items, e := deltas(ctx, tx, urls, 2, end)
		if e != nil {
			return e
		}
		body := ""
		var up, down int64
		for _, v := range items {
			if v.Upload > 0 || v.Download > 0 {
				up += v.Upload
				down += v.Download
				body += "【" + v.Site + "】\n上传量：" + humanSize(v.Upload) + "\n下载量：" + humanSize(v.Download) + "\n\n"
			}
		}
		if body != "" {
			body = "【今日汇总】\n总上传：" + humanSize(up) + "\n总下载：" + humanSize(down) + "\n\n" + body
			if e = bus.Emit(ctx, tx, "ptrefresh_date_message", j.ID, j.ID, "站点数据统计", body); e != nil {
				return e
			}
		}
	}
	if e = putJob(ctx, tx, j); e != nil {
		return e
	}
	return tx.Commit()
}
func humanSize(n int64) string {
	if n < 1024 {
		return stringInt(n) + " B"
	}
	v := float64(n)
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return strconv.FormatFloat(v, 'f', 2, 64) + " " + units[i]
}
