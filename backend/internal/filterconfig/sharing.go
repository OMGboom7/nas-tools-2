package filterconfig

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

var ErrInvalidShare = errors.New("invalid shared filter configuration")
var ErrEmptyGroup = errors.New("filter group has no rules")

const maxShareBytes = 1 << 20

// Older exports contain nullable text columns, and sometimes numeric priorities.
type sharedText string

func (value *sharedText) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*value = ""
		return nil
	}
	var text string
	if json.Unmarshal(data, &text) == nil {
		*value = sharedText(text)
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return ErrInvalidShare
	}
	if _, err := strconv.ParseFloat(number.String(), 64); err != nil {
		return ErrInvalidShare
	}
	*value = sharedText(number.String())
	return nil
}

type sharedRule struct {
	Name     sharedText `json:"name"`
	Priority sharedText `json:"pri"`
	Include  sharedText `json:"include"`
	Exclude  sharedText `json:"exclude"`
	Size     sharedText `json:"size"`
	Free     sharedText `json:"free"`
}

type sharedGroup struct {
	Name  string       `json:"name"`
	Rules []sharedRule `json:"rules"`
}

func (store *Store) Export(ctx context.Context, id int64) (string, error) {
	group := sharedGroup{Rules: []sharedRule{}}
	err := store.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT COALESCE(GROUP_NAME,'') FROM CONFIG_FILTER_GROUP WHERE ID=?", id).Scan(&group.Name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT COALESCE(ROLE_NAME,''), COALESCE(PRIORITY,'0'), COALESCE(INCLUDE,''), COALESCE(EXCLUDE,''), COALESCE(SIZE_LIMIT,''), COALESCE(NOTE,'') FROM CONFIG_FILTER_RULES WHERE GROUP_ID=? ORDER BY ID", strconv.FormatInt(id, 10))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rule sharedRule
			if err := rows.Scan(&rule.Name, &rule.Priority, &rule.Include, &rule.Exclude, &rule.Size, &rule.Free); err != nil {
				return err
			}
			group.Rules = append(group.Rules, rule)
		}
		return rows.Err()
	})
	if err != nil {
		return "", err
	}
	if len(group.Rules) == 0 {
		return "", ErrEmptyGroup
	}
	content, err := json.Marshal(group)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(content), nil
}

func (store *Store) Import(ctx context.Context, encoded string) (int64, error) {
	encoded = strings.TrimSpace(encoded)
	if len(encoded) > base64.StdEncoding.EncodedLen(maxShareBytes) {
		return 0, ErrInvalidShare
	}
	content, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(content) > maxShareBytes {
		return 0, ErrInvalidShare
	}
	var group sharedGroup
	if json.Unmarshal(content, &group) != nil {
		return 0, ErrInvalidShare
	}
	group.Name = strings.TrimSpace(group.Name)
	if group.Name == "" || len(group.Name) > 256 || len(group.Rules) > 1000 {
		return 0, ErrInvalidShare
	}
	// Validate the whole payload before changing any persisted data.
	for index := range group.Rules {
		rule := &group.Rules[index]
		if strings.TrimSpace(string(rule.Name)) == "" || len(rule.Name) > 256 {
			return 0, ErrInvalidShare
		}
		if rule.Priority == "" {
			rule.Priority = "0"
		}
		if _, err := strconv.Atoi(string(rule.Priority)); err != nil {
			return 0, ErrInvalidShare
		}
	}
	var id int64
	err = store.transaction(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT ID FROM CONFIG_FILTER_GROUP WHERE GROUP_NAME=? ORDER BY ID LIMIT 1", group.Name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			result, err := tx.ExecContext(ctx, "INSERT INTO CONFIG_FILTER_GROUP (GROUP_NAME, IS_DEFAULT) VALUES (?, 'N')", group.Name)
			if err != nil {
				return err
			}
			id, err = result.LastInsertId()
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			// Match legacy import: append to a same-name group and clear its default flag.
			if _, err := tx.ExecContext(ctx, "UPDATE CONFIG_FILTER_GROUP SET IS_DEFAULT='N' WHERE ID=?", id); err != nil {
				return err
			}
		}
		for _, rule := range group.Rules {
			if _, err := tx.ExecContext(ctx, "INSERT INTO CONFIG_FILTER_RULES (GROUP_ID, ROLE_NAME, PRIORITY, INCLUDE, EXCLUDE, SIZE_LIMIT, NOTE) VALUES (?, ?, ?, ?, ?, ?, ?)", strconv.FormatInt(id, 10), string(rule.Name), string(rule.Priority), string(rule.Include), string(rule.Exclude), string(rule.Size), string(rule.Free)); err != nil {
				return err
			}
		}
		return nil
	})
	return id, err
}
