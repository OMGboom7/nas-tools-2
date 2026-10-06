package siteconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound  = errors.New("site not found")
	ErrDuplicate = errors.New("site name already exists")
)

type Site struct {
	ID       int64
	Name     string
	Priority string
	RSSURL   string
	SignURL  string
	Cookie   string
	APIKey   string
	Include  string
	Exclude  string
	Size     string
	Note     string
}

type Store struct {
	database *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create site database directory: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open site database: %w", err)
	}
	database.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"CREATE TABLE IF NOT EXISTS CONFIG_SITE (ID INTEGER PRIMARY KEY AUTOINCREMENT, NAME TEXT, PRI TEXT, RSSURL TEXT, SIGNURL TEXT, COOKIE TEXT, APIKEY TEXT, INCLUDE TEXT, EXCLUDE TEXT, SIZE TEXT, NOTE TEXT)",
	} {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			return nil, fmt.Errorf("initialize site database: %w", err)
		}
	}
	return &Store{database: database}, nil
}

func (store *Store) List(ctx context.Context) ([]Site, error) {
	rows, err := store.database.QueryContext(ctx, siteSelect+" ORDER BY CAST(PRI AS INTEGER), ID")
	if err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()
	items := make([]Site, 0)
	for rows.Next() {
		item, err := scanSite(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}
	return items, nil
}

func (store *Store) Get(ctx context.Context, id int64) (Site, error) {
	item, err := scanSite(store.database.QueryRowContext(ctx, siteSelect+" WHERE ID = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, ErrNotFound
	}
	return item, err
}

func (store *Store) Upsert(ctx context.Context, item Site) (Site, error) {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return Site{}, fmt.Errorf("begin site update: %w", err)
	}
	defer transaction.Rollback()
	var duplicate int
	if err := transaction.QueryRowContext(ctx, "SELECT COUNT(1) FROM CONFIG_SITE WHERE NAME = ? AND ID != ?", item.Name, item.ID).Scan(&duplicate); err != nil {
		return Site{}, fmt.Errorf("check duplicate site: %w", err)
	}
	if duplicate != 0 {
		return Site{}, ErrDuplicate
	}
	if item.ID == 0 {
		result, err := transaction.ExecContext(ctx, "INSERT INTO CONFIG_SITE (NAME, PRI, RSSURL, SIGNURL, COOKIE, APIKEY, INCLUDE, EXCLUDE, SIZE, NOTE) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", item.Name, item.Priority, item.RSSURL, item.SignURL, item.Cookie, item.APIKey, item.Include, item.Exclude, item.Size, item.Note)
		if err != nil {
			return Site{}, fmt.Errorf("insert site: %w", err)
		}
		item.ID, err = result.LastInsertId()
		if err != nil {
			return Site{}, fmt.Errorf("read inserted site ID: %w", err)
		}
	} else {
		var oldName string
		if err := transaction.QueryRowContext(ctx, "SELECT NAME FROM CONFIG_SITE WHERE ID = ?", item.ID).Scan(&oldName); errors.Is(err, sql.ErrNoRows) {
			return Site{}, ErrNotFound
		} else if err != nil {
			return Site{}, fmt.Errorf("read existing site: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE CONFIG_SITE SET NAME=?, PRI=?, RSSURL=?, SIGNURL=?, COOKIE=?, APIKEY=?, INCLUDE=?, EXCLUDE=?, SIZE=?, NOTE=? WHERE ID=?", item.Name, item.Priority, item.RSSURL, item.SignURL, item.Cookie, item.APIKey, item.Include, item.Exclude, item.Size, item.Note, item.ID); err != nil {
			return Site{}, fmt.Errorf("update site: %w", err)
		}
		if oldName != item.Name {
			for _, table := range []string{"SITE_USER_INFO_STATS", "SITE_USER_SEEDING_INFO", "SITE_STATISTICS_HISTORY"} {
				var exists int
				if err := transaction.QueryRowContext(ctx, "SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists); err != nil {
					return Site{}, fmt.Errorf("check site history table: %w", err)
				}
				if exists != 0 {
					if _, err := transaction.ExecContext(ctx, "UPDATE "+table+" SET SITE=? WHERE SITE=?", item.Name, oldName); err != nil {
						return Site{}, fmt.Errorf("rename site history: %w", err)
					}
				}
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return Site{}, fmt.Errorf("commit site update: %w", err)
	}
	return item, nil
}

func (store *Store) Delete(ctx context.Context, id int64) error {
	result, err := store.database.ExecContext(ctx, "DELETE FROM CONFIG_SITE WHERE ID = ?", id)
	if err != nil {
		return fmt.Errorf("delete site: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted site count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) UpdateCookie(ctx context.Context, id int64, cookie, note string) error {
	result, err := store.database.ExecContext(ctx, "UPDATE CONFIG_SITE SET COOKIE=?, NOTE=? WHERE ID=?", cookie, note, id)
	if err != nil {
		return fmt.Errorf("update site cookie: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read updated site count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) UpdateCookieAndAgent(ctx context.Context, id int64, cookie, agent string) error {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin site cookie update: %w", err)
	}
	defer transaction.Rollback()
	var raw string
	if err := transaction.QueryRowContext(ctx, "SELECT COALESCE(NOTE,'') FROM CONFIG_SITE WHERE ID=?", id).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read site attributes: %w", err)
	}
	note := map[string]any{}
	if raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &note); err != nil {
			return fmt.Errorf("decode site attributes: %w", err)
		}
	}
	if agent != "" {
		note["ua"] = agent
	}
	encoded, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("encode site attributes: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE CONFIG_SITE SET COOKIE=?, NOTE=? WHERE ID=?", cookie, string(encoded), id); err != nil {
		return fmt.Errorf("update site cookie: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit site cookie update: %w", err)
	}
	return nil
}

func (store *Store) Close() error { return store.database.Close() }

const siteSelect = "SELECT ID, COALESCE(NAME,''), COALESCE(PRI,''), COALESCE(RSSURL,''), COALESCE(SIGNURL,''), COALESCE(COOKIE,''), COALESCE(APIKEY,''), COALESCE(INCLUDE,''), COALESCE(EXCLUDE,''), COALESCE(SIZE,''), COALESCE(NOTE,'') FROM CONFIG_SITE"

type scanner interface{ Scan(...any) error }

func scanSite(row scanner) (Site, error) {
	var item Site
	err := row.Scan(&item.ID, &item.Name, &item.Priority, &item.RSSURL, &item.SignURL, &item.Cookie, &item.APIKey, &item.Include, &item.Exclude, &item.Size, &item.Note)
	if err != nil {
		return Site{}, err
	}
	return item, nil
}
