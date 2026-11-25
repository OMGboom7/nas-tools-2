package downloaderconfig

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
)

// DirectMedia records the media covered by a download that has no subscription.
// For movies, the single unit 0 represents the complete movie.
type DirectMedia struct {
	Kind         string
	TMDBID       string
	Name         string
	Year         string
	Season       int
	Units        []int
	ExtraSeasons map[int][]int `json:"extraSeasons,omitempty"`
}

func (m DirectMedia) valid() bool {
	id, err := strconv.ParseInt(m.TMDBID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != m.TMDBID || m.Name == "" {
		return false
	}
	if m.Kind == "movie" {
		return len(m.ExtraSeasons) == 0 && m.Season == 0 && slices.Equal(m.Units, []int{0})
	}
	if m.Kind != "tv" || m.Season < 0 || m.Season > 1000 || len(m.Units) == 0 || len(m.Units) > 10000 {
		return false
	}
	if len(m.ExtraSeasons) > 100 || !directEpisodes(m.Units) {
		return false
	}
	for season, units := range m.ExtraSeasons {
		if season == m.Season || season < 0 || season > 1000 || !directEpisodes(units) {
			return false
		}
	}
	return true
}
func directEpisodes(units []int) bool {
	if len(units) == 0 || len(units) > 10000 {
		return false
	}
	last := 0
	for _, n := range units {
		if n <= last || n > 10000 {
			return false
		}
		last = n
	}
	return true
}

type directReceiptReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readDirectCoverage(ctx context.Context, r directReceiptReader, kind, id string, season int) (map[int]bool, error) {
	covered := map[int]bool{}
	rows, err := r.QueryContext(ctx, manualSelect+` WHERE json_extract(DOCUMENT,'$.direct.TMDBID')=? AND json_extract(DOCUMENT,'$.direct.Kind')=? AND STATE!='rejected'`, id, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		s, err := scanManual(rows)
		if err != nil {
			return nil, err
		}
		m := s.Definition.Direct
		if m == nil || !m.valid() || s.State != "submitted" || s.TaskID == "" {
			return nil, ErrManualPending
		}
		units := m.ExtraSeasons[season]
		if m.Season == season {
			units = m.Units
		}
		for _, n := range units {
			covered[n] = true
		}
	}
	return covered, rows.Err()
}

func (s *Store) DirectCoverage(ctx context.Context, kind, id string, season int) (map[int]bool, error) {
	var exists bool
	if err := s.database.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='GO_MANUAL_DOWNLOAD_SUBMISSIONS' AND type='table')").Scan(&exists); err != nil || !exists {
		return nil, err
	}
	return readDirectCoverage(ctx, s.database, kind, id, season)
}

// The selection and original download receipt are reserved in one transaction.
// Only this pending owner can advance or record these files.
func directFileSubmission(ctx context.Context, r interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, d FileSelectionDefinition) (ManualSubmission, error) {
	row, err := scanManual(r.QueryRowContext(ctx, manualSelect+` WHERE ID=(SELECT SUBMISSION_ID FROM GO_MANUAL_DOWNLOAD_SCOPES WHERE SCOPE=?)`, profileScope(d.DownloaderID, d.InfoHash)))
	if err != nil {
		return row, err
	}
	m := row.Definition
	if row.State != "pending" || m.Direct == nil || !m.Direct.valid() || m.InfoHash != d.InfoHash || m.DownloaderID != d.DownloaderID || m.DownloaderType != d.DownloaderType || m.ConfigDigest != d.ConfigDigest || m.ProfileDigest != d.ProfileFingerprint() || m.Direct.Season != d.Season || !slices.Equal(m.Direct.Units, d.Episodes) {
		return row, ErrFileSelectionPending
	}
	return row, nil
}
func DirectFileSubmissionTx(ctx context.Context, tx *sql.Tx, d FileSelectionDefinition) (ManualSubmission, error) {
	return directFileSubmission(ctx, tx, d)
}
func (s *Store) DirectFileSubmission(ctx context.Context, d FileSelectionDefinition) (ManualSubmission, error) {
	return directFileSubmission(ctx, s.database, d)
}
