package rsstaskconfig

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("rss task not found")

type Task struct {
	ID, OverEdition, DownloadSetting                                int64
	Name, Address, Parser, Interval, Uses, Include, Exclude, Filter string
	UpdateTime, ProcessCount, State, SavePath, Recognization        string
	Sites, FilterArgs, MediaInfos, Note, FilterName                 string
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
		return nil, fmt.Errorf("open rss task configuration: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS CONFIG_USER_RSS (
		ID INTEGER PRIMARY KEY, NAME TEXT, ADDRESS TEXT, PARSER TEXT, INTERVAL TEXT, USES TEXT,
		INCLUDE TEXT, EXCLUDE TEXT, FILTER TEXT, UPDATE_TIME TEXT, PROCESS_COUNT TEXT,
		STATE TEXT, SAVE_PATH TEXT, DOWNLOAD_SETTING INTEGER, RECOGNIZATION TEXT,
		OVER_EDITION INTEGER, SITES TEXT, FILTER_ARGS TEXT, MEDIAINFOS TEXT, NOTE TEXT
	)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ensure rss task table: %w", err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS GO_RSS_DOWNLOAD_CLAIMS (RESOURCE_KEY TEXT PRIMARY KEY, TASK_ID INTEGER NOT NULL, STATE TEXT NOT NULL, UPDATED_AT TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS USERRSS_TASK_HISTORY (ID INTEGER PRIMARY KEY, TASK_ID TEXT, TITLE TEXT, DOWNLOADER TEXT, DATE TEXT)`,
		`CREATE TABLE IF NOT EXISTS RSS_TORRENTS (ID INTEGER PRIMARY KEY, TORRENT_NAME TEXT, ENCLOSURE TEXT, TYPE TEXT, TITLE TEXT, YEAR TEXT, SEASON TEXT, EPISODE TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("ensure rss history table: %w", err)
		}
	}
	return &Store{database: db}, nil
}

func (store *Store) Close() error { return store.database.Close() }

const selectTasks = `SELECT ID, COALESCE(NAME,''), COALESCE(ADDRESS,''), COALESCE(PARSER,''),
	COALESCE(INTERVAL,''), COALESCE(USES,''), COALESCE(INCLUDE,''), COALESCE(EXCLUDE,''),
	COALESCE(FILTER,''), COALESCE(UPDATE_TIME,''), COALESCE(PROCESS_COUNT,''),
	COALESCE(STATE,''), COALESCE(SAVE_PATH,''), COALESCE(DOWNLOAD_SETTING,0),
	COALESCE(RECOGNIZATION,''), COALESCE(OVER_EDITION,0), COALESCE(SITES,''),
	COALESCE(FILTER_ARGS,''), COALESCE(MEDIAINFOS,''), COALESCE(NOTE,'') FROM CONFIG_USER_RSS`

func scanTask(rows interface{ Scan(...any) error }) (Task, error) {
	var task Task
	err := rows.Scan(&task.ID, &task.Name, &task.Address, &task.Parser, &task.Interval,
		&task.Uses, &task.Include, &task.Exclude, &task.Filter, &task.UpdateTime,
		&task.ProcessCount, &task.State, &task.SavePath, &task.DownloadSetting,
		&task.Recognization, &task.OverEdition, &task.Sites, &task.FilterArgs,
		&task.MediaInfos, &task.Note)
	return task, err
}

func (store *Store) List(ctx context.Context) ([]Task, error) {
	rows, err := store.database.QueryContext(ctx, selectTasks+` ORDER BY STATE DESC, ID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Task, 0)
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, task)
	}
	return result, rows.Err()
}

func (store *Store) Get(ctx context.Context, id int64) (Task, error) {
	task, err := scanTask(store.database.QueryRowContext(ctx, selectTasks+` WHERE ID=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return task, err
}

func (store *Store) Upsert(ctx context.Context, task Task) (int64, error) {
	if task.ID > 0 {
		_, err := store.Get(ctx, task.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
		if err == nil {
			_, err = store.database.ExecContext(ctx, `UPDATE CONFIG_USER_RSS SET
				NAME=?,ADDRESS=?,PARSER=?,INTERVAL=?,USES=?,INCLUDE=?,EXCLUDE=?,FILTER=?,UPDATE_TIME=?,
				STATE=?,SAVE_PATH=?,DOWNLOAD_SETTING=?,RECOGNIZATION=?,OVER_EDITION=?,SITES=?,FILTER_ARGS=?,NOTE=? WHERE ID=?`,
				task.Name, task.Address, task.Parser, task.Interval, task.Uses, task.Include, task.Exclude,
				task.Filter, task.UpdateTime, task.State, task.SavePath, task.DownloadSetting,
				task.Recognization, task.OverEdition, task.Sites, task.FilterArgs, task.Note, task.ID)
			return task.ID, err
		}
	}
	result, err := store.database.ExecContext(ctx, `INSERT INTO CONFIG_USER_RSS
		(NAME,ADDRESS,PARSER,INTERVAL,USES,INCLUDE,EXCLUDE,FILTER,UPDATE_TIME,PROCESS_COUNT,
		 STATE,SAVE_PATH,DOWNLOAD_SETTING,RECOGNIZATION,OVER_EDITION,SITES,FILTER_ARGS,NOTE)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.Name, task.Address, task.Parser, task.Interval, task.Uses, task.Include, task.Exclude,
		task.Filter, task.UpdateTime, "0", task.State, task.SavePath, task.DownloadSetting,
		task.Recognization, task.OverEdition, task.Sites, task.FilterArgs, task.Note)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (store *Store) Delete(ctx context.Context, id int64) error {
	result, err := store.database.ExecContext(ctx, `DELETE FROM CONFIG_USER_RSS WHERE ID=?`, id)
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

type HistoryEntry struct {
	Title      string `json:"title"`
	Downloader string `json:"downloader"`
	Date       string `json:"date"`
}

func (store *Store) History(ctx context.Context, taskID int64) ([]HistoryEntry, error) {
	rows, err := store.database.QueryContext(ctx, `SELECT COALESCE(TITLE,''), COALESCE(DOWNLOADER,''), COALESCE(DATE,'') FROM USERRSS_TASK_HISTORY WHERE TASK_ID=? ORDER BY DATE DESC, ID DESC`, strconv.FormatInt(taskID, 10))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]HistoryEntry, 0)
	for rows.Next() {
		var item HistoryEntry
		if err := rows.Scan(&item.Title, &item.Downloader, &item.Date); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// IsProcessed uses the same shared enclosure key as the legacy RSS checker.
func (store *Store) IsProcessed(ctx context.Context, uses, title, year, enclosure string) (bool, error) {
	if uses != "D" && uses != "R" {
		return false, nil
	}
	name := title
	if year != "" {
		name += " " + year
	}
	if uses == "R" {
		enclosure = name
	}
	var exists int
	var err error
	if enclosure != "" {
		err = store.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, enclosure).Scan(&exists)
	} else {
		err = store.database.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE TORRENT_NAME=?)`, name).Scan(&exists)
	}
	return exists != 0, err
}

// RecordDownload persists a successful manual RSS submission in both legacy
// tables atomically. The external downloader operation must succeed first.
func (store *Store) RecordDownload(ctx context.Context, taskID int64, title, year, enclosure, downloaderName string) error {
	if taskID <= 0 || strings.TrimSpace(title) == "" || strings.TrimSpace(downloaderName) == "" {
		return errors.New("invalid RSS download record")
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	if err := recordDownloadTx(ctx, transaction, taskID, title, year, enclosure, downloaderName); err != nil {
		return err
	}
	return transaction.Commit()
}

func recordDownloadTx(ctx context.Context, transaction *sql.Tx, taskID int64, title, year, enclosure, downloaderName string) error {
	var exists int
	var err error
	if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM CONFIG_USER_RSS WHERE ID=?)`, taskID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	if enclosure != "" {
		err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, enclosure).Scan(&exists)
	} else {
		err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE TORRENT_NAME=?)`, title).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if exists == 0 {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO RSS_TORRENTS (TORRENT_NAME,ENCLOSURE,TITLE,YEAR) VALUES (?,?,?,?)`, title, enclosure, title, year); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO USERRSS_TASK_HISTORY (TASK_ID,TITLE,DOWNLOADER,DATE) VALUES (?,?,?,?)`, strconv.FormatInt(taskID, 10), title, downloaderName, time.Now().Format("2006-01-02 15:04:05")); err != nil {
		return err
	}
	return nil
}

type Article struct {
	Title     string `json:"title"`
	Enclosure string `json:"enclosure"`
	Year      string `json:"year"`
}

func (store *Store) SetArticles(ctx context.Context, taskID int64, flag string, articles []Article) error {
	task, err := store.Get(ctx, taskID)
	if err != nil {
		return err
	}
	if flag != "set_finished" && flag != "set_unfinish" {
		return errors.New("invalid article action")
	}
	if task.Uses != "D" && task.Uses != "R" && task.Uses != "S" {
		return errors.New("unsupported rss task type")
	}
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	for _, article := range articles {
		name := article.Title
		if article.Year != "" {
			name += " " + article.Year
		}
		enclosure := article.Enclosure
		if task.Uses != "D" {
			enclosure = name
		}
		if flag == "set_finished" {
			var exists int
			if enclosure != "" {
				err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?)`, enclosure).Scan(&exists)
			} else {
				err = transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE TORRENT_NAME=?)`, name).Scan(&exists)
			}
			if err != nil {
				return err
			}
			if exists == 0 {
				_, err = transaction.ExecContext(ctx, `INSERT INTO RSS_TORRENTS (TORRENT_NAME,ENCLOSURE) VALUES (?,?)`, name, enclosure)
				if err != nil {
					return err
				}
			}
			continue
		}
		if task.Uses == "D" && strings.TrimSpace(enclosure) == "" {
			_, err = transaction.ExecContext(ctx, `DELETE FROM RSS_TORRENTS WHERE TORRENT_NAME=?`, name)
		} else if task.Uses == "D" {
			// Download history uses the enclosure as its global identity, even
			// when the feed's display title/year has changed since submission.
			_, err = transaction.ExecContext(ctx, `DELETE FROM RSS_TORRENTS WHERE ENCLOSURE=?`, enclosure)
			if err == nil {
				_, err = transaction.ExecContext(ctx, `DELETE FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=? AND STATE='submitted'`, claimKey(enclosure))
			}
		} else {
			_, err = transaction.ExecContext(ctx, `DELETE FROM RSS_TORRENTS WHERE TORRENT_NAME=? AND ENCLOSURE=?`, name, enclosure)
		}
		if err != nil {
			return err
		}
	}
	return transaction.Commit()
}
