package wordconfig

import (
	"context"
	"database/sql"
	"errors"
	"net/url"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("custom word or group not found")
var ErrDuplicate = errors.New("custom word already exists")

type Word struct {
	ID       int64  `json:"id"`
	Replaced string `json:"replaced"`
	Replace  string `json:"replace"`
	Front    string `json:"front"`
	Back     string `json:"back"`
	Offset   string `json:"offset"`
	Type     int    `json:"type"`
	GroupID  int64  `json:"group_id"`
	Season   int    `json:"season"`
	Enabled  int    `json:"enabled"`
	Regex    int    `json:"regex"`
	Help     string `json:"help"`
}

type Group struct {
	ID          int64
	Title, Year string
	Type        int
	TMDBID      int64
	Seasons     int
}

type Store struct{ database *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw"}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		`CREATE TABLE IF NOT EXISTS CUSTOM_WORDS (ID INTEGER PRIMARY KEY, REPLACED TEXT, "REPLACE" TEXT, FRONT TEXT, BACK TEXT, "OFFSET" TEXT, TYPE INTEGER, GROUP_ID INTEGER, SEASON INTEGER, ENABLED INTEGER, REGEX INTEGER, HELP TEXT, NOTE TEXT)`,
		`CREATE TABLE IF NOT EXISTS CUSTOM_WORD_GROUPS (ID INTEGER PRIMARY KEY, TITLE TEXT, YEAR TEXT, TYPE INTEGER, TMDBID INTEGER, SEASON_COUNT INTEGER, NOTE TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &Store{database: db}, nil
}

func (store *Store) Close() error { return store.database.Close() }

const wordColumns = `ID, COALESCE(REPLACED,''), COALESCE("REPLACE",''), COALESCE(FRONT,''), COALESCE(BACK,''), COALESCE("OFFSET",''), COALESCE(TYPE,0), COALESCE(GROUP_ID,-1), COALESCE(SEASON,-1), COALESCE(ENABLED,0), COALESCE(REGEX,0), COALESCE(HELP,'')`

func scanWord(row interface{ Scan(...any) error }) (Word, error) {
	var word Word
	err := row.Scan(&word.ID, &word.Replaced, &word.Replace, &word.Front, &word.Back, &word.Offset, &word.Type, &word.GroupID, &word.Season, &word.Enabled, &word.Regex, &word.Help)
	return word, err
}

func (store *Store) Get(ctx context.Context, id int64) (Word, error) {
	word, err := scanWord(store.database.QueryRowContext(ctx, "SELECT "+wordColumns+" FROM CUSTOM_WORDS WHERE ID=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return word, err
}

// List reads both tables within one snapshot and uses the original display order.
func (store *Store) List(ctx context.Context) ([]Group, []Word, error) {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	groups, words := []Group{}, []Word{}
	rows, err := tx.QueryContext(ctx, `SELECT ID, COALESCE(TITLE,''), COALESCE(YEAR,''), COALESCE(TYPE,0), COALESCE(TMDBID,0), COALESCE(SEASON_COUNT,0) FROM CUSTOM_WORD_GROUPS ORDER BY ID`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Title, &group.Year, &group.Type, &group.TMDBID, &group.Seasons); err != nil {
			rows.Close()
			return nil, nil, err
		}
		groups = append(groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT "+wordColumns+" FROM CUSTOM_WORDS ORDER BY GROUP_ID, ENABLED DESC, TYPE, REGEX, ID")
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		word, err := scanWord(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		words = append(words, word)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	return groups, words, tx.Commit()
}

func (store *Store) Save(ctx context.Context, word Word) (int64, error) {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists int
	if word.GroupID != -1 {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM CUSTOM_WORD_GROUPS WHERE ID=?", word.GroupID).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			return 0, ErrNotFound
		}
	}
	if word.Replaced != "" {
		err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM CUSTOM_WORDS WHERE REPLACED=? AND ID<>?", word.Replaced, word.ID).Scan(&exists)
	} else if word.Type == 4 && word.Front != "" && word.Back != "" {
		err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM CUSTOM_WORDS WHERE FRONT=? AND BACK=? AND ID<>?", word.Front, word.Back, word.ID).Scan(&exists)
	}
	if err != nil {
		return 0, err
	}
	if exists > 0 && (word.Replaced != "" || word.Type == 4 && word.Front != "" && word.Back != "") {
		return 0, ErrDuplicate
	}
	args := []any{word.Replaced, word.Replace, word.Front, word.Back, word.Offset, word.Type, word.GroupID, word.Season, word.Enabled, word.Regex, word.Help}
	if word.ID == 0 {
		result, err := tx.ExecContext(ctx, `INSERT INTO CUSTOM_WORDS (REPLACED,"REPLACE",FRONT,BACK,"OFFSET",TYPE,GROUP_ID,SEASON,ENABLED,REGEX,HELP) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, args...)
		if err != nil {
			return 0, err
		}
		word.ID, err = result.LastInsertId()
		if err != nil {
			return 0, err
		}
	} else {
		args = append(args, word.ID)
		result, err := tx.ExecContext(ctx, `UPDATE CUSTOM_WORDS SET REPLACED=?,"REPLACE"=?,FRONT=?,BACK=?,"OFFSET"=?,TYPE=?,GROUP_ID=?,SEASON=?,ENABLED=?,REGEX=?,HELP=? WHERE ID=?`, args...)
		if err != nil {
			return 0, err
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			return 0, ErrNotFound
		}
	}
	return word.ID, tx.Commit()
}

func (store *Store) Delete(ctx context.Context, id int64) error {
	_, err := store.database.ExecContext(ctx, "DELETE FROM CUSTOM_WORDS WHERE ID=?", id)
	return err
}

func (store *Store) SetStatus(ctx context.Context, ids []int64, enabled int) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if len(ids) == 0 {
		_, err = tx.ExecContext(ctx, "UPDATE CUSTOM_WORDS SET ENABLED=?", enabled)
	} else {
		for _, id := range ids {
			if _, err = tx.ExecContext(ctx, "UPDATE CUSTOM_WORDS SET ENABLED=? WHERE ID=?", enabled, id); err != nil {
				return err
			}
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) DeleteGroup(ctx context.Context, id int64) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM CUSTOM_WORDS WHERE GROUP_ID=?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM CUSTOM_WORD_GROUPS WHERE ID=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) HasGroup(ctx context.Context, tmdbID int64, kind int) (bool, error) {
	var count int
	err := store.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM CUSTOM_WORD_GROUPS WHERE TMDBID=? AND TYPE=?", tmdbID, kind).Scan(&count)
	return count > 0, err
}

func (store *Store) AddGroup(ctx context.Context, group Group) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM CUSTOM_WORD_GROUPS WHERE TMDBID=? AND TYPE=?", group.TMDBID, group.Type).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrDuplicate
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO CUSTOM_WORD_GROUPS (TITLE,YEAR,TYPE,TMDBID,SEASON_COUNT) VALUES (?,?,?,?,?)", group.Title, group.Year, group.Type, group.TMDBID, group.Seasons); err != nil {
		return err
	}
	return tx.Commit()
}
