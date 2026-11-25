package downloadhistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	_ "modernc.org/sqlite"
)

func Command(ctx context.Context, path, op string, in io.Reader, out io.Writer) error {
	input := &io.LimitedReader{R: in, N: 8 << 20}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if op == "notice" {
		var n Notice
		if d.Decode(&n) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 || n.Source == "" {
			return ErrInput
		}
		title, message := n.Render()
		return json.NewEncoder(out).Encode([]string{title, message})
	}
	var q query
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 {
		return ErrInput
	}
	mode := "ro"
	if op == "record" {
		mode = "rw"
	}
	u := url.URL{Scheme: "file", Path: (config.Application{Path: path}).UserDatabasePath(), RawQuery: url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)"}, "_txlock": {"immediate"}}.Encode()}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	if op == "record" {
		if q.Downloader == nil || q.Task == nil {
			return ErrInput
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = RecordTx(ctx, tx, q.Media, *q.Downloader, *q.Task, q.Path); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(true)
	}
	exists := false
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='DOWNLOAD_HISTORY')`).Scan(&exists); err != nil {
		return err
	}
	if op == "exists" {
		if exists {
			if q.Enclosure != nil && *q.Enclosure != "" {
				err = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM DOWNLOAD_HISTORY WHERE ENCLOSURE=?)", q.Enclosure).Scan(&exists)
			} else {
				err = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM DOWNLOAD_HISTORY WHERE DOWNLOADER IS ? AND DOWNLOAD_ID IS ?)", q.Downloader, q.Task).Scan(&exists)
			}
			if err != nil {
				return err
			}
		}
		return json.NewEncoder(out).Encode(exists)
	}
	if !exists {
		return json.NewEncoder(out).Encode([]any{})
	}
	rows, err := records(ctx, db, op, q)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(rows)
}
