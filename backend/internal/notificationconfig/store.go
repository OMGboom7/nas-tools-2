package notificationconfig

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

var ErrNotFound = errors.New("notification channel not found")

type Channel struct {
	ID          int64
	Name        string
	Type        string
	Config      string
	Switches    string
	Interactive int
	Enabled     int
	Note        string
}

type Store struct{ database *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create notification database directory: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open notification database: %w", err)
	}
	database.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"CREATE TABLE IF NOT EXISTS MESSAGE_CLIENT (ID INTEGER PRIMARY KEY AUTOINCREMENT, NAME TEXT, TYPE TEXT, CONFIG TEXT, SWITCHS TEXT, INTERACTIVE INTEGER, ENABLED INTEGER, NOTE TEXT)",
	} {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			return nil, fmt.Errorf("initialize notification database: %w", err)
		}
	}
	return &Store{database: database}, nil
}

const channelSelect = "SELECT ID, COALESCE(NAME,''), COALESCE(TYPE,''), COALESCE(CONFIG,''), COALESCE(SWITCHS,''), COALESCE(INTERACTIVE,0), COALESCE(ENABLED,0), COALESCE(NOTE,'') FROM MESSAGE_CLIENT"

func (store *Store) List(ctx context.Context) ([]Channel, error) {
	rows, err := store.database.QueryContext(ctx, channelSelect+" ORDER BY ID")
	if err != nil {
		return nil, fmt.Errorf("list notification channels: %w", err)
	}
	defer rows.Close()
	channels := []Channel{}
	for rows.Next() {
		channel, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification channels: %w", err)
	}
	return channels, nil
}

func (store *Store) Get(ctx context.Context, id int64) (Channel, error) {
	channel, err := scanChannel(store.database.QueryRowContext(ctx, channelSelect+" WHERE ID=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, ErrNotFound
	}
	return channel, err
}

// Save writes a complete channel while preserving the ID and note of an existing row.
// Enabling interaction clears the flag on other channels of the same type atomically.
func (store *Store) Save(ctx context.Context, channel Channel) (int64, error) {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin notification channel save: %w", err)
	}
	defer tx.Rollback()
	if channel.ID != 0 {
		var currentType string
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(TYPE,'') FROM MESSAGE_CLIENT WHERE ID=?", channel.ID).Scan(&currentType); errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		} else if err != nil {
			return 0, fmt.Errorf("read notification channel: %w", err)
		}
		if currentType != channel.Type {
			return 0, errors.New("notification channel type cannot be changed")
		}
	}
	if channel.Interactive != 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE MESSAGE_CLIENT SET INTERACTIVE=0 WHERE TYPE=? AND ID<>?", channel.Type, channel.ID); err != nil {
			return 0, fmt.Errorf("disable other interactive channels: %w", err)
		}
	}
	if channel.ID == 0 {
		result, err := tx.ExecContext(ctx, "INSERT INTO MESSAGE_CLIENT (NAME, TYPE, CONFIG, SWITCHS, INTERACTIVE, ENABLED, NOTE) VALUES (?, ?, ?, ?, ?, ?, ?)", channel.Name, channel.Type, channel.Config, channel.Switches, channel.Interactive, channel.Enabled, channel.Note)
		if err != nil {
			return 0, fmt.Errorf("insert notification channel: %w", err)
		}
		channel.ID, err = result.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("read notification channel ID: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, "UPDATE MESSAGE_CLIENT SET NAME=?, CONFIG=?, SWITCHS=?, INTERACTIVE=?, ENABLED=? WHERE ID=?", channel.Name, channel.Config, channel.Switches, channel.Interactive, channel.Enabled, channel.ID); err != nil {
			return 0, fmt.Errorf("update notification channel: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit notification channel save: %w", err)
	}
	return channel.ID, nil
}

func (store *Store) SetStatus(ctx context.Context, id int64, interactive bool, enabled bool) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin notification status update: %w", err)
	}
	defer tx.Rollback()
	var channelType string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(TYPE,'') FROM MESSAGE_CLIENT WHERE ID=?", id).Scan(&channelType); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read notification channel: %w", err)
	}
	if interactive && enabled {
		if _, err := tx.ExecContext(ctx, "UPDATE MESSAGE_CLIENT SET INTERACTIVE=0 WHERE TYPE=?", channelType); err != nil {
			return fmt.Errorf("disable other interactive channels: %w", err)
		}
	}
	flag, value := "ENABLED", enabled
	if interactive {
		flag, value = "INTERACTIVE", enabled
	}
	if _, err := tx.ExecContext(ctx, "UPDATE MESSAGE_CLIENT SET "+flag+"=? WHERE ID=?", boolInt(value), id); err != nil {
		return fmt.Errorf("update notification status: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit notification status update: %w", err)
	}
	return nil
}

func (store *Store) Delete(ctx context.Context, id int64) error {
	result, err := store.database.ExecContext(ctx, "DELETE FROM MESSAGE_CLIENT WHERE ID=?", id)
	if err != nil {
		return fmt.Errorf("delete notification channel: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted notification channel: %w", err)
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (store *Store) Close() error { return store.database.Close() }

type scanner interface{ Scan(...any) error }

func scanChannel(row scanner) (Channel, error) {
	var channel Channel
	if err := row.Scan(&channel.ID, &channel.Name, &channel.Type, &channel.Config, &channel.Switches, &channel.Interactive, &channel.Enabled, &channel.Note); err != nil {
		return Channel{}, err
	}
	return channel, nil
}
