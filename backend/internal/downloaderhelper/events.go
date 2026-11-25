package downloaderhelper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const ControlKey = "GoDownloaderHelper"

type Event struct {
	Downloader             json.Number `json:"downloader_id,omitempty"`
	Path, Filename, Driver string
}

// Capture records only fields needed by this consumer, with the schedule that
// was active at commit. A new installation must not adopt older deletion facts.
func Capture(ctx context.Context, tx *sql.Tx, kind, key string, data any) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='SYSTEM_DICT')").Scan(&exists); err != nil || !exists {
		return err
	}
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT VALUE FROM SYSTEM_DICT WHERE TYPE='SystemConfig' AND KEY=? ORDER BY ID DESC LIMIT 1", ControlKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var c struct {
		Revision         string
		Download, Source bool
	}
	if json.Unmarshal([]byte(raw), &c) != nil || c.Revision == "" || !(kind == "download.add" && c.Download || kind == "sourcefile.deleted" && c.Source) {
		return nil
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var e Event
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	if e.Downloader != "" {
		if id, err := e.Downloader.Int64(); err != nil || id <= 0 {
			return nil
		}
	}
	if kind == "sourcefile.deleted" && (e.Path == "" || e.Filename == "" || e.Driver != "" && e.Driver != "local") {
		return nil
	}
	b, err = json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_DOWNLOADER_HELPER_EVENTS(id TEXT PRIMARY KEY,revision TEXT NOT NULL,kind TEXT NOT NULL,created TEXT NOT NULL,data TEXT NOT NULL)`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO GO_DOWNLOADER_HELPER_EVENTS VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, kind+":"+key, c.Revision, kind, time.Now().UTC().Format(time.RFC3339Nano), string(b))
	return err
}
