package siteconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type RequestPolicy struct {
	Interval, Gap time.Duration
	Count         int64
}

func ParseRequestPolicy(note map[string]any) (RequestPolicy, error) {
	values := map[string]int64{}
	for _, key := range []string{"limit_interval", "limit_count", "limit_seconds"} {
		if note[key] == nil {
			continue
		}
		raw := strings.TrimSpace(fmt.Sprint(note[key]))
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return RequestPolicy{}, errors.New("invalid site request limit")
		}
		values[key] = n
	}
	// Bound durations before multiplying into nanoseconds. Window units are
	// minutes in the saved editor; spacing units are seconds.
	if values["limit_interval"] > 52560000 || values["limit_seconds"] > 3153600000 || values["limit_count"] > 1000000000 {
		return RequestPolicy{}, errors.New("site request limit exceeded bounds")
	}
	if (values["limit_interval"] > 0) != (values["limit_count"] > 0) {
		return RequestPolicy{}, errors.New("site request window and count must be configured together")
	}
	return RequestPolicy{Interval: time.Duration(values["limit_interval"]) * time.Minute, Gap: time.Duration(values["limit_seconds"]) * time.Second, Count: values["limit_count"]}, nil
}

// AdmitRequest reads the current policy and reserves one visit in the same
// immediate SQLite transaction. A denial leaves both the count and timestamp intact.
func (store *Store) AdmitRequest(ctx context.Context, id int64, clock func() time.Time) (time.Duration, error) {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var note string
	var last, count sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(s.NOTE,''), r.LAST_NS, r.VISITS
        FROM CONFIG_SITE s LEFT JOIN GO_SITE_REQUEST_LIMIT r ON r.SITE_ID=s.ID WHERE s.ID=?`, id).Scan(&note, &last, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	var settings map[string]any
	if len(note) > 64<<10 || note != "" && json.Unmarshal([]byte(note), &settings) != nil {
		return 0, errors.New("invalid site request configuration")
	}
	policy, err := ParseRequestPolicy(settings)
	if err != nil || policy == (RequestPolicy{}) {
		return 0, err
	}
	now := clock()
	visits := count.Int64
	if last.Valid {
		prev := time.Unix(0, last.Int64)
		if policy.Interval > 0 && now.Sub(prev) >= policy.Interval {
			visits = 0
		}
		wait := prev.Add(policy.Gap).Sub(now)
		if policy.Interval > 0 && visits >= policy.Count {
			wait = max(wait, prev.Add(policy.Interval).Sub(now))
		}
		if wait > 0 {
			return wait, nil
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_SITE_REQUEST_LIMIT(SITE_ID,LAST_NS,VISITS) VALUES(?,?,?)
        ON CONFLICT(SITE_ID) DO UPDATE SET LAST_NS=excluded.LAST_NS,VISITS=excluded.VISITS`, id, now.UnixNano(), min(visits+1, int64(1000000000)))
	if err != nil {
		return 0, err
	}
	return 0, tx.Commit()
}
