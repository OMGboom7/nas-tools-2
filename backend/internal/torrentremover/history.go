package torrentremover

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

func cleanupHistories(ctx context.Context, tx *sql.Tx, r Receipt) error {
	for _, history := range r.Definition.Histories {
		remove := false
		next := history.Value
		if history.Plugin == "TorrentTransfer" {
			id, hash, ok := strings.Cut(history.Key, "-")
			if !ok || !ValidID(id) {
				return ErrState
			}
			root := Hash([]string{id, strings.ToLower(hash)})
			for _, task := range r.Tasks {
				if task.Root == root && task.Role == "transfer" && task.State == "completed" {
					remove = true
				}
			}
		} else {
			var entries []map[string]json.RawMessage
			if json.Unmarshal([]byte(history.Value), &entries) != nil || entries == nil {
				return ErrState
			}
			retained := []map[string]json.RawMessage{}
			changed := false
			for _, entry := range entries {
				id, err := exactJSONID(entry["downloader"])
				if err != nil {
					return err
				}
				hashes, err := readSeedHashes(entry["torrents"])
				if err != nil {
					return err
				}
				roots := map[string]bool{}
				for _, task := range r.Tasks {
					if task.Role != "cross-seed" && task.Downloader == id && task.Task == history.Key && task.State == "completed" {
						roots[task.Root] = true
					}
				}
				keep := []string{}
				for _, hash := range hashes {
					confirmed := false
					for _, task := range r.Tasks {
						if roots[task.Root] && task.Downloader == id && task.Task == hash && task.State == "completed" {
							confirmed = true
							break
						}
					}
					if confirmed {
						changed = true
					} else {
						keep = append(keep, hash)
					}
				}
				if len(keep) == 0 && len(hashes) > 0 {
					continue
				}
				if len(keep) != len(hashes) {
					entry["torrents"], _ = json.Marshal(keep)
				}
				retained = append(retained, entry)
			}
			if !changed {
				continue
			}
			if len(retained) == 0 {
				remove = true
			} else {
				b, err := json.Marshal(retained)
				if err != nil {
					return err
				}
				next = string(b)
			}
		}
		if !remove && next == history.Value {
			continue
		}
		// Match both ID and the exact frozen document. A conflict aborts every
		// history edit and leaves the receipt unfinalized for explicit review.
		var result sql.Result
		var err error
		if remove {
			result, err = tx.ExecContext(ctx, `DELETE FROM PLUGIN_HISTORY WHERE ID=? AND PLUGIN_ID=? AND KEY=? AND VALUE=? AND COALESCE(DATE,'')=?`, history.ID, history.Plugin, history.Key, history.Value, history.Date)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE PLUGIN_HISTORY SET VALUE=? WHERE ID=? AND PLUGIN_ID=? AND KEY=? AND VALUE=? AND COALESCE(DATE,'')=?`, next, history.ID, history.Plugin, history.Key, history.Value, history.Date)
		}
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrState
		}
		var total int
		var stored string
		if remove {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM PLUGIN_HISTORY WHERE ID=?`, history.ID).Scan(&total); err != nil || total != 0 {
				return ErrState
			}
		} else {
			if err := tx.QueryRowContext(ctx, `SELECT VALUE FROM PLUGIN_HISTORY WHERE ID=?`, history.ID).Scan(&stored); err != nil || stored != next {
				return ErrState
			}
		}
	}
	return nil
}
