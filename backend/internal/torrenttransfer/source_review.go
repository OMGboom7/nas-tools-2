package torrenttransfer

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
)

type SourceClient interface {
	Address() string
	CompletedIDs(context.Context) ([]string, error)
	TransferTask(context.Context, string) (Task, error)
}

// SourceReview binds the exact local metainfo and remote source incarnation.
// Only the currently yielded item needs to keep torrent bytes in memory.
type SourceReview struct {
	Task           Task
	Source         Source
	Directory      string
	Endpoint       string
	SettingsDigest string
}

func eligibleSource(s Settings, task Task) (bool, error) {
	if !task.Valid() {
		return false, ErrState
	}
	if !task.Complete || task.Error || task.Kind == "transmission" && !task.SourceSeeding {
		return false, nil
	}
	excluded, err := s.Excluded(task.Kind, task.Directory, task.RawQbTags, task.Labels)
	return !excluded, err
}

// ScanSources yields errors with the affected wire ID where available, so a
// per-task failure need not prevent the controller from reviewing later items.
func ScanSources(ctx context.Context, s Settings, c SourceClient, root *MetainfoRoot) iter.Seq2[SourceReview, error] {
	return func(yield func(SourceReview, error) bool) {
		if s.Ready() != nil || c == nil || c.Address() == "" || root == nil || root.configured != filepath.Clean(s.TorrentPath) || root.check() != nil {
			yield(SourceReview{}, ErrState)
			return
		}
		ids, err := c.CompletedIDs(ctx)
		if err != nil {
			yield(SourceReview{}, err)
			return
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				yield(SourceReview{}, err)
				return
			}
			review, eligible, err := prepareSource(ctx, s, c, root, id)
			if errors.Is(err, ErrTaskAbsent) {
				continue
			}
			if (eligible || err != nil) && !yield(review, err) {
				return
			}
		}
	}
}

func prepareSource(ctx context.Context, s Settings, c SourceClient, root *MetainfoRoot, id string) (SourceReview, bool, error) {
	r := SourceReview{Task: Task{WireID: id}}
	task, err := c.TransferTask(ctx, id)
	if err != nil {
		return r, false, err
	}
	if task.WireID != id {
		return r, false, ErrState
	}
	ok, err := eligibleSource(s, task)
	if !ok || err != nil {
		return r, false, err
	}
	dir, err := MapDirectory(task.Directory, s.FromPath, s.ToPath)
	if err != nil {
		return r, false, err
	}
	src, err := root.ReadTask(ctx, task, task.Kind == "qbittorrent")
	if err != nil {
		return r, false, err
	}
	if !task.FullComplete(src.Prepared) {
		return r, false, ErrState
	}
	return SourceReview{Task: task, Source: src, Directory: dir, Endpoint: Hash(c.Address()), SettingsDigest: Hash(s)}, true, nil
}

// Revalidate reads the source again immediately before an action. The caller
// supplies the current settings and the client resolved from those settings.
func (r SourceReview) Revalidate(ctx context.Context, s Settings, c SourceClient, root *MetainfoRoot) error {
	if s.Ready() != nil || Hash(s) != r.SettingsDigest || c == nil || Hash(c.Address()) != r.Endpoint || root == nil || root.configured != filepath.Clean(s.TorrentPath) {
		return ErrState
	}
	current, ok, err := prepareSource(ctx, s, c, root, r.Task.WireID)
	if err != nil {
		return err
	}
	if !ok || !r.Task.SameInstance(current.Task) || r.Directory != current.Directory || Hash(r.Source) != Hash(current.Source) || !equalContent(r.Source.Prepared.Content, current.Source.Prepared.Content) {
		return ErrState
	}
	return nil
}
