package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/iyuu"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type iyuuSchedule struct {
	Actor, Scope, Revision, Last string
	Next, Once                   time.Time
}

func readIyuuSchedule(raw string) (iyuuSchedule, error) {
	var st iyuuSchedule
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			return st, err
		}
	}
	return st, nil
}

func iyuuScheduleProof(sel iyuuSelection, c iyuu.Settings) string {
	return sitelogin.Hash([]any{sel.proof, c.Enabled, c.Cron, c.Notify})
}

func (api *iyuuAPI) scheduleSummary(ctx context.Context, raw map[string]string) (bool, bool) {
	st, err := readIyuuSchedule(raw[iyuuControlKey])
	if err != nil {
		return false, false
	}
	if st.Next.IsZero() && st.Once.IsZero() {
		return false, true
	}
	_, _, err = api.scheduledScope(ctx, st, raw[iyuuControlKey])
	return err == nil, err == nil
}

func (api *iyuuAPI) configureSchedule(ctx context.Context, actor string, raw map[string]string, v map[string]json.RawMessage, c iyuu.Settings, now time.Time) (map[string]string, error) {
	old, err := readIyuuSchedule(raw[iyuuControlKey])
	if err != nil {
		return nil, err
	}
	st := iyuuSchedule{Last: old.Last}
	if c.Clear {
		v["success_caches"], v["error_caches"] = json.RawMessage("[]"), json.RawMessage("[]")
		v["clearcache"] = json.RawMessage("false")
	}
	loc, err := rssWorkerLocation()
	if err != nil {
		return nil, err
	}
	next, err := cookieCloudSchedule(c.Cron, loc, now)
	if err != nil {
		return nil, err
	}
	if c.Once && !c.Enabled {
		return nil, iyuu.ErrInput
	}
	if c.Enabled && (c.Once || !next.IsZero()) {
		auth, _, err := api.transfer.login.authority(actor)
		if err != nil {
			return nil, err
		}
		s := iyuuScope{actor: actor, authority: auth, cfg: c, raw: raw}
		sel, err := api.selection(ctx, s)
		if err != nil || c.Token == "" || len(sel.downloaders) == 0 || len(sel.downloaders) != len(c.Downloaders) || len(c.Sites) > 0 && len(sel.sites) != len(c.Sites) {
			return nil, iyuu.ErrInput
		}
		st.Revision, err = refreshNonce()
		if err != nil {
			return nil, err
		}
		st.Actor, st.Scope, st.Next = actor, iyuuScheduleProof(sel, c), next.UTC()
		if c.Once {
			st.Once = now.Add(3 * time.Second).UTC()
			v["onlyonce"] = json.RawMessage("false")
		} else if old.Scope == st.Scope {
			st.Once = old.Once
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	control, err := json.Marshal(st)
	return map[string]string{iyuu.ConfigKey: string(b), iyuuControlKey: string(control)}, err
}

func (api *iyuuAPI) scheduledScope(ctx context.Context, st iyuuSchedule, control string) (iyuuScope, iyuuSelection, error) {
	s, err := api.scope(ctx, st.Actor)
	if err != nil {
		return s, iyuuSelection{}, err
	}
	if s.raw[iyuuControlKey] != control || !s.cfg.Enabled {
		return s, iyuuSelection{}, iyuu.ErrInput
	}
	sel, err := api.selection(ctx, s)
	if err == nil && iyuuScheduleProof(sel, s.cfg) != st.Scope {
		err = iyuu.ErrInput
	}
	return s, sel, err
}

// Adopt an installed legacy schedule only on first upgrade. An empty control
// object written by uninstall/reinstall deliberately has a different meaning.
func (api *iyuuAPI) importSchedule(ctx context.Context, raw map[string]string, now time.Time) error {
	c, err := iyuu.Read(raw[iyuu.ConfigKey])
	if err != nil {
		return err
	}
	changes := map[string]string{iyuuControlKey: "{}"}
	if c.Clear || c.Enabled && (c.Cron != "" || c.Once) {
		cfg, err := api.transfer.login.config.Snapshot()
		if err != nil {
			return err
		}
		v, err := iyuu.Values(raw[iyuu.ConfigKey])
		if err != nil {
			return err
		}
		changes, err = api.configureSchedule(ctx, text(objectValue(cfg["app"])["login_user"]), raw, v, c, now)
		if err != nil {
			return err
		}
	}
	return api.transfer.system.UpdateMany(ctx, iyuuKeys, func(actual map[string]string) (map[string]string, error) {
		if !maps.Equal(actual, raw) {
			return nil, iyuu.ErrInput
		}
		return changes, nil
	})
}

type iyuuBatchProgress struct {
	Started                                                                           time.Time
	Cursor, Scanned, Returned, Added, Existing, Unknown, Failed, Cached, ScanFailures int
}

func (p iyuuBatchProgress) message() string {
	return fmt.Sprintf("已匹配源种 %d 个，服务返回 %d 条匹配，已处理候选 %d 个，暂停添加 %d 个，已有 %d 个，待核实 %d 个，提交失败 %d 个，新增失败缓存 %d 条，扫描失败的下载器 %d 个。", p.Scanned, p.Returned, p.Cursor, p.Added, p.Existing, p.Unknown, p.Failed, p.Cached, p.ScanFailures)
}

type iyuuBatch struct {
	id       string
	items    []iyuuMatch
	progress iyuuBatchProgress
}

// Freeze candidates before sending anything. Only one slot can execute under
// the cross-process lock, so one manifest is enough even after a config change.
func (api *iyuuAPI) loadBatch(ctx context.Context, db *sql.DB, id string, s iyuuScope, sel iyuuSelection, now time.Time, current func() error) (iyuuBatch, error) {
	b := iyuuBatch{id: id}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS GO_IYUU_BATCH (slot INTEGER PRIMARY KEY CHECK(slot=1), id TEXT NOT NULL, items TEXT NOT NULL, progress TEXT NOT NULL)`); err != nil {
		return b, err
	}
	var items, progress string
	err := db.QueryRowContext(ctx, "SELECT items,progress FROM GO_IYUU_BATCH WHERE slot=1 AND id=?", id).Scan(&items, &progress)
	if err == nil {
		if json.Unmarshal([]byte(items), &b.items) != nil || json.Unmarshal([]byte(progress), &b.progress) != nil || b.progress.Cursor < 0 || b.progress.Cursor > len(b.items) {
			return b, iyuu.ErrInput
		}
		return b, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return b, err
	}
	b.progress.Started = now
	matcher := iyuuMatcher{scope: s, sites: sel.sites, current: current}
	for _, d := range sel.downloaders {
		client, err := api.transfer.organizer.clientFromDownloader(d)
		if err != nil {
			b.progress.ScanFailures++
			continue
		}
		rows, n, err := matcher.scan(ctx, d, client)
		if err != nil {
			b.progress.ScanFailures++
			continue
		}
		b.progress.Scanned += n
		b.items = append(b.items, rows...)
	}
	b.progress.Returned = matcher.returned
	b.progress.Existing = matcher.existing
	if err = current(); err != nil {
		return b, err
	}
	rows, err := json.Marshal(b.items)
	if err != nil {
		return b, err
	}
	initial, err := json.Marshal(b.progress)
	if err != nil {
		return b, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO GO_IYUU_BATCH VALUES(1,?,?,?) ON CONFLICT(slot) DO UPDATE SET id=excluded.id,items=excluded.items,progress=excluded.progress`, id, string(rows), string(initial))
	return b, err
}

func (api *iyuuAPI) scheduleTick(ctx context.Context, now time.Time) error {
	raw, err := api.transfer.system.ReadMany(ctx, []string{"UserInstalledPlugins", iyuuControlKey})
	if err != nil {
		return err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], iyuu.Plugin)
	if err != nil || !installed {
		return err
	}
	control := raw[iyuuControlKey]
	if control == "" {
		full, err := api.transfer.system.ReadMany(ctx, iyuuKeys)
		if err != nil || full[iyuuControlKey] != "" || full["UserInstalledPlugins"] != raw["UserInstalledPlugins"] {
			return err
		}
		return api.importSchedule(ctx, full, now)
	}
	st, err := readIyuuSchedule(control)
	if err != nil {
		return err
	}
	due := func(t time.Time) bool { return !t.IsZero() && !t.After(now) }
	if !due(st.Once) && !due(st.Next) {
		return nil
	}
	g, err := api.transfer.organizer.jobs.AcquireJob(ctx, "iyuu-automatic-reseed")
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer g.Close()
	s, sel, err := api.scheduledScope(ctx, st, control)
	if err != nil {
		// A replaced site, account or downloader must never inherit the schedule.
		if !errors.Is(err, iyuu.ErrInput) && !errors.Is(err, sitelogin.ErrScope) {
			return err
		}
		return api.transfer.system.UpdateMany(ctx, iyuuKeys, func(actual map[string]string) (map[string]string, error) {
			if actual[iyuuControlKey] != control {
				return nil, nil
			}
			b, err := json.Marshal(iyuuSchedule{Last: "账户、下载器或站点设置已变化，自动辅种已停止。"})
			return map[string]string{iyuuControlKey: string(b)}, err
		})
	}
	id := sitelogin.Hash([]any{st.Revision, st.Once, st.Next})
	current := func() error { _, _, err := api.scheduledScope(ctx, st, control); return err }
	db, err := api.seedDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	b, err := api.loadBatch(ctx, db, id, s, sel, now, current)
	if err != nil {
		return err
	}
	for b.progress.Cursor < len(b.items) {
		m := b.items[b.progress.Cursor]
		s, sel, err = api.scheduledScope(ctx, st, control)
		if err != nil {
			return err
		}
		op, cancel := context.WithTimeout(ctx, 2*time.Minute)
		view, sendErr := api.submitCandidate(op, s, sel, m, id)
		cancel()
		if err = current(); err != nil {
			return err
		}
		cache := ""
		switch {
		case errors.Is(sendErr, errIyuuStore) || errors.Is(sendErr, organization.ErrBusy):
			return sendErr
		case sendErr != nil:
			b.progress.Failed++
			cache = iyuuCacheKey(sendErr)
		case view.State == "duplicate":
			b.progress.Existing++
		case view.State == "unknown" || view.State == "adding":
			b.progress.Unknown++
		case view.State == "unsent":
			b.progress.Failed++
		default:
			b.progress.Added++
		}
		b.progress.Cursor++
		if err := api.saveBatchItem(ctx, &b, control, m.Hash, cache); err != nil {
			return err
		}
	}
	if err = current(); err != nil {
		return err
	}
	if !st.Once.After(b.progress.Started) {
		st.Once = time.Time{}
	}
	if !st.Next.IsZero() && !st.Next.After(b.progress.Started) {
		loc, err := rssWorkerLocation()
		if err != nil {
			return err
		}
		st.Next, err = cookieCloudSchedule(s.cfg.Cron, loc, now)
		if err != nil {
			return err
		}
		st.Next = st.Next.UTC()
	}
	st.Last = b.progress.message()
	return api.transfer.system.Transaction(ctx, func(tx *sql.Tx) error {
		err := systemconfig.UpdateManyTx(ctx, tx, iyuuKeys, func(actual map[string]string) (map[string]string, error) {
			if actual[iyuuControlKey] != control || actual[iyuuGeneration] != s.raw[iyuuGeneration] {
				return nil, iyuu.ErrInput
			}
			data, err := json.Marshal(st)
			return map[string]string{iyuuControlKey: string(data)}, err
		})
		if err != nil {
			return err
		}
		p := b.progress
		if s.cfg.Notify && p.Added+p.Failed+p.Unknown+p.ScanFailures > 0 {
			if err := bus.EmitData(ctx, tx, "plugin_message", id, id, "【IYUU自动辅种任务完成】", st.Last, map[string]string{"plugin": iyuu.Plugin}); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM GO_IYUU_BATCH WHERE slot=1 AND id=?", id)
		return err
	})
}

func (api *iyuuAPI) saveBatchItem(ctx context.Context, b *iyuuBatch, control, hash, cache string) error {
	return api.transfer.system.Transaction(ctx, func(tx *sql.Tx) error {
		err := systemconfig.UpdateManyTx(ctx, tx, iyuuKeys, func(raw map[string]string) (map[string]string, error) {
			if raw[iyuuControlKey] != control {
				return nil, iyuu.ErrInput
			}
			if cache == "" {
				return nil, nil
			}
			v, err := iyuu.Values(raw[iyuu.ConfigKey])
			if err != nil {
				return nil, err
			}
			var list []string
			if data := v[cache]; data != nil && json.Unmarshal(data, &list) != nil {
				return nil, iyuu.ErrInput
			}
			if slices.Contains(list, hash) {
				return nil, nil
			}
			b.progress.Cached++
			v[cache], err = json.Marshal(append(list, hash))
			if err != nil {
				return nil, err
			}
			data, err := json.Marshal(v)
			return map[string]string{iyuu.ConfigKey: string(data)}, err
		})
		if err != nil {
			return err
		}
		data, err := json.Marshal(b.progress)
		if err != nil {
			return err
		}
		r, err := tx.ExecContext(ctx, "UPDATE GO_IYUU_BATCH SET progress=? WHERE slot=1 AND id=?", string(data), b.id)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil || n != 1 {
			return iyuu.ErrInput
		}
		return nil
	})
}
