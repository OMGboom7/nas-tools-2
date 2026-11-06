package torrentremover

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
)

type reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func tableExists(ctx context.Context, q reader, table string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists)
	return exists, err
}
func exactJSONID(raw json.RawMessage) (string, error) {
	var id string
	if json.Unmarshal(raw, &id) != nil {
		id = string(raw)
	}
	if !ValidID(id) {
		return "", ErrState
	}
	return id, nil
}
func historyFor(ctx context.Context, q reader, plugin, key string) (*History, error) {
	exists, err := tableExists(ctx, q, "PLUGIN_HISTORY")
	if err != nil || !exists {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT CAST(ID AS TEXT),PLUGIN_ID,KEY,COALESCE(VALUE,''),COALESCE(DATE,'') FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=? ORDER BY ID LIMIT 2`, plugin, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out *History
	for rows.Next() {
		var h History
		if rows.Scan(&h.ID, &h.Plugin, &h.Key, &h.Value, &h.Date) != nil || out != nil || len(h.Value) > 1<<20 || !json.Valid([]byte(h.Value)) {
			return nil, ErrState
		}
		out = &h
	}
	return out, rows.Err()
}
func readSeedHashes(raw json.RawMessage) ([]string, error) {
	var hashes []string
	if len(raw) > 0 && raw[0] == '[' {
		if json.Unmarshal(raw, &hashes) != nil {
			return nil, ErrState
		}
	} else {
		var h string
		if json.Unmarshal(raw, &h) != nil {
			return nil, ErrState
		}
		hashes = []string{h}
	}
	if len(hashes) > 1000 {
		return nil, ErrState
	}
	seen := map[string]bool{}
	for i, h := range hashes {
		h = strings.ToLower(h)
		if !downloadcompletion.ValidID("qbittorrent", h) || seen[h] {
			return nil, ErrState
		}
		seen[h] = true
		hashes[i] = h
	}
	return hashes, nil
}

// Snapshot reads the original rows within the source-event transaction. It
// preserves the exact historical JSON, including integers beyond float64.
func Snapshot(ctx context.Context, q reader, event Event, control Control) (Definition, error) {
	d := Definition{Event: event, Control: control, Candidates: []Candidate{}, Histories: []History{}, Problems: []string{}}
	if !event.Valid() || !control.Valid() {
		return d, ErrState
	}
	exists, err := tableExists(ctx, q, "DOWNLOAD_HISTORY")
	if err != nil || !exists {
		return d, err
	}
	rows, err := q.QueryContext(ctx, `SELECT CAST(ID AS TEXT),COALESCE(TITLE,''),COALESCE(DOWNLOADER,''),COALESCE(DOWNLOAD_ID,''),COALESCE(SAVE_PATH,'') FROM DOWNLOAD_HISTORY WHERE TITLE=? ORDER BY ID LIMIT 1001`, event.Title)
	if err != nil {
		return d, err
	}
	type original struct {
		id, hash string
		record   Download
	}
	originals := []original{}
	for rows.Next() {
		var item original
		if err := rows.Scan(&item.record.ID, &item.record.Title, &item.id, &item.hash, &item.record.SavePath); err != nil {
			rows.Close()
			return d, err
		}
		item.record.Downloader = item.id
		item.record.Task = item.hash
		originals = append(originals, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	if len(originals) > 1000 {
		return d, ErrState
	}
	seen := map[string]bool{}
	historySeen := map[string]bool{}
	remember := func(h *History) {
		if h != nil && !historySeen[h.ID] {
			historySeen[h.ID] = true
			d.Histories = append(d.Histories, *h)
		}
	}
	add := func(root, id, hash, role, parent string) (Candidate, bool) {
		hash = strings.ToLower(hash)
		t, ok := targetFor(control, id)
		if !ok || !downloadcompletion.ValidID(t.Type, hash) {
			d.Problems = append(d.Problems, "原历史关联的下载器或任务不在已审核范围："+id)
			return Candidate{}, false
		}
		c := Candidate{ID: CandidateID(root, id, hash, role), Downloader: id, Task: hash, Root: root, Role: role, Parent: parent}
		if !seen[c.ID] {
			seen[c.ID] = true
			d.Candidates = append(d.Candidates, c)
		}
		return c, true
	}
	for _, o := range originals {
		if o.id == "" || o.hash == "" {
			continue
		}
		if !ValidID(o.id) || (!downloadcompletion.ValidID("qbittorrent", o.hash) && !downloadcompletion.ValidID("aria2", o.hash)) {
			return d, ErrState
		}
		d.Downloads = append(d.Downloads, o.record)
		o.hash = strings.ToLower(o.hash)
		root := Hash([]string{o.id, o.hash})
		historyKey := o.id + "-" + o.hash
		transfer, err := historyFor(ctx, q, "TorrentTransfer", historyKey)
		if err != nil {
			return d, err
		}
		targetID, targetHash := o.id, o.hash
		role := "original"
		if transfer != nil {
			var v map[string]json.RawMessage
			if json.Unmarshal([]byte(transfer.Value), &v) != nil || v == nil {
				return d, ErrState
			}
			targetID, err = exactJSONID(v["to_download"])
			if err != nil {
				return d, err
			}
			if json.Unmarshal(v["to_download_id"], &targetHash) != nil || !downloadcompletion.ValidID("qbittorrent", targetHash) {
				return d, ErrState
			}
			var deleted bool
			if b, ok := v["delete_source"]; !ok || string(b) == "null" || json.Unmarshal(b, &deleted) != nil {
				return d, ErrState
			}
			if !deleted {
				add(root, o.id, o.hash, "original", "")
			}
			role = "transfer"
			remember(transfer)
		}
		base, ok := add(root, targetID, targetHash, role, "")
		if !ok {
			continue
		}
		seed, err := historyFor(ctx, q, "IYUUAutoSeed", base.Task)
		if err != nil {
			return d, err
		}
		if seed == nil {
			continue
		}
		var entries []map[string]json.RawMessage
		if json.Unmarshal([]byte(seed.Value), &entries) != nil || entries == nil || len(entries) > 1000 {
			return d, ErrState
		}
		for _, entry := range entries {
			id, err := exactJSONID(entry["downloader"])
			if err != nil {
				return d, err
			}
			hashes, err := readSeedHashes(entry["torrents"])
			if err != nil {
				return d, err
			}
			// The original plugin only removes cross-seeds on the same downloader.
			if id != targetID {
				continue
			}
			remember(seed)
			for _, hash := range hashes {
				if strings.EqualFold(hash, base.Task) {
					continue
				}
				add(root, id, hash, "cross-seed", base.ID)
			}
		}
	}
	if len(d.Candidates) > 2000 || len(d.Histories) > 1000 || len(d.Problems) > 1000 || !d.Valid() {
		return d, ErrState
	}
	return d, nil
}

// DownloaderDigest binds every field which influences endpoint, isolation and
// path handling. Callers must use this projection for both review and execution.
func DownloaderDigest(id int64, name string, enabled int, kind string, transfer, onlyNastool, matchPath int, mode, config, dirs string) string {
	return Hash([]any{strconv.FormatInt(id, 10), name, enabled, kind, transfer, onlyNastool, matchPath, mode, config, dirs})
}
