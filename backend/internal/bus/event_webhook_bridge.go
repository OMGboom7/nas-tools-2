package bus

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
)

func finishBusinessSignal(ctx context.Context, tx *sql.Tx, kind, key, reference, title, body string, data json.RawMessage) error {
	if len(data) > 0 {
		// Complete the original typed fact before any subscriber snapshots it.
		if _, err := tx.ExecContext(ctx, `INSERT INTO GO_BUSINESS_EVENT_DATA(EVENT_ID,DATA)VALUES(?,?) ON CONFLICT(EVENT_ID) DO NOTHING`, Hash([]string{kind, key}), string(data)); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT DATA FROM GO_BUSINESS_EVENT_DATA WHERE EVENT_ID=?`, Hash([]string{kind, key})).Scan(&raw); err != nil {
			return err
		}
		if raw != string(data) {
			return ErrState
		}
	}
	legacy := map[string]string{"download_start": "download.add", "rss_added": "subscribe.add", "rss_finished": "subscribe.finished", "source_file_deleted": "sourcefile.deleted", "library_file_deleted": "libraryfile.deleted"}[kind]
	if legacy == "" {
		return nil
	}
	var payload map[string]any
	if len(data) > 0 {
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber()
		if d.Decode(&payload) != nil || payload == nil {
			return ErrState
		}
	} else {
		payload = map[string]any{}
	}
	// Add observation context without inventing missing domain fields or
	// turning an accepted download into a pre-add mutation hook.
	payload["native_event"] = map[string]any{"id": Hash([]string{kind, key}), "kind": kind, "reference": reference, "title": title, "text": body, "stage": "committed"}
	return PublishSignal(ctx, tx, legacy, key, reference, payload)
}
