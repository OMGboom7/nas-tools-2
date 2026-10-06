package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

var (
	ErrUserNotFound = errors.New("user not found")
	ErrUserExists   = errors.New("user already exists")
)

type User struct {
	ID          int64
	Name        string
	Password    string
	Permissions []string
}

type UserStore interface {
	FindByName(context.Context, string) (User, error)
	List(context.Context) ([]User, error)
	Create(context.Context, User) (User, error)
	Delete(context.Context, string) error
	UpdatePassword(context.Context, int64, string) error
}

type SQLiteUserStore struct {
	database *sql.DB
}

func OpenSQLiteUserStore(path string) (*SQLiteUserStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create user database directory: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open user database: %w", err)
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("configure user database: %w", err)
	}
	if _, err := database.Exec("CREATE TABLE IF NOT EXISTS CONFIG_USERS (ID INTEGER PRIMARY KEY AUTOINCREMENT, NAME TEXT NOT NULL, PASSWORD TEXT NOT NULL, PRIS TEXT)"); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("initialize user database: %w", err)
	}
	return &SQLiteUserStore{database: database}, nil
}

func (store *SQLiteUserStore) List(ctx context.Context) ([]User, error) {
	rows, err := store.database.QueryContext(ctx,
		"SELECT ID, NAME, PASSWORD, COALESCE(PRIS, '') FROM CONFIG_USERS ORDER BY ID")
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		var permissions string
		if err := rows.Scan(&user.ID, &user.Name, &user.Password, &permissions); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		user.Permissions = splitPermissions(permissions)
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

func (store *SQLiteUserStore) Create(ctx context.Context, user User) (User, error) {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin create user: %w", err)
	}
	defer transaction.Rollback()
	var count int
	if err := transaction.QueryRowContext(ctx, "SELECT COUNT(1) FROM CONFIG_USERS WHERE NAME = ?", user.Name).Scan(&count); err != nil {
		return User{}, fmt.Errorf("check existing user: %w", err)
	}
	if count != 0 {
		return User{}, ErrUserExists
	}
	result, err := transaction.ExecContext(ctx,
		"INSERT INTO CONFIG_USERS (NAME, PASSWORD, PRIS) VALUES (?, ?, ?)",
		user.Name, user.Password, strings.Join(user.Permissions, ","))
	if err != nil {
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	user.ID, err = result.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("read inserted user ID: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return User{}, fmt.Errorf("commit create user: %w", err)
	}
	return user, nil
}

func (store *SQLiteUserStore) Delete(ctx context.Context, name string) error {
	result, err := store.database.ExecContext(ctx, "DELETE FROM CONFIG_USERS WHERE NAME = ?", name)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted user count: %w", err)
	}
	if count == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (store *SQLiteUserStore) UpdatePassword(ctx context.Context, id int64, password string) error {
	_, err := store.database.ExecContext(ctx, "UPDATE CONFIG_USERS SET PASSWORD = ? WHERE ID = ?", password, id)
	if err != nil {
		return fmt.Errorf("upgrade user password: %w", err)
	}
	return nil
}

func (store *SQLiteUserStore) FindByName(ctx context.Context, name string) (User, error) {
	var user User
	var permissions string
	err := store.database.QueryRowContext(ctx,
		"SELECT ID, NAME, PASSWORD, COALESCE(PRIS, '') FROM CONFIG_USERS WHERE NAME = ? LIMIT 1", name,
	).Scan(&user.ID, &user.Name, &user.Password, &permissions)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("query user: %w", err)
	}
	user.Permissions = splitPermissions(permissions)
	return user, nil
}

func (store *SQLiteUserStore) Close() error {
	return store.database.Close()
}

func splitPermissions(value string) []string {
	parts := strings.Split(value, ",")
	permissions := make([]string, 0, len(parts))
	for _, part := range parts {
		if permission := strings.TrimSpace(part); permission != "" {
			permissions = append(permissions, permission)
		}
	}
	return permissions
}
