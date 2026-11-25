package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/iyuu"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

func startIyuuSeeds(ctx context.Context, api *iyuuAPI) func() {
	if api == nil || api.transfer.organizer.jobs == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				api.seedTick(ctx)
			}
		}
	}()
	scheduled := make(chan struct{})
	go func() {
		defer close(scheduled)
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if err := api.scheduleTick(ctx, now); err != nil && ctx.Err() == nil {
					slog.Warn("IYUU 自动辅种状态更新失败")
				}
			}
		}
	}()
	return func() { <-done; <-scheduled }
}
func (api *iyuuAPI) seedTick(ctx context.Context) {
	db, err := api.seedDB(ctx)
	if err != nil {
		return
	}
	defer db.Close()
	var cursor int64
	for ctx.Err() == nil {
		rows, err := db.QueryContext(ctx, `SELECT rowid,id FROM GO_IYUU_SEEDS WHERE rowid>? AND state IN ('adding','added','bound','verifying','verified','prepared','started') ORDER BY rowid LIMIT 64`, cursor)
		if err != nil {
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&cursor, &id) != nil {
				rows.Close()
				return
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			func() {
				step, cancel := context.WithTimeout(ctx, 45*time.Second)
				defer cancel()
				_ = api.advanceSeed(step, db, id)
			}()
		}
		if len(ids) < 64 {
			return
		}
	}
}

func (api *iyuuAPI) advanceSeed(ctx context.Context, db *sql.DB, id string) error {
	var j iyuuJob
	var state, raw string
	read := func() error {
		if err := db.QueryRowContext(ctx, "SELECT job,state FROM GO_IYUU_SEEDS WHERE id=?", id).Scan(&raw, &state); err != nil {
			return err
		}
		if json.Unmarshal([]byte(raw), &j) != nil || j.Receipt != id || j.Match.Hash != j.Meta.V1Hash {
			return iyuu.ErrInput
		}
		return nil
	}
	if err := read(); err != nil {
		return err
	}
	s, err := api.scope(ctx, j.Actor)
	if err != nil {
		return err
	}
	c, err := api.seedScope(ctx, s, j.Match)
	if err != nil || c.proof != j.Scope {
		return iyuu.ErrInput
	}
	lock, err := api.transfer.organizer.jobs.AcquireJob(ctx, torrentLabelGuard(c.d.ID, j.Match.Hash))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := read(); err != nil {
		return err
	}
	if state == "adding" {
		// The sending process held the same lock. After a restart, only the
		// ledger's acknowledged add reply can promote an interrupted request.
		l, err := torrenttransfer.NewLedger(ctx, db)
		if err != nil {
			return err
		}
		next := iyuuAddState(ctx, l, torrenttransfer.Hash(c.client.Address()), j)
		return iyuuSaveStage(ctx, db, j, state, next)
	}
	src, err := api.seedSource(ctx, j, c)
	if err != nil || !iyuuPayloadMatches(src, j.Meta) {
		return iyuu.ErrInput
	}
	t, err := c.client.TransferTask(ctx, j.Match.Hash)
	if err != nil {
		return err
	}
	dir, err := torrenttransfer.NormalizeRemote(t.Directory)
	if err != nil || dir != j.Directory || !t.Matches(j.Meta) || t.Error || t.AutoManaged || !slices.Contains(t.Labels, "已整理") || !slices.Contains(t.Labels, "辅种") || !slices.Contains(t.Labels, "iyuu-"+id) {
		return iyuu.ErrInput
	}
	proof := iyuuSourceProof(t)
	if state != "added" && j.TargetProof != proof {
		return iyuu.ErrInput
	}
	switch state {
	case "added":
		if !t.Paused && !t.Checking {
			return iyuu.ErrInput
		}
		j.TargetProof = proof
		next := "bound"
		if t.Kind == "transmission" && t.Checking {
			j.WasIncomplete = true
			next = "verifying"
		}
		return api.recordSeedAdded(ctx, db, j, s, next)
	case "bound":
		if !t.Paused || t.Checking {
			return nil
		}
		j.WasIncomplete = !t.FullComplete(j.Meta)
		err := api.seedMutation(ctx, db, j, state, "verify_pending", "verifying", func() error { return c.client.RecheckTransfer(ctx, t, j.Meta) })
		if err != nil || j.WasIncomplete {
			return err
		}
		// Observe short checks immediately, before the next worker tick.
		after, err := c.client.TransferTask(ctx, j.Match.Hash)
		if err == nil && after.Checking && iyuuSourceProof(after) == j.TargetProof {
			j.WasIncomplete = true
			return iyuuSaveStage(ctx, db, j, "verifying", "verifying")
		}
		return err
	case "verifying":
		if t.Checking && !j.WasIncomplete {
			j.WasIncomplete = true
			return iyuuSaveStage(ctx, db, j, state, state)
		}
		if t.Paused && t.FullComplete(j.Meta) {
			verified := j.WasIncomplete
			if !verified {
				if tr, ok := c.client.(interface {
					ReseedVerified(context.Context, torrenttransfer.Task, torrentmeta.TransferMetadata) (bool, error)
				}); ok {
					verified, err = tr.ReseedVerified(ctx, t, j.Meta)
					if err != nil {
						return err
					}
				}
			}
			if verified {
				return iyuuSaveStage(ctx, db, j, state, "verified")
			}
		}
	case "verified":
		if !t.Paused || !t.HasTracker || !t.FullComplete(j.Meta) {
			return nil
		}
		if t.Kind == "transmission" && j.Meta.Private {
			profile, ok := c.client.(interface {
				PrepareReseedStart(context.Context, torrenttransfer.Task, torrentmeta.TransferMetadata, string) error
			})
			if !ok {
				return iyuu.ErrInput
			}
			return api.seedMutation(ctx, db, j, state, "profile_pending", "prepared", func() error { return profile.PrepareReseedStart(ctx, t, j.Meta, id) })
		}
		return iyuuSaveStage(ctx, db, j, state, "prepared")
	case "prepared":
		if !t.Paused || !t.HasTracker || !t.FullComplete(j.Meta) {
			return nil
		}
		return api.seedMutation(ctx, db, j, state, "start_pending", "started", func() error { return c.client.StartTransfer(ctx, t, j.Meta) })
	case "started":
		if t.SourceSeeding && t.FullComplete(j.Meta) {
			return iyuuSaveStage(ctx, db, j, state, "seeding")
		}
	}
	return nil
}
func (api *iyuuAPI) seedMutation(ctx context.Context, db *sql.DB, j iyuuJob, old, pending, next string, send func() error) error {
	if err := iyuuSaveStage(ctx, db, j, old, pending); err != nil {
		return err
	}
	err := send()
	if err != nil {
		next = "unknown"
	}
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(err, iyuuSaveStage(save, db, j, pending, next))
}

// Match the old per-source/downloader history consumed by TorrentRemover.
// Unknown fields and other downloader entries are kept intact.
func (api *iyuuAPI) recordSeedAdded(ctx context.Context, db *sql.DB, j iyuuJob, s iyuuScope, next string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS PLUGIN_HISTORY (ID INTEGER PRIMARY KEY, PLUGIN_ID TEXT, KEY TEXT, VALUE TEXT, DATE TEXT)`)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT ID,VALUE FROM PLUGIN_HISTORY WHERE PLUGIN_ID=? AND KEY=? LIMIT 2", iyuu.Plugin, j.Match.Source)
	if err != nil {
		return err
	}
	var rowID int64
	history := []map[string]json.RawMessage{}
	for rows.Next() {
		var raw string
		if rowID != 0 || rows.Scan(&rowID, &raw) != nil || json.Unmarshal([]byte(raw), &history) != nil || history == nil {
			rows.Close()
			return iyuu.ErrInput
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	matched := false
	for _, item := range history {
		var d json.Number
		if json.Unmarshal(item["downloader"], &d) != nil {
			return iyuu.ErrInput
		}
		if d.String() != j.Match.DownloaderID {
			continue
		}
		if matched {
			return iyuu.ErrInput
		}
		matched = true
		var list []string
		if err := json.Unmarshal(item["torrents"], &list); err != nil {
			return err
		}
		if !slices.Contains(list, j.Match.Hash) {
			list = append(list, j.Match.Hash)
		}
		item["torrents"], _ = json.Marshal(list)
	}
	if !matched {
		ds, _ := json.Marshal(json.Number(j.Match.DownloaderID))
		hs, _ := json.Marshal([]string{j.Match.Hash})
		history = append(history, map[string]json.RawMessage{"downloader": ds, "torrents": hs})
	}
	value, err := json.Marshal(history)
	if err != nil {
		return err
	}
	if rowID == 0 {
		_, err = tx.ExecContext(ctx, "INSERT INTO PLUGIN_HISTORY(PLUGIN_ID,KEY,VALUE,DATE) VALUES(?,?,?,?)", iyuu.Plugin, j.Match.Source, string(value), time.Now().Format("2006-01-02 15:04:05"))
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE PLUGIN_HISTORY SET VALUE=? WHERE ID=?", string(value), rowID)
	}
	if err != nil {
		return err
	}
	err = systemconfig.UpdateManyTx(ctx, tx, iyuuKeys, func(raw map[string]string) (map[string]string, error) {
		if raw[iyuuGeneration] != s.raw[iyuuGeneration] || raw[iyuu.ConfigKey] != s.raw[iyuu.ConfigKey] || raw["UserInstalledPlugins"] != s.raw["UserInstalledPlugins"] {
			return nil, iyuu.ErrInput
		}
		fields, err := iyuu.Values(raw[iyuu.ConfigKey])
		if err != nil {
			return nil, err
		}
		var success []string
		if old := fields["success_caches"]; len(old) > 0 && json.Unmarshal(old, &success) != nil {
			return nil, iyuu.ErrInput
		}
		if !slices.Contains(success, j.Match.Hash) {
			success = append(success, j.Match.Hash)
		}
		fields["success_caches"], _ = json.Marshal(success)
		b, err := json.Marshal(fields)
		return map[string]string{iyuu.ConfigKey: string(b)}, err
	})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE GO_IYUU_SEEDS SET state=?,job=? WHERE id=? AND state='added'", next, string(raw), j.Receipt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return iyuu.ErrInput
	}
	return tx.Commit()
}
