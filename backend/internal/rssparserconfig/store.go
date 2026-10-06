package rssparserconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("rss parser not found")

type Parser struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Format string `json:"format"`
	Params string `json:"params"`
	Note   string `json:"note"`
}

type Store struct{ database *sql.DB }

func Open(path string) (*Store, error) {
	dsn := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open rss parser configuration: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS CONFIG_RSS_PARSER (
		ID INTEGER PRIMARY KEY, NAME TEXT, TYPE TEXT, FORMAT TEXT, PARAMS TEXT, NOTE TEXT, SYSDEF TEXT
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure rss parser table: %w", err)
	}
	return &Store{database: db}, nil
}

func (store *Store) Close() error { return store.database.Close() }

func (store *Store) List(ctx context.Context) ([]Parser, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT ID, COALESCE(NAME,''), COALESCE(TYPE,''), COALESCE(FORMAT,''), COALESCE(PARAMS,''), COALESCE(NOTE,'') FROM CONFIG_RSS_PARSER ORDER BY ID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Parser, 0)
	for rows.Next() {
		var parser Parser
		if err := rows.Scan(&parser.ID, &parser.Name, &parser.Type, &parser.Format, &parser.Params, &parser.Note); err != nil {
			return nil, err
		}
		result = append(result, parser)
	}
	return result, rows.Err()
}

func (store *Store) Get(ctx context.Context, id int64) (Parser, error) {
	var parser Parser
	err := store.database.QueryRowContext(ctx, `SELECT ID, COALESCE(NAME,''), COALESCE(TYPE,''), COALESCE(FORMAT,''), COALESCE(PARAMS,''), COALESCE(NOTE,'') FROM CONFIG_RSS_PARSER WHERE ID=?`, id).Scan(&parser.ID, &parser.Name, &parser.Type, &parser.Format, &parser.Params, &parser.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Parser{}, ErrNotFound
	}
	return parser, err
}

// Upsert mirrors the legacy behavior: an unknown ID creates a new row, while
// updating an existing parser leaves NOTE and SYSDEF unchanged.
func (store *Store) Upsert(ctx context.Context, parser Parser) (int64, error) {
	if parser.ID > 0 {
		_, err := store.Get(ctx, parser.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
		if err == nil {
			_, err = store.database.ExecContext(ctx, `UPDATE CONFIG_RSS_PARSER SET NAME=?, TYPE=?, FORMAT=?, PARAMS=? WHERE ID=?`, parser.Name, parser.Type, parser.Format, parser.Params, parser.ID)
			return parser.ID, err
		}
	}
	result, err := store.database.ExecContext(ctx, `INSERT INTO CONFIG_RSS_PARSER (NAME, TYPE, FORMAT, PARAMS) VALUES (?,?,?,?)`, parser.Name, parser.Type, parser.Format, parser.Params)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (store *Store) Delete(ctx context.Context, id int64) error {
	result, err := store.database.ExecContext(ctx, `DELETE FROM CONFIG_RSS_PARSER WHERE ID=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}
