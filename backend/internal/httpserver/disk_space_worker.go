package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

var errDiskCache = errors.New("disk space cache unavailable")

func startDiskSpace(ctx context.Context, api *diskSpaceAPI) func() {
	if api == nil {
		return func() {}
	}
	return startOneShotWorker(ctx, "磁盘空间释放", api.tick)
}
func startOneShotWorker(ctx context.Context, name string, tick func(context.Context, time.Time) error) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := tick(ctx, time.Now()); err != nil && ctx.Err() == nil {
				slog.Warn(name+"任务暂未完成", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { <-done }
}
func (api *diskSpaceAPI) prepare(ctx context.Context, s diskRun, c diskOptions, current func() error) error {
	var exists bool
	err := api.store.Transaction(ctx, func(tx *sql.Tx) error {
		if err := diskTables(ctx, tx); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM GO_DISK_SPACE_RUNS WHERE id=?)", s.ID).Scan(&exists)
	})
	if err != nil || exists {
		return err
	}
	cache, err := organization.LoadDiskCache(api.configFile, c.Legacy)
	if err != nil {
		return errors.Join(errDiskCache, err)
	}
	entries := map[string]organization.DiskCacheRow{}
	for _, v := range cache.Rows {
		entries[v.Path] = v
	}
	exts, minimum, err := diskFilters(c)
	if err != nil {
		return err
	}
	var pairs []organization.DiskPair
	files, linked, failed := 0, 0, len(s.Unavailable)
	seen := map[string]bool{}
	for _, root := range s.Roots {
		if err := current(); err != nil {
			return err
		}
		scan, err := organization.ScanDiskDuplicates(ctx, root, exts, minimum, c.Fast, c.Dry, entries)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failed++
			continue
		}
		files += scan.Files
		linked += scan.ExistingLinks
		failed += scan.Failed
		for _, pair := range scan.Pairs {
			key := filepath.Join(pair.Root.Path, pair.Duplicate.Path)
			if !seen[key] {
				pairs = append(pairs, pair)
				seen[key] = true
			}
		}
	}
	if err := current(); err != nil {
		return err
	}
	if err := organization.SaveDiskCache(ctx, api.configFile, cache, entries); err != nil {
		return errors.Join(errDiskCache, err)
	}
	state := "pending"
	if c.Dry {
		state = "candidate"
	}
	return api.store.Transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO GO_DISK_SPACE_RUNS VALUES(?,?,?,?)", s.ID, files, linked, failed); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO GO_DISK_SPACE_PAIRS(job,seq,pair,state) VALUES(?,?,?,?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i, p := range pairs {
			b, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if _, err = stmt.ExecContext(ctx, s.ID, i, string(b), state); err != nil {
				return err
			}
		}
		return nil
	})
}
func (api *diskSpaceAPI) execute(ctx context.Context, s diskRun, c diskOptions, current func() error) (string, error) {
	if err := api.prepare(ctx, s, c, current); err != nil {
		return "", err
	}
	for {
		if err := current(); err != nil {
			return "", err
		}
		var seq int
		var raw, hold string
		err := api.store.Transaction(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, "SELECT seq,pair,hold FROM GO_DISK_SPACE_PAIRS WHERE job=? AND state='pending' ORDER BY seq LIMIT 1", s.ID).Scan(&seq, &raw, &hold)
		})
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return "", err
		}
		var pair organization.DiskPair
		if err := json.Unmarshal([]byte(raw), &pair); err != nil {
			return "", err
		}
		record := func(id string) error {
			return api.store.Transaction(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, "UPDATE GO_DISK_SPACE_PAIRS SET hold=?,stage=? WHERE job=? AND seq=? AND state='pending' AND hold=''", id, organization.DiskStagePath(pair, s.ID, seq), s.ID, seq)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil || n != 1 {
					return organization.ErrState
				}
				return nil
			})
		}
		linked, err := organization.ReplaceDiskDuplicate(ctx, pair, s.ID, seq, hold, record, current)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if e := current(); e != nil {
			return "", e
		}
		state := "different"
		if err != nil {
			state = "failed"
		} else if linked {
			state = "linked"
		}
		err = api.store.Transaction(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE GO_DISK_SPACE_PAIRS SET state=? WHERE job=? AND seq=? AND state='pending'", state, s.ID, seq)
			return err
		})
		if err != nil {
			return "", err
		}
	}
	var files, linked, failed, candidates, replaced, mismatches, errorsCount int
	err := api.store.Transaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT files,linked,failed,
 (SELECT COUNT(*) FROM GO_DISK_SPACE_PAIRS WHERE job=r.id),
 (SELECT COUNT(*) FROM GO_DISK_SPACE_PAIRS WHERE job=r.id AND state='linked'),
 (SELECT COUNT(*) FROM GO_DISK_SPACE_PAIRS WHERE job=r.id AND state='different'),
 (SELECT COUNT(*) FROM GO_DISK_SPACE_PAIRS WHERE job=r.id AND state='failed')
 FROM GO_DISK_SPACE_RUNS r WHERE id=?`, s.ID).Scan(&files, &linked, &failed, &candidates, &replaced, &mismatches, &errorsCount)
	})
	last := fmt.Sprintf("已扫描 %d 个文件，查重候选 %d 项，已替换 %d 项，已有硬链接 %d 项，内容差异 %d 项，失败 %d 项。", files, candidates, replaced, linked, mismatches, failed+errorsCount)
	if len(s.Unavailable) > 0 {
		last += " 无法访问的目录：" + strings.Join(s.Unavailable, "、")
	}
	return last, err
}
