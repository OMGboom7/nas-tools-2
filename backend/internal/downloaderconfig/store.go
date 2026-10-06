package downloaderconfig

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

var ErrNotFound = errors.New("downloader not found")

var ErrSettingNotFound = errors.New("download setting not found")

type Downloader struct {
	ID          int64
	Name        string
	Enabled     int
	Type        string
	Transfer    int
	OnlyNastool int
	MatchPath   int
	RmtMode     string
	Config      string
	DownloadDir string
}

type DownloadSetting struct {
	ID               int64
	Name             string
	Category         string
	Tags             string
	Paused           int
	UploadLimit      int
	DownloadLimit    int
	RatioLimit       int
	SeedingTimeLimit int
	DownloaderID     string
}

type TaskHistory struct{ Title, Year, SeasonEpisode, Poster string }

type DownloadHistory struct {
	TMDBID, Title, MediaType, Year, Poster, Torrent, Date, Site string
}

func (store *Store) ListDownloadHistory(ctx context.Context, page, pageSize int) ([]DownloadHistory, error) {
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, errors.New("invalid download history page")
	}
	var exists int
	if err := store.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='DOWNLOAD_HISTORY'").Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return []DownloadHistory{}, nil
	}
	rows, err := store.database.QueryContext(ctx, `
		WITH ranked AS (
			SELECT COALESCE(TMDBID,'') AS TMDBID, COALESCE(TITLE,'') AS TITLE, COALESCE(TYPE,'') AS TYPE, COALESCE(YEAR,'') AS YEAR,
			       COALESCE(POSTER,'') AS POSTER, COALESCE(TORRENT,'') AS TORRENT, COALESCE(DATE,'') AS DATE, COALESCE(SITE,'') AS SITE,
			       ROW_NUMBER() OVER (PARTITION BY TITLE ORDER BY DATE DESC, ID DESC) AS position
			FROM DOWNLOAD_HISTORY
		)
		SELECT * FROM (
			SELECT TMDBID, TITLE, TYPE, YEAR, POSTER, TORRENT, DATE, SITE
			FROM ranked WHERE position = 1
		) ORDER BY DATE DESC LIMIT ? OFFSET ?`, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, fmt.Errorf("list download history: %w", err)
	}
	defer rows.Close()
	items := make([]DownloadHistory, 0)
	for rows.Next() {
		var item DownloadHistory
		if err := rows.Scan(&item.TMDBID, &item.Title, &item.MediaType, &item.Year, &item.Poster, &item.Torrent, &item.Date, &item.Site); err != nil {
			return nil, fmt.Errorf("scan download history: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate download history: %w", err)
	}
	return items, nil
}

func (store *Store) TaskHistory(ctx context.Context, downloaderID, taskID string) (TaskHistory, error) {
	var exists int
	if err := store.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='DOWNLOAD_HISTORY'").Scan(&exists); err != nil {
		return TaskHistory{}, err
	}
	if exists == 0 {
		return TaskHistory{}, nil
	}
	var item TaskHistory
	err := store.database.QueryRowContext(ctx, "SELECT COALESCE(TITLE,''),COALESCE(YEAR,''),COALESCE(SE,''),COALESCE(POSTER,'') FROM DOWNLOAD_HISTORY WHERE DOWNLOADER=? AND DOWNLOAD_ID=? ORDER BY DATE DESC,ID DESC LIMIT 1", downloaderID, taskID).Scan(&item.Title, &item.Year, &item.SeasonEpisode, &item.Poster)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskHistory{}, nil
	}
	if err != nil {
		return TaskHistory{}, fmt.Errorf("read task download history: %w", err)
	}
	return item, nil
}

type Store struct {
	database *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create downloader database directory: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=rwc"}).String()
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open downloader database: %w", err)
	}
	database.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA busy_timeout = 5000",
		"CREATE TABLE IF NOT EXISTS DOWNLOADER (ID INTEGER PRIMARY KEY AUTOINCREMENT, NAME TEXT, ENABLED INTEGER, TYPE TEXT, TRANSFER INTEGER, ONLY_NASTOOL INTEGER, MATCH_PATH INTEGER, RMT_MODE TEXT, CONFIG TEXT, DOWNLOAD_DIR TEXT)",
		"CREATE TABLE IF NOT EXISTS DOWNLOAD_SETTING (ID INTEGER PRIMARY KEY AUTOINCREMENT, NAME TEXT, CATEGORY TEXT, TAGS TEXT, IS_PAUSED INTEGER, UPLOAD_LIMIT INTEGER, DOWNLOAD_LIMIT INTEGER, RATIO_LIMIT INTEGER, SEEDING_TIME_LIMIT INTEGER, DOWNLOADER TEXT, NOTE TEXT)",
	} {
		if _, err := database.Exec(statement); err != nil {
			_ = database.Close()
			return nil, fmt.Errorf("initialize downloader database: %w", err)
		}
	}
	return &Store{database: database}, nil
}

func (store *Store) ListSettings(ctx context.Context) ([]DownloadSetting, error) {
	rows, err := store.database.QueryContext(ctx, "SELECT ID, COALESCE(NAME,''), COALESCE(CATEGORY,''), COALESCE(TAGS,''), COALESCE(IS_PAUSED,0), COALESCE(UPLOAD_LIMIT,0), COALESCE(DOWNLOAD_LIMIT,0), COALESCE(RATIO_LIMIT,0), COALESCE(SEEDING_TIME_LIMIT,0), COALESCE(DOWNLOADER,'') FROM DOWNLOAD_SETTING ORDER BY ID")
	if err != nil {
		return nil, fmt.Errorf("list download settings: %w", err)
	}
	defer rows.Close()
	items := make([]DownloadSetting, 0)
	for rows.Next() {
		item, err := scanDownloadSetting(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list download settings: %w", err)
	}
	return items, nil
}

func (store *Store) GetSetting(ctx context.Context, id int64) (DownloadSetting, error) {
	row := store.database.QueryRowContext(ctx, "SELECT ID, COALESCE(NAME,''), COALESCE(CATEGORY,''), COALESCE(TAGS,''), COALESCE(IS_PAUSED,0), COALESCE(UPLOAD_LIMIT,0), COALESCE(DOWNLOAD_LIMIT,0), COALESCE(RATIO_LIMIT,0), COALESCE(SEEDING_TIME_LIMIT,0), COALESCE(DOWNLOADER,'') FROM DOWNLOAD_SETTING WHERE ID = ?", id)
	item, err := scanDownloadSetting(row)
	if errors.Is(err, sql.ErrNoRows) {
		return DownloadSetting{}, ErrSettingNotFound
	}
	return item, err
}

func (store *Store) UpsertSetting(ctx context.Context, item DownloadSetting) (DownloadSetting, error) {
	if item.ID == 0 {
		result, err := store.database.ExecContext(ctx, "INSERT INTO DOWNLOAD_SETTING (NAME, CATEGORY, TAGS, IS_PAUSED, UPLOAD_LIMIT, DOWNLOAD_LIMIT, RATIO_LIMIT, SEEDING_TIME_LIMIT, DOWNLOADER, NOTE) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '')", item.Name, item.Category, item.Tags, item.Paused, item.UploadLimit, item.DownloadLimit, item.RatioLimit, item.SeedingTimeLimit, item.DownloaderID)
		if err != nil {
			return DownloadSetting{}, fmt.Errorf("insert download setting: %w", err)
		}
		item.ID, err = result.LastInsertId()
		if err != nil {
			return DownloadSetting{}, fmt.Errorf("read inserted download setting ID: %w", err)
		}
		return item, nil
	}
	result, err := store.database.ExecContext(ctx, "UPDATE DOWNLOAD_SETTING SET NAME=?, CATEGORY=?, TAGS=?, IS_PAUSED=?, UPLOAD_LIMIT=?, DOWNLOAD_LIMIT=?, RATIO_LIMIT=?, SEEDING_TIME_LIMIT=?, DOWNLOADER=? WHERE ID=?", item.Name, item.Category, item.Tags, item.Paused, item.UploadLimit, item.DownloadLimit, item.RatioLimit, item.SeedingTimeLimit, item.DownloaderID, item.ID)
	if err != nil {
		return DownloadSetting{}, fmt.Errorf("update download setting: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return DownloadSetting{}, fmt.Errorf("read updated download setting count: %w", err)
	}
	if count == 0 {
		return DownloadSetting{}, ErrSettingNotFound
	}
	return item, nil
}

func (store *Store) DeleteSetting(ctx context.Context, id int64) error {
	result, err := store.database.ExecContext(ctx, "DELETE FROM DOWNLOAD_SETTING WHERE ID = ?", id)
	if err != nil {
		return fmt.Errorf("delete download setting: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted download setting count: %w", err)
	}
	if count == 0 {
		return ErrSettingNotFound
	}
	return nil
}

func (store *Store) List(ctx context.Context) ([]Downloader, error) {
	rows, err := store.database.QueryContext(ctx, "SELECT ID, COALESCE(NAME,''), COALESCE(ENABLED,0), COALESCE(TYPE,''), COALESCE(TRANSFER,0), COALESCE(ONLY_NASTOOL,0), COALESCE(MATCH_PATH,0), COALESCE(RMT_MODE,''), COALESCE(CONFIG,'{}'), COALESCE(DOWNLOAD_DIR,'[]') FROM DOWNLOADER ORDER BY ID")
	if err != nil {
		return nil, fmt.Errorf("list downloaders: %w", err)
	}
	defer rows.Close()
	items := make([]Downloader, 0)
	for rows.Next() {
		item, err := scanDownloader(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list downloaders: %w", err)
	}
	return items, nil
}

func (store *Store) Get(ctx context.Context, id int64) (Downloader, error) {
	row := store.database.QueryRowContext(ctx, "SELECT ID, COALESCE(NAME,''), COALESCE(ENABLED,0), COALESCE(TYPE,''), COALESCE(TRANSFER,0), COALESCE(ONLY_NASTOOL,0), COALESCE(MATCH_PATH,0), COALESCE(RMT_MODE,''), COALESCE(CONFIG,'{}'), COALESCE(DOWNLOAD_DIR,'[]') FROM DOWNLOADER WHERE ID = ?", id)
	item, err := scanDownloader(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Downloader{}, ErrNotFound
	}
	return item, err
}

func (store *Store) Upsert(ctx context.Context, item Downloader) (Downloader, error) {
	if item.ID == 0 {
		result, err := store.database.ExecContext(ctx, "INSERT INTO DOWNLOADER (NAME, ENABLED, TYPE, TRANSFER, ONLY_NASTOOL, MATCH_PATH, RMT_MODE, CONFIG, DOWNLOAD_DIR) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)", item.Name, item.Enabled, item.Type, item.Transfer, item.OnlyNastool, item.MatchPath, item.RmtMode, item.Config, item.DownloadDir)
		if err != nil {
			return Downloader{}, fmt.Errorf("insert downloader: %w", err)
		}
		item.ID, err = result.LastInsertId()
		if err != nil {
			return Downloader{}, fmt.Errorf("read inserted downloader ID: %w", err)
		}
		return item, nil
	}
	result, err := store.database.ExecContext(ctx, "UPDATE DOWNLOADER SET NAME=?, ENABLED=?, TYPE=?, TRANSFER=?, ONLY_NASTOOL=?, MATCH_PATH=?, RMT_MODE=?, CONFIG=?, DOWNLOAD_DIR=? WHERE ID=?", item.Name, item.Enabled, item.Type, item.Transfer, item.OnlyNastool, item.MatchPath, item.RmtMode, item.Config, item.DownloadDir, item.ID)
	if err != nil {
		return Downloader{}, fmt.Errorf("update downloader: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Downloader{}, fmt.Errorf("read updated downloader count: %w", err)
	}
	if count == 0 {
		return Downloader{}, ErrNotFound
	}
	return item, nil
}

func (store *Store) Delete(ctx context.Context, id int64) error {
	result, err := store.database.ExecContext(ctx, "DELETE FROM DOWNLOADER WHERE ID = ?", id)
	if err != nil {
		return fmt.Errorf("delete downloader: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read deleted downloader count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) SetFlag(ctx context.Context, id int64, flag string, value int) error {
	column := map[string]string{"enabled": "ENABLED", "transfer": "TRANSFER", "only_nastool": "ONLY_NASTOOL", "match_path": "MATCH_PATH"}[flag]
	if column == "" || (value != 0 && value != 1) {
		return errors.New("invalid downloader flag")
	}
	result, err := store.database.ExecContext(ctx, "UPDATE DOWNLOADER SET "+column+" = ? WHERE ID = ?", value, id)
	if err != nil {
		return fmt.Errorf("update downloader flag: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read downloader flag update count: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (store *Store) Close() error {
	return store.database.Close()
}

type scanner interface {
	Scan(...any) error
}

func scanDownloader(row scanner) (Downloader, error) {
	var item Downloader
	err := row.Scan(&item.ID, &item.Name, &item.Enabled, &item.Type, &item.Transfer, &item.OnlyNastool, &item.MatchPath, &item.RmtMode, &item.Config, &item.DownloadDir)
	if err != nil {
		return Downloader{}, err
	}
	return item, nil
}

func scanDownloadSetting(row scanner) (DownloadSetting, error) {
	var item DownloadSetting
	err := row.Scan(&item.ID, &item.Name, &item.Category, &item.Tags, &item.Paused, &item.UploadLimit, &item.DownloadLimit, &item.RatioLimit, &item.SeedingTimeLimit, &item.DownloaderID)
	if err != nil {
		return DownloadSetting{}, err
	}
	return item, nil
}
