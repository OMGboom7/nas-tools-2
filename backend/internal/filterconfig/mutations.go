package filterconfig

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strconv"
)

var ErrNotFound = errors.New("filter group or rule not found")

type Rule struct {
	ID                                           int64
	GroupID                                      int64
	Name, Priority, Include, Exclude, Size, Free string
}

// OpenWritable is used only after the server has made its migration backup.
func OpenWritable(path string) (*Store, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"CREATE TABLE IF NOT EXISTS CONFIG_FILTER_GROUP (ID INTEGER PRIMARY KEY, GROUP_NAME TEXT, IS_DEFAULT TEXT, NOTE TEXT)",
		"CREATE TABLE IF NOT EXISTS CONFIG_FILTER_RULES (ID INTEGER PRIMARY KEY, GROUP_ID TEXT, ROLE_NAME TEXT, PRIORITY TEXT, INCLUDE TEXT, EXCLUDE TEXT, SIZE_LIMIT TEXT, NOTE TEXT)",
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Store{database: db}, nil
}

func (store *Store) transaction(ctx context.Context, action func(*sql.Tx) error) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := action(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func requireGroup(ctx context.Context, tx *sql.Tx, id int64) error {
	var found int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM CONFIG_FILTER_GROUP WHERE ID=?", id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (store *Store) AddGroup(ctx context.Context, name string, isDefault bool) (int64, error) {
	var id int64
	err := store.transaction(ctx, func(tx *sql.Tx) error {
		flag := "N"
		if isDefault {
			flag = "Y"
			if _, err := tx.ExecContext(ctx, "UPDATE CONFIG_FILTER_GROUP SET IS_DEFAULT='N'"); err != nil {
				return err
			}
		}
		err := tx.QueryRowContext(ctx, "SELECT ID FROM CONFIG_FILTER_GROUP WHERE GROUP_NAME=? ORDER BY ID LIMIT 1", name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			result, err := tx.ExecContext(ctx, "INSERT INTO CONFIG_FILTER_GROUP (GROUP_NAME, IS_DEFAULT) VALUES (?, ?)", name, flag)
			if err != nil {
				return err
			}
			id, err = result.LastInsertId()
			return err
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE CONFIG_FILTER_GROUP SET IS_DEFAULT=? WHERE ID=?", flag, id)
		return err
	})
	return id, err
}

func (store *Store) SetDefault(ctx context.Context, id int64) error {
	return store.transaction(ctx, func(tx *sql.Tx) error {
		if id != 0 {
			if err := requireGroup(ctx, tx, id); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE CONFIG_FILTER_GROUP SET IS_DEFAULT=CASE WHEN ID=? THEN 'Y' ELSE 'N' END", id)
		return err
	})
}

func (store *Store) DeleteGroup(ctx context.Context, id int64) error {
	return store.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireGroup(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM CONFIG_FILTER_RULES WHERE GROUP_ID=?", strconv.FormatInt(id, 10)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM CONFIG_FILTER_GROUP WHERE ID=?", id)
		return err
	})
}

func (store *Store) SaveRule(ctx context.Context, rule Rule) (int64, error) {
	id := rule.ID
	err := store.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireGroup(ctx, tx, rule.GroupID); err != nil {
			return err
		}
		if id == 0 {
			result, err := tx.ExecContext(ctx, "INSERT INTO CONFIG_FILTER_RULES (GROUP_ID, ROLE_NAME, PRIORITY, INCLUDE, EXCLUDE, SIZE_LIMIT, NOTE) VALUES (?, ?, ?, ?, ?, ?, ?)", strconv.FormatInt(rule.GroupID, 10), rule.Name, rule.Priority, rule.Include, rule.Exclude, rule.Size, rule.Free)
			if err != nil {
				return err
			}
			id, err = result.LastInsertId()
			return err
		}
		// Updating a rule must not silently move it to another group.
		result, err := tx.ExecContext(ctx, "UPDATE CONFIG_FILTER_RULES SET ROLE_NAME=?, PRIORITY=?, INCLUDE=?, EXCLUDE=?, SIZE_LIMIT=?, NOTE=? WHERE ID=? AND GROUP_ID=?", rule.Name, rule.Priority, rule.Include, rule.Exclude, rule.Size, rule.Free, id, strconv.FormatInt(rule.GroupID, 10))
		return changed(result, err)
	})
	return id, err
}

func changed(result sql.Result, err error) error {
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

func (store *Store) DeleteRule(ctx context.Context, id int64) error {
	result, err := store.database.ExecContext(ctx, "DELETE FROM CONFIG_FILTER_RULES WHERE ID=?", id)
	return changed(result, err)
}

func (store *Store) Rule(ctx context.Context, groupID, id int64) (Rule, error) {
	var rule Rule
	err := store.database.QueryRowContext(ctx, "SELECT ID, GROUP_ID, COALESCE(ROLE_NAME,''), COALESCE(PRIORITY,'0'), COALESCE(INCLUDE,''), COALESCE(EXCLUDE,''), COALESCE(SIZE_LIMIT,''), COALESCE(NOTE,'') FROM CONFIG_FILTER_RULES WHERE ID=? AND GROUP_ID=?", id, strconv.FormatInt(groupID, 10)).Scan(&rule.ID, &rule.GroupID, &rule.Name, &rule.Priority, &rule.Include, &rule.Exclude, &rule.Size, &rule.Free)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	return rule, err
}
