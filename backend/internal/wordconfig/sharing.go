package wordconfig

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

var ErrInvalidShare = errors.New("invalid custom-word share")

type Selection struct{ GroupID, WordID int64 }

type SharedWord struct {
	ID       int64  `json:"id"`
	Replaced string `json:"replaced"`
	Replace  string `json:"replace"`
	Front    string `json:"front"`
	Back     string `json:"back"`
	Offset   string `json:"offset"`
	Type     int    `json:"type"`
	Season   int    `json:"season"`
	Regex    int    `json:"regex"`
	Help     string `json:"help"`
}

type SharedGroup struct {
	ID      int64                 `json:"id"`
	Title   string                `json:"title"`
	Year    string                `json:"year,omitempty"`
	Type    int                   `json:"type"`
	TMDBID  int64                 `json:"tmdbid,omitempty"`
	Seasons int                   `json:"season_count,omitempty"`
	Words   map[string]SharedWord `json:"words"`
}

func (store *Store) Export(ctx context.Context, selection []Selection, note string) (string, error) {
	if len(note) > 16<<10 || len(selection) > 1000 {
		return "", ErrInvalidShare
	}
	groups, words, err := store.List(ctx)
	if err != nil {
		return "", err
	}
	available := map[int64]SharedGroup{-1: {ID: -1, Title: "通用", Type: 1, Words: map[string]SharedWord{}}}
	for _, group := range groups {
		available[group.ID] = SharedGroup{ID: group.ID, Title: group.Title, Year: group.Year, Type: group.Type, TMDBID: group.TMDBID, Seasons: group.Seasons, Words: map[string]SharedWord{}}
	}
	wanted := map[Selection]bool{}
	for _, item := range selection {
		wanted[item] = true
	}
	shared := map[string]SharedGroup{}
	if len(selection) == 0 {
		for id, group := range available {
			shared[strconv.FormatInt(id, 10)] = group
		}
	}
	for _, word := range words {
		key := Selection{word.GroupID, word.ID}
		if len(selection) != 0 && !wanted[key] {
			continue
		}
		group, exists := available[word.GroupID]
		if !exists {
			return "", ErrInvalidShare
		}
		group.Words[strconv.FormatInt(word.ID, 10)] = SharedWord{word.ID, word.Replaced, word.Replace, word.Front, word.Back, word.Offset, word.Type, word.Season, word.Regex, word.Help}
		shared[strconv.FormatInt(word.GroupID, 10)] = group
		delete(wanted, key)
	}
	if len(wanted) != 0 {
		return "", ErrNotFound
	}
	contents, err := json.Marshal(shared)
	if err != nil || len(contents)+len(note) > 1<<20 {
		return "", ErrInvalidShare
	}
	return base64.StdEncoding.EncodeToString(append(contents, []byte("@@@@@@"+note)...)), nil
}

func ParseShare(code string) (map[string]SharedGroup, string, error) {
	if len(code) > 1400<<10 {
		return nil, "", ErrInvalidShare
	}
	contents, err := base64.StdEncoding.DecodeString(strings.TrimSpace(code))
	if err != nil || len(contents) > 1<<20 {
		return nil, "", ErrInvalidShare
	}
	parts := strings.SplitN(string(contents), "@@@@@@", 2)
	if len(parts) != 2 || len(parts[1]) > 16<<10 {
		return nil, "", ErrInvalidShare
	}
	var groups map[string]SharedGroup
	if json.Unmarshal([]byte(parts[0]), &groups) != nil || groups == nil || len(groups) > 1000 {
		return nil, "", ErrInvalidShare
	}
	count := 0
	for key, group := range groups {
		if strconv.FormatInt(group.ID, 10) != key || group.ID != -1 && group.ID <= 0 || group.Type < 1 || group.Type > 2 || group.Title == "" || len(group.Title) > 1000 || len(group.Year) > 16 || group.Seasons < 0 || group.Seasons > 10000 || group.ID != -1 && group.TMDBID <= 0 {
			return nil, "", ErrInvalidShare
		}
		for wordKey, word := range group.Words {
			count++
			if count > 1000 || word.ID <= 0 || strconv.FormatInt(word.ID, 10) != wordKey || !validSharedWord(word) {
				return nil, "", ErrInvalidShare
			}
		}
	}
	return groups, parts[1], nil
}

func validSharedWord(word SharedWord) bool {
	if word.Type < 1 || word.Type > 4 || word.Regex < 0 || word.Regex > 1 || word.Season < -2 || word.Season > 10000 || word.Type != 4 && word.Replaced == "" {
		return false
	}
	for _, value := range []string{word.Replaced, word.Replace, word.Front, word.Back, word.Offset, word.Help} {
		if len(value) > 16<<10 || strings.ContainsRune(value, 0) {
			return false
		}
	}
	if word.Type == 3 || word.Type == 4 {
		return strings.Contains(word.Offset, "EP") && strings.Trim(strings.ReplaceAll(word.Offset, "EP", ""), "+-*/0123456789") == ""
	}
	return true
}

func (store *Store) Import(ctx context.Context, groups map[string]SharedGroup, selection []Selection) error {
	if len(selection) > 1000 {
		return ErrInvalidShare
	}
	for _, item := range selection {
		group, exists := groups[strconv.FormatInt(item.GroupID, 10)]
		word, found := group.Words[strconv.FormatInt(item.WordID, 10)]
		if !exists || !found || !validSharedWord(word) {
			return ErrInvalidShare
		}
	}
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	groupIDs := map[int64]int64{-1: -1}
	seen := map[Selection]bool{}
	for _, item := range selection {
		if seen[item] {
			continue
		}
		seen[item] = true
		group := groups[strconv.FormatInt(item.GroupID, 10)]
		word := group.Words[strconv.FormatInt(item.WordID, 10)]
		id, exists := groupIDs[group.ID]
		if !exists {
			err := tx.QueryRowContext(ctx, "SELECT ID FROM CUSTOM_WORD_GROUPS WHERE TMDBID=? AND TYPE=? ORDER BY ID LIMIT 1", group.TMDBID, group.Type).Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				result, insertErr := tx.ExecContext(ctx, "INSERT INTO CUSTOM_WORD_GROUPS (TITLE,YEAR,TYPE,TMDBID,SEASON_COUNT) VALUES (?,?,?,?,?)", group.Title, group.Year, group.Type, group.TMDBID, group.Seasons)
				if insertErr != nil {
					return insertErr
				}
				id, err = result.LastInsertId()
			}
			if err != nil {
				return err
			}
			groupIDs[group.ID] = id
		}
		var count int
		if word.Type != 4 && word.Replaced != "" {
			err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM CUSTOM_WORDS WHERE REPLACED=?", word.Replaced).Scan(&count)
		} else if word.Type == 4 && word.Front != "" && word.Back != "" {
			err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM CUSTOM_WORDS WHERE FRONT=? AND BACK=?", word.Front, word.Back).Scan(&count)
		}
		if err != nil {
			return err
		}
		if count != 0 {
			return ErrDuplicate
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO CUSTOM_WORDS (REPLACED,"REPLACE",FRONT,BACK,"OFFSET",TYPE,GROUP_ID,SEASON,ENABLED,REGEX,HELP) VALUES (?,?,?,?,?,?,?,?,1,?,?)`, word.Replaced, word.Replace, word.Front, word.Back, word.Offset, word.Type, id, word.Season, word.Regex, word.Help); err != nil {
			return err
		}
	}
	return tx.Commit()
}
