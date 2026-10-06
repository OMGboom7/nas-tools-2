package filterconfig

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"strconv"
)

//go:embed init_filter.sql
var builtinSQL string

var ErrInvalidTemplate = errors.New("unknown filter template")
var ErrTemplateConflict = errors.New("template rule ID belongs to another group")

type GroupInfo struct {
	Group Group
	Rules []Rule
}

// List returns groups and their rules from a single database snapshot.
func (store *Store) List(ctx context.Context) ([]GroupInfo, error) {
	groups := make([]GroupInfo, 0)
	err := store.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT ID, COALESCE(GROUP_NAME,''), COALESCE(IS_DEFAULT,'N'), COALESCE(NOTE,'') FROM CONFIG_FILTER_GROUP ORDER BY ID")
		if err != nil {
			return err
		}
		positions := map[int64]int{}
		for rows.Next() {
			var item GroupInfo
			var flag string
			if err := rows.Scan(&item.Group.ID, &item.Group.Name, &flag, &item.Group.Note); err != nil {
				rows.Close()
				return err
			}
			item.Group.Default = flag == "Y"
			item.Rules = []Rule{}
			positions[item.Group.ID] = len(groups)
			groups = append(groups, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, "SELECT ID, COALESCE(GROUP_ID,''), COALESCE(ROLE_NAME,''), COALESCE(PRIORITY,'0'), COALESCE(INCLUDE,''), COALESCE(EXCLUDE,''), COALESCE(SIZE_LIMIT,''), COALESCE(NOTE,'') FROM CONFIG_FILTER_RULES ORDER BY GROUP_ID, CAST(PRIORITY AS INTEGER), ID")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rule Rule
			var groupID string
			if err := rows.Scan(&rule.ID, &groupID, &rule.Name, &rule.Priority, &rule.Include, &rule.Exclude, &rule.Size, &rule.Free); err != nil {
				return err
			}
			rule.GroupID, err = strconv.ParseInt(groupID, 10, 64)
			if err != nil {
				continue
			} // Unattached legacy rules are not part of any group.
			if index, found := positions[rule.GroupID]; found {
				groups[index].Rules = append(groups[index].Rules, rule)
			}
		}
		return rows.Err()
	})
	return groups, err
}

// Builtins executes only the embedded, version-controlled SQL in a private
// in-memory database. Request-supplied SQL is never used.
func Builtins(ctx context.Context) ([]GroupInfo, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"CREATE TABLE CONFIG_FILTER_GROUP (ID INTEGER PRIMARY KEY, GROUP_NAME TEXT, IS_DEFAULT TEXT, NOTE TEXT)",
		"CREATE TABLE CONFIG_FILTER_RULES (ID INTEGER PRIMARY KEY, GROUP_ID TEXT, ROLE_NAME TEXT, PRIORITY TEXT, INCLUDE TEXT, EXCLUDE TEXT, SIZE_LIMIT TEXT, NOTE TEXT)",
		builtinSQL,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, err
		}
	}
	return (&Store{database: db}).List(ctx)
}

func (store *Store) Restore(ctx context.Context, ids []int64) error {
	templates, err := Builtins(ctx)
	if err != nil {
		return err
	}
	available := map[int64]GroupInfo{}
	for _, template := range templates {
		available[template.Group.ID] = template
	}
	selected := map[int64]bool{}
	for _, id := range ids {
		if _, ok := available[id]; !ok {
			return ErrInvalidTemplate
		}
		selected[id] = true
	}
	return store.transaction(ctx, func(tx *sql.Tx) error {
		// Delete every selected group's rules before inserting to support restoring
		// multiple templates together without silently ignoring ID collisions.
		for id := range selected {
			if _, err := tx.ExecContext(ctx, "DELETE FROM CONFIG_FILTER_RULES WHERE GROUP_ID=?", strconv.FormatInt(id, 10)); err != nil {
				return err
			}
		}
		for _, template := range templates {
			if !selected[template.Group.ID] {
				continue
			}
			flag := "N"
			if template.Group.Default {
				flag = "Y"
				if _, err := tx.ExecContext(ctx, "UPDATE CONFIG_FILTER_GROUP SET IS_DEFAULT='N'"); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO CONFIG_FILTER_GROUP (ID,GROUP_NAME,IS_DEFAULT,NOTE) VALUES (?,?,?,NULL) ON CONFLICT(ID) DO UPDATE SET GROUP_NAME=excluded.GROUP_NAME, IS_DEFAULT=excluded.IS_DEFAULT, NOTE=NULL", template.Group.ID, template.Group.Name, flag); err != nil {
				return err
			}
			for _, rule := range template.Rules {
				var count int
				if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM CONFIG_FILTER_RULES WHERE ID=?", rule.ID).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					return ErrTemplateConflict
				}
				if _, err := tx.ExecContext(ctx, "INSERT INTO CONFIG_FILTER_RULES (ID,GROUP_ID,ROLE_NAME,PRIORITY,INCLUDE,EXCLUDE,SIZE_LIMIT,NOTE) VALUES (?,?,?,?,?,?,?,?)", rule.ID, strconv.FormatInt(rule.GroupID, 10), rule.Name, rule.Priority, rule.Include, rule.Exclude, rule.Size, rule.Free); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
