package httpserver

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
)

func (s downloadService) classicSelectionState(ctx context.Context, item downloaderconfig.Downloader, id, marker string, h downloaderconfig.ClassicAcknowledgement) (torrentfiles.Snapshot, error) {
	var st torrentfiles.Snapshot
	var err error
	if item.Type == "qbittorrent" {
		c, e := s.qbProfileClient(item)
		if e != nil {
			return st, e
		}
		st, err = c.Selection(ctx, id)
	} else {
		c, e := s.profileClient(item)
		if e != nil {
			return st, e
		}
		st, err = c.Selection(ctx, id)
	}
	if err != nil || st.Hash != id || !slices.Contains(st.Labels, marker) || h.Directory != "" && strings.TrimRight(st.Directory, "/") != strings.TrimRight(h.Directory, "/") || !torrentfiles.Match(h.Files, st.Files) {
		return st, downloaderconfig.ErrFileSelectionPending
	}
	return st, nil
}

// Both clients add paused. Only read operations are polled. An uncertain
// priority change or start leaves the original receipt pending.
func (s downloadService) applyClassicSelection(ctx context.Context, item downloaderconfig.Downloader, id, marker string, startHere bool) error {
	h := s.classic.history
	poll := func(check func(torrentfiles.Snapshot) bool) (torrentfiles.Snapshot, error) {
		wait, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		timer := time.NewTicker(200 * time.Millisecond)
		defer timer.Stop()
		for {
			st, err := s.classicSelectionState(wait, item, id, marker, h)
			if err == nil && check(st) {
				return st, nil
			}
			select {
			case <-wait.Done():
				return st, downloaderconfig.ErrFileSelectionPending
			case <-timer.C:
			}
		}
	}
	st, err := poll(func(st torrentfiles.Snapshot) bool { return st.Paused })
	if err != nil {
		return err
	}
	desired := selectedRemoteFiles(h.Files, st)
	wanted, unwanted := torrentfiles.IDs(desired, true), torrentfiles.IDs(desired, false)
	if len(wanted) == 0 {
		return downloaderconfig.ErrFileSelectionPending
	}
	if !torrentfiles.Wanted(h.Files, st.Files) {
		if item.Type == "qbittorrent" {
			c, e := s.qbProfileClient(item)
			if e != nil {
				return e
			}
			if len(unwanted) > 0 && c.SetFilePriority(ctx, id, unwanted, false) != nil {
				return downloaderconfig.ErrFileSelectionPending
			}
			// Re-map against a fresh snapshot before the next indexed write.
			st, err = poll(func(st torrentfiles.Snapshot) bool {
				return st.Paused && unwantedMatch(h.Files, st)
			})
			if err != nil {
				return err
			}
			if !torrentfiles.Wanted(h.Files, st.Files) {
				wanted = torrentfiles.IDs(selectedRemoteFiles(h.Files, st), true)
				if c.SetFilePriority(ctx, id, wanted, true) != nil {
					return downloaderconfig.ErrFileSelectionPending
				}
			}
		} else {
			c, e := s.profileClient(item)
			if e != nil {
				return e
			}
			if c.SelectFiles(ctx, id, wanted, unwanted) != nil {
				return downloaderconfig.ErrFileSelectionPending
			}
		}
	}
	if _, err = poll(func(st torrentfiles.Snapshot) bool { return st.Paused && torrentfiles.Wanted(h.Files, st.Files) }); err != nil {
		return err
	}
	if startHere && !h.Paused {
		if s.startSelectedFiles(ctx, item, id) != nil {
			return downloaderconfig.ErrFileSelectionPending
		}
		_, err = poll(func(st torrentfiles.Snapshot) bool { return !st.Paused && torrentfiles.Wanted(h.Files, st.Files) })
	}
	return err
}
