package mediasync

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/database"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	_ "modernc.org/sqlite"
)

var ErrSnapshot = errors.New("invalid media snapshot")

type Store struct{ db *sql.DB }
type Snapshot struct {
	Server       string                 `json:"server"`
	Generation   string                 `json:"generation"`
	ConfigDigest string                 `json:"configDigest"`
	Libraries    []string               `json:"libraries"`
	Actor        string                 `json:"actor"`
	Updated      string                 `json:"updated"`
	Total        int                    `json:"total"`
	Movies       int                    `json:"movies"`
	Series       int                    `json:"series"`
	Page         int                    `json:"page"`
	Items        []mediaserver.SyncItem `json:"items"`
}

func Open(path string) (*Store, error) {
	if _, err := database.EnsureMigrationBackup(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS MEDIASYNC_ITEMS (ID INTEGER PRIMARY KEY, SERVER TEXT, LIBRARY TEXT, ITEM_ID TEXT, ITEM_TYPE TEXT, TITLE TEXT, ORGIN_TITLE TEXT, YEAR TEXT, TMDBID TEXT, IMDBID TEXT, PATH TEXT, NOTE TEXT, JSON TEXT)`,
		`CREATE INDEX IF NOT EXISTS INDX_MEDIASYNC_ITEMS_SL ON MEDIASYNC_ITEMS(SERVER,LIBRARY)`,
		`CREATE TABLE IF NOT EXISTS MEDIASYNC_STATISTICS (ID INTEGER PRIMARY KEY, SERVER TEXT, TOTAL_COUNT TEXT, MOVIE_COUNT TEXT, TV_COUNT TEXT, UPDATE_TIME TEXT)`,
		`CREATE TABLE IF NOT EXISTS GO_MEDIASYNC_SNAPSHOTS (SERVER TEXT PRIMARY KEY, DOCUMENT TEXT NOT NULL)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

// Publish replaces one server's display cache and both statistics tables in a
// single transaction. Callers must hold the cross-process media-sync guard.
func (s *Store) Publish(ctx context.Context, server, digest, actor string, libraries []string, items []mediaserver.SyncItem) (Snapshot, error) {
	result := Snapshot{Server: server, ConfigDigest: digest, Actor: actor, Libraries: libraries, Updated: time.Now().UTC().Format(time.RFC3339Nano), Total: len(items), Items: []mediaserver.SyncItem{}}
	if (server != "emby" && server != "jellyfin" && server != "plex") || len(digest) != 64 || actor == "" || len(libraries) == 0 || len(libraries) > 100 || len(items) > 100000 {
		return result, ErrSnapshot
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return result, ErrSnapshot
	}
	selected := map[string]bool{}
	for _, library := range libraries {
		if library == "" || selected[library] {
			return result, ErrSnapshot
		}
		selected[library] = true
	}
	seen := map[string]bool{}
	for _, item := range items {
		if item.ID == "" || item.Title == "" || seen[item.ID] || !selected[item.Library] || item.Type != "movie" && item.Type != "show" {
			return result, ErrSnapshot
		}
		seen[item.ID] = true
		if item.Type == "movie" {
			result.Movies++
		} else {
			result.Series++
		}
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return result, err
	}
	result.Generation = hex.EncodeToString(id[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for _, statement := range []string{`DELETE FROM MEDIASYNC_ITEMS WHERE SERVER=?`, `DELETE FROM MEDIASYNC_STATISTICS WHERE SERVER=?`} {
		if _, err := tx.ExecContext(ctx, statement, server); err != nil {
			return result, err
		}
	}
	for _, item := range items {
		episodes, err := json.Marshal(item.Episodes)
		if err != nil {
			return result, err
		}
		kind := item.Type
		if server != "plex" {
			if kind == "movie" {
				kind = "Movie"
			} else {
				kind = "Series"
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO MEDIASYNC_ITEMS (SERVER,LIBRARY,ITEM_ID,ITEM_TYPE,TITLE,ORGIN_TITLE,YEAR,TMDBID,IMDBID,PATH,JSON) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, server, item.Library, item.ID, kind, item.Title, item.OriginalTitle, item.Year, item.TMDBID, item.IMDBID, item.Path, string(episodes))
		if err != nil {
			return result, err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return result, ErrSnapshot
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO MEDIASYNC_STATISTICS (SERVER,TOTAL_COUNT,MOVIE_COUNT,TV_COUNT,UPDATE_TIME) VALUES(?,?,?,?,?)`, server, result.Total, result.Movies, result.Series, result.Updated)
	if err != nil {
		return result, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return result, ErrSnapshot
	}
	document, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	var itemCount, statisticCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM MEDIASYNC_ITEMS WHERE SERVER=?`, server).Scan(&itemCount); err != nil {
		return result, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM MEDIASYNC_STATISTICS WHERE SERVER=?`, server).Scan(&statisticCount); err != nil {
		return result, err
	}
	if itemCount != result.Total || statisticCount != 1 {
		return result, ErrSnapshot
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO GO_MEDIASYNC_SNAPSHOTS(SERVER,DOCUMENT) VALUES(?,?) ON CONFLICT(SERVER) DO UPDATE SET DOCUMENT=excluded.DOCUMENT`, server, string(document))
	if err != nil {
		return result, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return result, ErrSnapshot
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) View(ctx context.Context, server string, page int) (Snapshot, error) {
	result := Snapshot{Server: server, Page: page, Items: []mediaserver.SyncItem{}, Libraries: []string{}}
	if page < 1 || page > 1000 {
		return result, ErrSnapshot
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var document string
	err = tx.QueryRowContext(ctx, `SELECT DOCUMENT FROM GO_MEDIASYNC_SNAPSHOTS WHERE SERVER=?`, server).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal([]byte(document), &result); err != nil {
		return result, err
	}
	result.Page = page
	rows, err := tx.QueryContext(ctx, `SELECT ITEM_ID,LIBRARY,ITEM_TYPE,TITLE,COALESCE(ORGIN_TITLE,''),COALESCE(YEAR,''),COALESCE(TMDBID,''),COALESCE(IMDBID,''),COALESCE(PATH,''),COALESCE(JSON,'[]') FROM MEDIASYNC_ITEMS WHERE SERVER=? ORDER BY ID LIMIT 100 OFFSET ?`, server, (page-1)*100)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item mediaserver.SyncItem
		var episodes string
		if err := rows.Scan(&item.ID, &item.Library, &item.Type, &item.Title, &item.OriginalTitle, &item.Year, &item.TMDBID, &item.IMDBID, &item.Path, &episodes); err != nil {
			return result, err
		}
		if err := json.Unmarshal([]byte(episodes), &item.Episodes); err != nil {
			return result, err
		}
		if item.Type == "Movie" {
			item.Type = "movie"
		}
		if item.Type == "Series" {
			item.Type = "show"
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
