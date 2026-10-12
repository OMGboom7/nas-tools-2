package organization

import (
	"context"
	"database/sql"
	"errors"
)

type HistoryRecord struct {
	ID             int64  `json:"id"`
	Mode           string `json:"mode"`
	Type           string `json:"type"`
	Category       string `json:"category"`
	TMDBID         string `json:"tmdbId"`
	Title          string `json:"title"`
	Year           string `json:"year"`
	SeasonEpisode  string `json:"seasonEpisode"`
	Source         string `json:"source"`
	SourcePath     string `json:"sourcePath"`
	SourceFilename string `json:"sourceFilename"`
	Dest           string `json:"dest"`
	DestPath       string `json:"destPath"`
	DestFilename   string `json:"destFilename"`
	Date           string `json:"date"`
}

const HistoryColumns = `ID,COALESCE(MODE,''),COALESCE(TYPE,''),COALESCE(CATEGORY,''),COALESCE(TMDBID,''),COALESCE(TITLE,''),COALESCE(YEAR,''),COALESCE(SEASON_EPISODE,''),COALESCE(SOURCE,''),COALESCE(SOURCE_PATH,''),COALESCE(SOURCE_FILENAME,''),COALESCE(DEST,''),COALESCE(DEST_PATH,''),COALESCE(DEST_FILENAME,''),COALESCE(DATE,'')`

func ScanHistory(row interface{ Scan(...any) error }) (HistoryRecord, error) {
	var record HistoryRecord
	err := row.Scan(&record.ID, &record.Mode, &record.Type, &record.Category, &record.TMDBID, &record.Title, &record.Year, &record.SeasonEpisode, &record.Source, &record.SourcePath, &record.SourceFilename, &record.Dest, &record.DestPath, &record.DestFilename, &record.Date)
	return record, err
}
func readHistory(ctx context.Context, reader unknownReader, id int64) (HistoryRecord, error) {
	record, err := ScanHistory(reader.QueryRowContext(ctx, `SELECT `+HistoryColumns+` FROM TRANSFER_HISTORY WHERE ID=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrNotFound
	}
	return record, err
}
func (s *Store) GetHistory(ctx context.Context, id int64) (HistoryRecord, error) {
	return readHistory(ctx, s.db, id)
}
func (s *Store) VerifyHistory(ctx context.Context, job Job) error {
	if job.Definition.History == nil {
		return nil
	}
	current, err := s.GetHistory(ctx, job.Definition.History.ID)
	if err != nil {
		return err
	}
	if Digest(current) != Digest(*job.Definition.History) {
		return ErrState
	}
	return nil
}
