package mediasync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// CachePresence is deliberately not Coverage: neither a negative result nor
// a recorded season is evidence that a download is needed or a season complete.
type CacheQuery struct {
	TMDBID  string `json:"tmdbid"`
	Title   string `json:"title"`
	Year    string `json:"year"`
	Type    string `json:"type"`
	Season  *int   `json:"season,omitempty"`
	Episode *int   `json:"episode,omitempty"`
}
type CachePresence struct {
	State    string `json:"state"`
	Seasons  int    `json:"seasons"`
	Episodes int    `json:"episodes"`
}
type CacheSummary struct {
	State       string   `json:"state"`
	Server      string   `json:"server"`
	Generation  string   `json:"generation"`
	Updated     string   `json:"updated"`
	Libraries   []string `json:"libraries"`
	DisplayOnly bool     `json:"displayOnly"`
}
type CacheBatch struct {
	Cache   CacheSummary    `json:"cache"`
	Results []CachePresence `json:"results"`
}

func validCacheQuery(q CacheQuery) bool {
	if q.Type != "movie" && q.Type != "show" || len(q.Title) > 4096 || len(q.Year) > 4 || q.TMDBID == "" && strings.TrimSpace(q.Title) == "" {
		return false
	}
	if q.TMDBID != "" {
		id, err := strconv.ParseInt(q.TMDBID, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != q.TMDBID {
			return false
		}
	}
	if q.Year != "" {
		if len(q.Year) != 4 {
			return false
		}
		for _, c := range q.Year {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	for _, n := range []*int{q.Season, q.Episode} {
		if n != nil && (*n < 0 || *n > 10000) {
			return false
		}
	}
	return q.Type != "movie" || q.Season == nil && q.Episode == nil
}
func ValidateCacheQueries(queries []CacheQuery) error {
	if len(queries) > 200 {
		return ErrSnapshot
	}
	for _, q := range queries {
		if !validCacheQuery(q) {
			return ErrSnapshot
		}
	}
	return nil
}
func (s *Store) LookupBatch(ctx context.Context, server, digest string, queries []CacheQuery) (CacheBatch, error) {
	result := CacheBatch{Cache: CacheSummary{State: "unavailable", Server: server, Libraries: []string{}, DisplayOnly: true}, Results: make([]CachePresence, len(queries))}
	if err := ValidateCacheQueries(queries); err != nil {
		return result, err
	}
	for i := range queries {
		result.Results[i].State = "unavailable"
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_MEDIASYNC_SNAPSHOTS WHERE SERVER=?`, server).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		result.Cache.State = "unsynced"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return result, err
	}
	result.Cache.Generation = snapshot.Generation
	result.Cache.Updated = snapshot.Updated
	result.Cache.Libraries = snapshot.Libraries
	if snapshot.ConfigDigest != digest {
		result.Cache.State = "scope-changed"
		return result, nil
	}
	selected := map[string]bool{}
	for _, id := range snapshot.Libraries {
		selected[id] = true
	}
	result.Cache.State = "ready"
	for i, q := range queries {
		presence, err := lookupCachedItem(ctx, tx, server, selected, q)
		if err != nil {
			return result, err
		}
		result.Results[i] = presence
	}
	return result, tx.Commit()
}
func lookupCachedItem(ctx context.Context, tx *sql.Tx, server string, selected map[string]bool, q CacheQuery) (CachePresence, error) {
	result := CachePresence{State: "not-recorded"}
	legacyKind := "Movie"
	if q.Type == "show" {
		legacyKind = "Series"
	}
	query := `SELECT LIBRARY,COALESCE(JSON,'[]') FROM MEDIASYNC_ITEMS WHERE SERVER=? AND ITEM_TYPE IN (?,?) AND `
	args := []any{server, q.Type, legacyKind}
	if q.TMDBID != "" {
		query += `TMDBID=?`
		args = append(args, q.TMDBID)
	} else {
		query += `TITLE=? AND (?='' OR YEAR=?)`
		args = append(args, q.Title, q.Year, q.Year)
	}
	rows, err := tx.QueryContext(ctx, query+` LIMIT 1001`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	pairs := map[[2]int]bool{}
	seasons := map[int]bool{}
	matched := false
	count := 0
	for rows.Next() {
		var library, episodes string
		if err := rows.Scan(&library, &episodes); err != nil {
			return result, err
		}
		count++
		if count > 1000 || !selected[library] {
			return CachePresence{State: "unavailable"}, nil
		}
		if q.Type == "movie" {
			matched = true
			continue
		}
		var records []struct {
			Season  *int `json:"season_num"`
			Episode *int `json:"episode_num"`
		}
		if json.Unmarshal([]byte(episodes), &records) != nil || records == nil || len(records) > 100000 {
			return CachePresence{State: "unavailable"}, nil
		}
		if q.Season == nil && q.Episode == nil {
			matched = true
		}
		for _, ep := range records {
			if ep.Season == nil || ep.Episode == nil || *ep.Season < 0 || *ep.Season > 10000 || *ep.Episode < 0 || *ep.Episode > 10000 {
				return CachePresence{State: "unavailable"}, nil
			}
			season := q.Season
			if season == nil && q.Episode != nil {
				one := 1
				season = &one
			}
			if season != nil && *ep.Season != *season || q.Episode != nil && *ep.Episode != *q.Episode {
				continue
			}
			matched = true
			pairs[[2]int{*ep.Season, *ep.Episode}] = true
			seasons[*ep.Season] = true
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if matched {
		result.State = "present"
	}
	result.Seasons = len(seasons)
	result.Episodes = len(pairs)
	return result, nil
}
