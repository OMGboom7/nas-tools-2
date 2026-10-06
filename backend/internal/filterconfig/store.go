package filterconfig

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

type Group struct {
	ID      int64
	Name    string
	Default bool
	Note    string
}

// Store reads the existing rules database without caching Python-owned state.
type Store struct{ database *sql.DB }

func Open(path string) (*Store, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open filter configuration: %w", err)
	}
	return &Store{database: db}, nil
}

func (store *Store) Groups(ctx context.Context) ([]Group, error) {
	// A fresh Go installation has no saved groups until they are configured.
	var exists int
	if err := store.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='CONFIG_FILTER_GROUP'").Scan(&exists); err != nil {
		return nil, err
	}
	groups := make([]Group, 0)
	if exists == 0 {
		return groups, nil
	}
	rows, err := store.database.QueryContext(ctx, "SELECT ID, COALESCE(GROUP_NAME,''), COALESCE(IS_DEFAULT,'N'), COALESCE(NOTE,'') FROM CONFIG_FILTER_GROUP ORDER BY ID")
	if err != nil {
		return nil, fmt.Errorf("read filter groups: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var group Group
		var flag string
		if err := rows.Scan(&group.ID, &group.Name, &flag, &group.Note); err != nil {
			return nil, err
		}
		group.Default = flag == "Y"
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (store *Store) Close() error { return store.database.Close() }
