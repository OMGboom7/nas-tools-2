package rsstaskconfig

import (
	"context"
	"errors"
)

// The old marker table is shared by site feeds, subscriptions and plugins.
// Preserve SQL NULL for fields omitted by name-only plugin markers.
type torrentMarker struct{ Name, Enclosure, Type, Title, Year, Season, Episode *string }

func (store *Store) applyMarker(ctx context.Context, op string, m torrentMarker) (bool, error) {
	switch op {
	case "insert-marker":
		_, err := store.database.ExecContext(ctx, `INSERT INTO RSS_TORRENTS(TORRENT_NAME,ENCLOSURE,TYPE,TITLE,YEAR,SEASON,EPISODE) VALUES(?,?,?,?,?,?,?)`, m.Name, m.Enclosure, m.Type, m.Title, m.Year, m.Season, m.Episode)
		return err == nil, err
	case "has-marker":
		if m.Enclosure != nil && *m.Enclosure != "" {
			return store.IsProcessed(ctx, "D", "", "", *m.Enclosure)
		}
		if m.Name == nil || *m.Name == "" {
			return true, nil
		}
		return store.IsProcessed(ctx, "D", *m.Name, "", "")
	case "remove-marker":
		query := "DELETE FROM RSS_TORRENTS WHERE TORRENT_NAME IS ?"
		args := []any{m.Name}
		if m.Enclosure != nil && *m.Enclosure != "" {
			query += " AND ENCLOSURE=?"
			args = append(args, *m.Enclosure)
		}
		_, err := store.database.ExecContext(ctx, query, args...)
		return err == nil, err
	default:
		return false, errors.New("invalid marker operation")
	}
}

// ClearSubscriptionCache implements the existing cache reset used by the
// service page, chat command and client API. Completed history and pending
// downloader receipts live in separate tables.
func (store *Store) ClearSubscriptionCache(ctx context.Context) error {
	tx, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM RSS_TORRENTS"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM RSS_TV_EPISODES"); err != nil {
		return err
	}
	return tx.Commit()
}
