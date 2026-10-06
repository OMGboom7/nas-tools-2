package systemconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

var ErrInvalidSetting = errors.New("system setting key and value are required")

type Store struct {
	database *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create system database directory: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open system database: %w", err)
	}
	database.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"CREATE TABLE IF NOT EXISTS SYSTEM_DICT (ID INTEGER PRIMARY KEY AUTOINCREMENT, TYPE TEXT, KEY TEXT, VALUE TEXT, NOTE TEXT)",
		"CREATE INDEX IF NOT EXISTS INDX_SYSTEM_DICT ON SYSTEM_DICT (TYPE, KEY)",
	} {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			return nil, fmt.Errorf("initialize system database: %w", err)
		}
	}
	return &Store{database: database}, nil
}

func (store *Store) Set(ctx context.Context, key, value string) error {
	if key == "" || value == "" {
		return ErrInvalidSetting
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin system setting update: %w", err)
	}
	defer transaction.Rollback()
	result, err := transaction.ExecContext(ctx,
		"UPDATE SYSTEM_DICT SET VALUE = ? WHERE TYPE = 'SystemConfig' AND KEY = ?", value, key)
	if err != nil {
		return fmt.Errorf("update system setting: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read system setting update count: %w", err)
	}
	if updated == 0 {
		if _, err := transaction.ExecContext(ctx,
			"INSERT INTO SYSTEM_DICT (TYPE, KEY, VALUE, NOTE) VALUES ('SystemConfig', ?, ?, '')", key, value); err != nil {
			return fmt.Errorf("insert system setting: %w", err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit system setting update: %w", err)
	}
	return nil
}

func (store *Store) Get(ctx context.Context, key string) (string, error) {
	var value string
	err := store.database.QueryRowContext(ctx,
		"SELECT VALUE FROM SYSTEM_DICT WHERE TYPE = 'SystemConfig' AND KEY = ? ORDER BY ID DESC LIMIT 1", key,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read system setting: %w", err)
	}
	return value, nil
}

// Update serializes read/modify/write on the same setting. A callback error
// leaves the stored value unchanged, including across concurrent requests.
func (store *Store) Update(ctx context.Context, key string, transform func(string) (string, error)) error {
	if key == "" || transform == nil {
		return ErrInvalidSetting
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var current string
	err = transaction.QueryRowContext(ctx, "SELECT VALUE FROM SYSTEM_DICT WHERE TYPE = 'SystemConfig' AND KEY = ? ORDER BY ID DESC LIMIT 1", key).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	next, err := transform(current)
	if err != nil {
		return err
	}
	if next == "" {
		return ErrInvalidSetting
	}
	result, err := transaction.ExecContext(ctx, "UPDATE SYSTEM_DICT SET VALUE = ? WHERE TYPE = 'SystemConfig' AND KEY = ?", next, key)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		if _, err := transaction.ExecContext(ctx, "INSERT INTO SYSTEM_DICT (TYPE, KEY, VALUE, NOTE) VALUES ('SystemConfig', ?, ?, '')", key, next); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (store *Store) Close() error {
	return store.database.Close()
}
