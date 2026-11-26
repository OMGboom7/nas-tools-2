package rsstaskconfig

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
)

// Command gives the classic UI and scheduler access to the same history as React.
func Command(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	var q struct {
		ID         int64
		Flag, Uses string
		Articles   []Article
		Marker     *torrentMarker
	}
	input := &io.LimitedReader{R: in, N: 8 << 20}
	dec := json.NewDecoder(input)
	dec.DisallowUnknownFields()
	if dec.Decode(&q) != nil || dec.Decode(new(any)) != io.EOF || input.N == 0 {
		return errors.New("invalid RSS history request")
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	dbPath := (config.Application{Path: path}).UserDatabasePath()
	info, err := os.Stat(dbPath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("RSS database unavailable")
	}
	store, err := Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	var result any
	switch op {
	case "has-marker", "insert-marker", "remove-marker":
		if q.Marker == nil {
			return errors.New("missing RSS marker")
		}
		result, err = store.applyMarker(ctx, op, *q.Marker)
	case "clear-cache":
		err = store.ClearSubscriptionCache(ctx)
		result = true
	case "clear-markers":
		_, err = store.database.ExecContext(ctx, "DELETE FROM RSS_TORRENTS")
		result = true
	case "processed":
		flags := make([]bool, len(q.Articles))
		for i, item := range q.Articles {
			flags[i], err = store.IsProcessed(ctx, q.Uses, item.Title, item.Year, item.Enclosure)
			if err != nil {
				return err
			}
		}
		result = flags
	case "mark":
		if q.ID < 1 || len(q.Articles) == 0 {
			return errors.New("invalid RSS task or articles")
		}
		for _, article := range q.Articles {
			if strings.TrimSpace(article.Title) == "" {
				return errors.New("missing RSS article title")
			}
		}
		if err := store.SetArticles(ctx, q.ID, q.Flag, q.Articles); err != nil {
			return err
		}
		result = true
	case "history":
		result, err = store.History(ctx, q.ID)
	case "progress":
		task, e := store.Get(ctx, q.ID)
		if e != nil {
			return e
		}
		result = map[string]string{"counter": task.ProcessCount, "update_time": task.UpdateTime}
	default:
		return errors.New("unknown RSS history operation")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
