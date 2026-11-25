package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediaarchive"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

func startMediaArchiveWorker(ctx context.Context, s *pluginService) func() {
	if s == nil || !s.nativeArchiveRunner(mediaarchive.Plugin) {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			if err := s.archiveTick(ctx, time.Now()); err != nil && ctx.Err() == nil {
				slog.Warn("media archive worker could not complete its check")
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return func() { <-done }
}

// Changes to other plugins' installations are independent of an archive run.
func archiveRunCurrent(raw, actual map[string]string) bool {
	installed, err := indexerInstalled(actual["UserInstalledPlugins"], mediaarchive.Plugin)
	return err == nil && installed && raw[archiveSettingsKey] == actual[archiveSettingsKey] && raw[archiveControlKey] == actual[archiveControlKey]
}

func (s pluginService) archiveTick(ctx context.Context, now time.Time) error {
	raw, err := s.system.ReadMany(ctx, archiveKeys)
	if err != nil {
		return err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], mediaarchive.Plugin)
	if err != nil || !installed {
		return err
	}
	c, err := decodeArchiveControl(raw[archiveControlKey])
	if err != nil || c.Actor == "" {
		return err
	}
	due := func(t time.Time) bool { return !t.IsZero() && !t.After(now) }
	if !due(c.Once) && !due(c.Next) && !c.Running {
		return nil
	}
	guard, err := s.archiveMedia.guards.AcquireJob(ctx, "media-library-archive")
	if errors.Is(err, organization.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	defer guard.Close()
	commit := func(c archiveControl) error {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if err = s.system.UpdateMany(ctx, archiveKeys, func(actual map[string]string) (map[string]string, error) {
			if !archiveRunCurrent(raw, actual) {
				return nil, mediaarchive.ErrChanged
			}
			return map[string]string{archiveControlKey: string(b)}, nil
		}); err != nil {
			return err
		}
		raw[archiveControlKey] = string(b)
		return nil
	}
	if c.Running {
		return commit(archiveControl{Last: "上次归档中断，请核对文件并重新保存运行设置。"})
	}
	source, err := s.archiveSource(c.Actor, raw[archiveSettingsKey])
	if err != nil || source.scope != c.Scope {
		return commit(archiveControl{Last: "媒体服务或归档授权已变更，请重新保存运行设置。"})
	}
	cfg, keep, err := mediaarchive.Parse(raw[archiveSettingsKey])
	if err != nil {
		return err
	}
	loc := source.location
	kind, slot := "cron", c.Next
	if due(c.Once) {
		kind, slot = "once", c.Once
		c.Once = time.Time{}
	} else {
		c.Next, err = cookieCloudSchedule(cfg.Cron, loc, now)
		if err != nil {
			return err
		}
		c.Next = c.Next.UTC()
	}
	id := sitelogin.Hash([]any{"media-library-archive", c.Scope, kind, slot})
	c.Running = true
	c.Last = "正在生成媒体库归档。"
	if err = commit(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	current := func() error {
		latest, err := s.system.ReadMany(ctx, archiveKeys)
		if err != nil {
			return err
		}
		source, err := s.archiveSource(c.Actor, raw[archiveSettingsKey])
		if err != nil || source.scope != c.Scope || !archiveRunCurrent(raw, latest) {
			return mediaarchive.ErrChanged
		}
		return ctx.Err()
	}
	name := "归档_" + now.In(loc).Format("20060102150405") + ".md"
	result, err := mediaarchive.Generate(ctx, s.archiveConfigPath, cfg.Path, s.archiveContainer, name, keep, func(w io.Writer) (int, error) {
		return source.client.WriteArchive(ctx, source.user, w)
	}, current)
	c.Running = false
	switch {
	case err != nil && result.Name != "":
		c.Last = "已生成 " + result.Name + "，清理未完整结束，请核对归档目录。"
	case err != nil:
		c.Last = "归档失败，请核对媒体服务和归档目录。"
	case result.Name == "":
		c.Last = "媒体服务器没有可归档的媒体库。"
	default:
		c.Last = fmt.Sprintf("已生成 %s，共 %d 个媒体库，已清理 %d 份归档。", result.Name, result.Libraries, result.Removed)
	}
	notify := cfg.Notify && err == nil && result.Name != ""
	if !notify {
		return commit(c)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	dir := cfg.Path
	if s.archiveContainer || dir == "" {
		dir = filepath.Join(filepath.Dir(s.archiveConfigPath), "archive_file", "media_library")
	}
	return s.system.Transaction(ctx, func(tx *sql.Tx) error {
		err := systemconfig.UpdateManyTx(ctx, tx, archiveKeys, func(actual map[string]string) (map[string]string, error) {
			if !archiveRunCurrent(raw, actual) {
				return nil, mediaarchive.ErrChanged
			}
			return map[string]string{archiveControlKey: string(b)}, nil
		})
		if err != nil {
			return err
		}
		return bus.EmitData(ctx, tx, "plugin_message", id, id, "媒体库归档任务执行完成", fmt.Sprintf("归档文件: %s\n清理数量: %d", filepath.Join(dir, result.Name), result.Removed), map[string]string{"plugin": mediaarchive.Plugin})
	})
}
