package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/torrentmark"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

func markReceiptKey(id string) string { return "GoTorrentMarkRun." + id }

type markTransport func(*http.Request) (*http.Response, error)

func (f markTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func torrentLabelGuard(id int64, hash string) string {
	return organization.Digest([]string{"torrent-labels", strconv.FormatInt(id, 10), strings.ToLower(hash)})
}
func markSchedule(s markScope, now time.Time) (time.Time, error) {
	if s.settings.Cron == "" {
		return time.Time{}, nil
	}
	return nextSyncTimer(organization.MonitorTimer{Cron: s.settings.Cron, Timezone: s.location.String()}, now)
}
func (api *torrentMarkAPI) acquire(ctx context.Context, key string) (*organization.JobGuard, error) {
	if api.login.guards == nil || *api.login.guards == nil {
		return nil, torrentmark.ErrEvidence
	}
	return (*api.login.guards).AcquireJob(ctx, key)
}
func (api *torrentMarkAPI) valid(ctx context.Context, s markScope, control markControl, runID string) error {
	if !api.login.pureGo || ctx.Err() != nil {
		return torrentmark.ErrEvidence
	}
	current, err := api.scope(ctx, s.actor)
	if err != nil || !current.installed || current.fingerprint() != s.fingerprint() {
		return torrentmark.ErrEvidence
	}
	st, err := api.state(ctx)
	if err != nil || st.Control != control {
		return torrentmark.ErrEvidence
	}
	if runID != "" {
		for _, r := range st.Runs {
			if r.ID == runID && r.State == "running" {
				return nil
			}
		}
		return torrentmark.ErrEvidence
	}
	return nil
}
func (api *torrentMarkAPI) client(item downloaderconfig.Downloader, check func(context.Context) error) (markClient, error) {
	var cfg map[string]any
	if json.Unmarshal([]byte(item.Config), &cfg) != nil {
		return markClient{}, torrentmark.ErrEvidence
	}
	transport := markTransport(func(r *http.Request) (*http.Response, error) {
		if err := check(r.Context()); err != nil {
			return nil, err
		}
		base := api.login.transport
		if base == nil {
			base = http.DefaultTransport
		}
		return base.RoundTrip(r)
	})
	switch item.Type {
	case "qbittorrent":
		c, err := qbittorrent.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), transport)
		if err != nil {
			return markClient{}, err
		}
		return markClient{c.CompletedIDs, c.MarkEvidence, c.SetMark}, nil
	case "transmission":
		c, err := transmission.New(text(cfg["host"]), text(cfg["port"]), text(cfg["username"]), text(cfg["password"]), transport)
		if err != nil {
			return markClient{}, err
		}
		return markClient{c.CompletedIDs, c.MarkEvidence, c.SetMark}, nil
	}
	return markClient{}, torrentmark.ErrEvidence
}
func markCurrent(values map[string]string, s markScope) bool {
	return values[markConfigKey] == s.raw && values["UserInstalledPlugins"] == s.installedRaw
}
func markRunIndex(st *markState, id string) int {
	for i, r := range st.Runs {
		if r.ID == id {
			return i
		}
	}
	return -1
}
func (api *torrentMarkAPI) execute(parent context.Context, s markScope, id, kind string, exclusive bool, epoch, slot string) (markRun, error) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	guard, err := api.acquire(ctx, "native-torrent-mark")
	if err != nil {
		return markRun{}, err
	}
	defer guard.Close()
	if !s.settings.Enable || !s.installed || len(s.targets) == 0 || !api.login.pureGo {
		return markRun{}, torrentmark.ErrEvidence
	}
	for _, d := range s.targets {
		if d.Type == "transmission" && !exclusive {
			return markRun{}, torrentmark.ErrEvidence
		}
	}
	fresh, err := api.scope(ctx, s.actor)
	if err != nil || fresh.fingerprint() != s.fingerprint() {
		return markRun{}, torrentmark.ErrEvidence
	}
	run := markRun{ID: id, Kind: kind, State: "running", Created: time.Now().UTC().Format(time.RFC3339Nano)}
	var control markControl
	exists := false
	err = api.update(ctx, func(values map[string]string, st *markState) error {
		if i := markRunIndex(st, id); i >= 0 {
			run = st.Runs[i]
			exists = true
			return nil
		}
		if kind == "manual" && values[markReceiptKey(id)] != "" {
			run.State = "archived"
			exists = true
			return nil
		}
		if !markCurrent(values, s) {
			return torrentmark.ErrEvidence
		}
		if kind != "manual" {
			if st.Control.Epoch != epoch || st.Control.Scope != s.fingerprint() {
				return torrentmark.ErrEvidence
			}
			if kind == "once" {
				if st.Control.Once != slot {
					return torrentmark.ErrEvidence
				}
				st.Control.Once = ""
			} else {
				if st.Control.Next != slot {
					return torrentmark.ErrEvidence
				}
				next, err := markSchedule(s, time.Now())
				if err != nil || next.IsZero() {
					return torrentmark.ErrEvidence
				}
				st.Control.Next = next.UTC().Format(time.RFC3339Nano)
			}
		}
		// A held process lock proves any earlier running record has lost its executor.
		for i := range st.Runs {
			if st.Runs[i].State == "running" {
				st.Runs[i].State = "unknown"
			}
		}
		control = st.Control
		st.Runs = append(st.Runs, run)
		if len(st.Runs) > 30 {
			st.Runs = st.Runs[len(st.Runs)-30:]
		}
		if kind == "manual" {
			values[markReceiptKey(id)] = "accepted"
		}
		return nil
	}, markReceiptKey(id))
	if err != nil || exists {
		return run, err
	}
	check := func(ctx context.Context) error { return api.valid(ctx, s, control, id) }
	failed := false
	for _, item := range s.targets {
		if check(ctx) != nil {
			failed = true
			break
		}
		c, err := api.client(item, check)
		if err != nil {
			failed = true
			continue
		}
		ids, err := c.ids(ctx)
		if err != nil {
			failed = true
			continue
		}
		for _, hash := range ids {
			if check(ctx) != nil {
				failed = true
				break
			}
			if err := api.markOne(ctx, s, control, run.ID, item, c, hash, &run); err != nil {
				failed = true
			}
		}
	}
	run.State = "complete"
	if failed {
		run.State = "partial"
	}
	if run.Unknown > 0 {
		run.State = "unknown"
	}
	book, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	err = api.update(book, func(_ map[string]string, st *markState) error {
		i := markRunIndex(st, id)
		if i < 0 {
			return torrentmark.ErrEvidence
		}
		if st.Runs[i].State != "running" {
			run = st.Runs[i]
			return nil
		}
		st.Runs[i] = run
		return nil
	})
	return run, err
}
func (api *torrentMarkAPI) markOne(ctx context.Context, s markScope, control markControl, runID string, item downloaderconfig.Downloader, c markClient, hash string, run *markRun) error {
	lock, err := api.acquire(ctx, torrentLabelGuard(item.ID, hash))
	if err != nil {
		run.Skipped++
		return err
	}
	defer lock.Close()
	before, err := c.read(ctx, hash)
	if err != nil {
		run.Skipped++
		return nil
	}
	want := torrentmark.Classify(before)
	if want == "" {
		run.Skipped++
		return nil
	}
	after := torrentmark.LabelsAfter(before.Labels, want)
	if _, err := torrentmark.NormalizeLabels(after); err != nil {
		run.Skipped++
		return nil
	}
	pendingID := organization.Digest([]any{"mark-intent", item.ID, before.Hash, before.Added})
	st, err := api.state(ctx)
	if err != nil {
		return err
	}
	for _, p := range st.Pending {
		if p.ID == pendingID {
			run.Unknown++
			return nil
		}
	}
	if slices.Equal(before.Labels, after) {
		run.Skipped++
		return nil
	}
	fresh, err := c.read(ctx, hash)
	if err != nil || organization.Digest(fresh) != organization.Digest(before) {
		run.Skipped++
		return nil
	}
	p := markStoredPending{markPending: markPending{ID: pendingID, Downloader: strconv.FormatInt(item.ID, 10), Hash: before.Hash, Added: before.Added, Goal: want}, ConfigDigest: organization.Digest(item), LabelsDigest: organization.Digest(after)}
	err = api.update(ctx, func(values map[string]string, st *markState) error {
		i := markRunIndex(st, runID)
		if !markCurrent(values, s) || st.Control != control || i < 0 || st.Runs[i].State != "running" || len(st.Pending) >= 500 {
			return torrentmark.ErrEvidence
		}
		for _, old := range st.Pending {
			if old.ID == p.ID {
				return torrentmark.ErrEvidence
			}
		}
		st.Pending = append(st.Pending, p)
		return nil
	})
	if err != nil {
		return err
	}
	// The persisted intent is never automatically replayed, including after restart.
	if c.write(ctx, before, want) != nil {
		run.Unknown++
		return nil
	}
	observed, err := c.read(ctx, hash)
	if err != nil || observed.Added != p.Added || torrentmark.Classify(observed) != p.Goal || organization.Digest(observed.Labels) != p.LabelsDigest {
		run.Unknown++
		return nil
	}
	if err := api.clearPending(ctx, s, control, p); err != nil {
		run.Unknown++
		return nil
	}
	run.Changed++
	return nil
}
func (api *torrentMarkAPI) clearPending(ctx context.Context, s markScope, control markControl, p markStoredPending) error {
	return api.update(ctx, func(values map[string]string, st *markState) error {
		if !markCurrent(values, s) || st.Control != control {
			return torrentmark.ErrEvidence
		}
		st.Pending = slices.DeleteFunc(st.Pending, func(old markStoredPending) bool { return old == p })
		return nil
	})
}
func (api *torrentMarkAPI) reconcile(ctx context.Context, s markScope) error {
	guard, err := api.acquire(ctx, "native-torrent-mark")
	if err != nil {
		return err
	}
	defer guard.Close()
	st, err := api.state(ctx)
	if err != nil {
		return err
	}
	check := func(ctx context.Context) error { return api.valid(ctx, s, st.Control, "") }
	for _, p := range st.Pending {
		for _, item := range s.targets {
			if strconv.FormatInt(item.ID, 10) != p.Downloader || organization.Digest(item) != p.ConfigDigest {
				continue
			}
			lock, err := api.acquire(ctx, torrentLabelGuard(item.ID, p.Hash))
			if err != nil {
				return err
			}
			c, err := api.client(item, check)
			if err == nil {
				observed, e := c.read(ctx, p.Hash)
				if e == nil && observed.Added == p.Added && torrentmark.Classify(observed) == p.Goal && organization.Digest(observed.Labels) == p.LabelsDigest {
					err = api.clearPending(ctx, s, st.Control, p)
				}
			}
			lock.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
func (api *torrentMarkAPI) tick(ctx context.Context, now time.Time) {
	if guard, err := api.acquire(ctx, "native-torrent-mark"); err == nil {
		if st, err := api.state(ctx); err == nil && slices.ContainsFunc(st.Runs, func(r markRun) bool { return r.State == "running" }) {
			_ = api.update(ctx, func(_ map[string]string, st *markState) error {
				for i := range st.Runs {
					if st.Runs[i].State == "running" {
						st.Runs[i].State = "unknown"
					}
				}
				return nil
			})
		}
		guard.Close()
	}
	st, err := api.state(ctx)
	if err != nil || st.Control.Actor == "" {
		return
	}
	s, err := api.scope(ctx, st.Control.Actor)
	if err != nil || !s.installed || st.Control.Scope != s.fingerprint() {
		return
	}
	for _, kind := range []string{"once", "cron"} {
		slot := st.Control.Next
		if kind == "once" {
			slot = st.Control.Once
		}
		at, err := time.Parse(time.RFC3339Nano, slot)
		if err != nil || at.After(now) {
			continue
		}
		id := organization.Digest([]string{"mark-slot", st.Control.Epoch, kind, slot})
		_, _ = api.execute(ctx, s, id, kind, st.Control.Exclusive, st.Control.Epoch, slot)
		return
	}
}
func startTorrentMarkWorker(ctx context.Context, api *torrentMarkAPI) func() {
	if api == nil || api.system == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		api.tick(ctx, time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				api.tick(ctx, now)
			}
		}
	}()
	return func() { <-done }
}
