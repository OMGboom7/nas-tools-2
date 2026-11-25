package sitestats

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reader queries legacy statistics tables on a database or a consistent read transaction.
type Reader struct{ db querySource }

type querySource interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func NewReader(db querySource) Reader { return Reader{db: db} }

func stringInt(v int64) string { return strconv.FormatInt(v, 10) }

const snapshotSelect = `SELECT COALESCE(SITE,''),COALESCE(URL,''),COALESCE(USERNAME,''),COALESCE(USER_LEVEL,''),COALESCE(JOIN_AT,''),COALESCE(LAST_SEEN,''),COALESCE(UPDATE_AT,''),COALESCE(UPLOAD,0),COALESCE(DOWNLOAD,0),COALESCE(RATIO,0),COALESCE(SEEDING,0),COALESCE(LEECHING,0),COALESCE(SEEDING_SIZE,0),COALESCE(BONUS,0),COALESCE(MSG_UNREAD,0) FROM SITE_USER_INFO_STATS`

func scanSnapshot(row interface{ Scan(...any) error }) (Snapshot, error) {
	var v Snapshot
	e := row.Scan(&v.Site, &v.URL, &v.Username, &v.Level, &v.Joined, &v.LastSeen, &v.Updated, &v.Upload, &v.Download, &v.Ratio, &v.Seeding, &v.Leeching, &v.SeedingSize, &v.Bonus, &v.Unread)
	v.Seeds = [][2]int64{}
	return v, e
}
func (s Reader) Snapshot(ctx context.Context, address string) (Snapshot, error) {
	return scanSnapshot(s.db.QueryRowContext(ctx, snapshotSelect+` WHERE URL=?`, address))
}

func (s Reader) List(ctx context.Context, urls []string) ([]Snapshot, error) {
	scope, e := json.Marshal(urls)
	if e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, snapshotSelect+` WHERE URL IN (SELECT value FROM json_each(?)) ORDER BY SITE,URL`, string(scope))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		v, e := scanSnapshot(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
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

func (s Reader) History(ctx context.Context, address string, days int) ([]History, error) {
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
func (s Reader) Seeds(ctx context.Context, address string) ([][2]int64, error) {
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
	URL      string `json:"-"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
}

func deltas(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, urls []string, days int, end time.Time) ([]Delta, error) {
	if days < 1 || days > 730 {
		return nil, ErrState
	}
	scope, e := json.Marshal(urls)
	if e != nil {
		return nil, e
	}
	rows, e := db.QueryContext(ctx, `SELECT COALESCE(SITE,''),COALESCE(URL,''),CAST(COALESCE(UPLOAD,'0')AS INTEGER),CAST(COALESCE(DOWNLOAD,'0')AS INTEGER)FROM SITE_STATISTICS_HISTORY WHERE URL IN (SELECT value FROM json_each(?)) AND DATE>? AND DATE<=? ORDER BY URL,DATE`, string(scope), end.AddDate(0, 0, -days).Format("2006-01-02"), end.Format("2006-01-02"))
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	type span struct {
		site                   string
		minU, minD, maxU, maxD int64
	}
	data := map[string]span{}
	for rows.Next() {
		var site, u string
		var up, down int64
		if e = rows.Scan(&site, &u, &up, &down); e != nil {
			return nil, e
		}
		v, ok := data[u]
		if !ok {
			v = span{site, up, down, up, down}
		} else {
			v.site = site
			v.minU = min(v.minU, up)
			v.maxU = max(v.maxU, up)
			v.minD = min(v.minD, down)
			v.maxD = max(v.maxD, down)
		}
		data[u] = v
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	out := []Delta{}
	for address, v := range data {
		if v.minU == 0 && v.minD == 0 {
			v.minU = v.maxU
			v.minD = v.maxD
		}
		out = append(out, Delta{Site: v.site, URL: address, Upload: max(0, v.maxU-v.minU), Download: max(0, v.maxD-v.minD)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Upload != out[j].Upload {
			return out[i].Upload > out[j].Upload
		}
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		return out[i].URL < out[j].URL
	})
	return out, nil
}
func (s Reader) Deltas(ctx context.Context, urls []string, days int, end time.Time) ([]Delta, error) {
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
		if e := emitSummary(ctx, tx, urls, end, j.ID); e != nil {
			return e
		}
	}
	if e = putJob(ctx, tx, j); e != nil {
		return e
	}
	return tx.Commit()
}
func emitSummary(ctx context.Context, tx *sql.Tx, urls []string, end time.Time, reference string) error {
	items, e := deltas(ctx, tx, urls, 2, end)
	if e != nil {
		return e
	}
	var body strings.Builder
	var up, down big.Int
	labels := make(map[string]int)
	for _, v := range items {
		labels[v.Site]++
	}
	for _, v := range items {
		if v.Upload == 0 && v.Download == 0 {
			continue
		}
		up.Add(&up, big.NewInt(v.Upload))
		down.Add(&down, big.NewInt(v.Download))
		name := v.Site
		if labels[name] > 1 {
			name += " (" + v.URL + ")"
		}
		body.WriteString("【" + name + "】\n上传量：" + humanSize(big.NewInt(v.Upload)) + "\n下载量：" + humanSize(big.NewInt(v.Download)) + "\n\n")
	}
	if body.Len() == 0 {
		return nil
	}
	title := "【今日汇总】\n总上传：" + humanSize(&up) + "\n总下载：" + humanSize(&down) + "\n\n"
	return emitNotice(ctx, tx, "ptrefresh_date_message", reference, reference, "站点数据统计", title+body.String())
}

func (s *Store) ScheduledSummary(ctx context.Context, end time.Time, reference string, scope func(context.Context, *sql.Tx) ([]string, error)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Keep eligibility and daily counters at the same revision as the recipients.
	if _, err = tx.ExecContext(ctx, "UPDATE CONFIG_SITE SET ID=ID WHERE 0"); err != nil {
		return err
	}
	urls, err := scope(ctx, tx)
	if err != nil {
		return err
	}
	if err = emitSummary(ctx, tx, urls, end, reference); err != nil {
		return err
	}
	return tx.Commit()
}

func humanSize(n *big.Int) string {
	if n.BitLen() <= 10 {
		return n.String() + " B"
	}
	v, _ := new(big.Float).SetInt(n).Float64()
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return strconv.FormatFloat(v, 'f', 2, 64) + " " + units[i]
}
