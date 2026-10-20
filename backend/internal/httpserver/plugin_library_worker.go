package httpserver

import (
	"context"
	"errors"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func (api *libraryScraper) pluginOriginCurrent(ctx context.Context, origin *organization.LibraryScheduleOrigin) error {
	raw, err := api.intake.system.ReadMany(ctx, libraryPluginKeys)
	if err != nil {
		return err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], libraryPluginID)
	if err != nil || !installed {
		return organization.ErrState
	}
	c, err := decodeLibraryControl(raw[libraryControlKey])
	if err != nil || c.Epoch != origin.Epoch {
		return organization.ErrState
	}
	if origin.Event {
		return nil
	}
	if !c.Authorized || c.State != "queued" || c.Batch != origin.Batch {
		return organization.ErrState
	}
	values, err := decodeMetadataConfig(raw[libraryPluginKey])
	if err != nil || organization.Digest(values) != c.Lease.PluginDigest {
		return organization.ErrState
	}
	return nil
}
func libraryRun(c libraryPluginControl, state, reason string, now time.Time) organization.LibraryPluginRun {
	return organization.LibraryPluginRun{ID: c.Batch, Epoch: c.Epoch, State: state, Reason: reason, Created: c.Created, Updated: now.UTC().Format(time.RFC3339Nano), Jobs: append([]string{}, c.Jobs...), Empty: c.Empty}
}
func (api *libraryScraper) pluginCommit(ctx context.Context, raw map[string]string, c libraryPluginControl, run *organization.LibraryPluginRun, plans []organization.LibraryPrepared, revoke string) error {
	encoded, err := libraryControlJSON(c)
	if err != nil {
		return err
	}
	return api.intake.organizer.jobs.LibraryPluginMutation(ctx, raw, map[string]string{libraryControlKey: encoded}, revoke, run, plans)
}
func (api *libraryScraper) pluginPause(ctx context.Context, raw map[string]string, c libraryPluginControl, reason string, now time.Time) error {
	var run *organization.LibraryPluginRun
	if c.Batch != "" && (c.State == "scanning" || c.State == "queued") {
		r := libraryRun(c, "paused", reason, now)
		run = &r
	}
	c.Authorized, c.State, c.Reason = false, "paused", reason
	c.OnceAt, c.Next = time.Time{}, time.Time{}
	return api.pluginCommit(ctx, raw, c, run, nil, c.Epoch)
}
func (api *libraryScraper) pluginTick(ctx context.Context, now time.Time) error {
	if api == nil || api.intake == nil || api.intake.system == nil || api.intake.organizer == nil || !api.intake.organizer.pureGo || api.intake.organizer.jobs == nil {
		return nil
	}
	store := api.intake.organizer.jobs
	guard, err := store.AcquireJob(ctx, "library-scraper-scheduler")
	if err != nil {
		return err
	}
	defer guard.Close()
	raw, err := api.intake.system.ReadMany(ctx, libraryPluginKeys)
	if err != nil {
		return err
	}
	installed, err := indexerInstalled(raw["UserInstalledPlugins"], libraryPluginID)
	if err != nil || !installed {
		return err
	}
	c, err := decodeLibraryControl(raw[libraryControlKey])
	if err != nil || !c.Authorized {
		return err
	}
	values, err := decodeMetadataConfig(raw[libraryPluginKey])
	if err != nil {
		return api.pluginPause(ctx, raw, c, "plugin configuration invalid; explicit new review required", now)
	}
	lease, err := api.pluginLease(ctx, values, c.Lease.Actor)
	if err != nil || organization.Digest(lease) != organization.Digest(c.Lease) {
		return api.pluginPause(ctx, raw, c, "configuration, options, roots or authority changed; explicit new review required", now)
	}
	if c.State == "scanning" {
		return api.pluginPause(ctx, raw, c, "previous scan was interrupted; no scan or write automatically replayed", now)
	}
	if c.State == "queued" {
		publication, err := store.AcquireJob(ctx, "library-scraper-publication")
		if errors.Is(err, organization.ErrBusy) {
			return nil
		}
		if err != nil {
			return err
		}
		pending, interrupted, partial := false, false, false
		for _, id := range c.Jobs {
			job, err := store.LibraryJob(ctx, id)
			if err != nil {
				publication.Close()
				return err
			}
			switch job.State {
			case "ready":
				pending = true
			case "running", "needs_review":
				interrupted = true
			case "partial", "cancelled":
				partial = true
			case "completed":
			default:
				interrupted = true
			}
		}
		uncertain, err := store.LibraryPluginUncertain(ctx, c.Jobs)
		publication.Close()
		if err != nil {
			return err
		}
		if interrupted || uncertain {
			return api.pluginPause(ctx, raw, c, "previous batch has interrupted or uncertain outputs; inspect receipts before authorizing another cycle", now)
		}
		if pending {
			return nil
		}
		state, reason := "completed", "all selected media receipts completed"
		if partial {
			state, reason = "partial", "one or more files blocked, incomplete or explicitly cancelled; inspect file receipts"
		}
		run := libraryRun(c, state, reason, now)
		c.State, c.Reason = "scheduled", reason
		if c.Next.IsZero() && c.OnceAt.IsZero() {
			c.State = "idle"
		}
		if err := api.pluginCommit(ctx, raw, c, &run, nil, ""); err != nil {
			return err
		}
		raw[libraryControlKey], _ = libraryControlJSON(c)
	}
	once := !c.OnceAt.IsZero() && !c.OnceAt.After(now)
	period := !c.Next.IsZero() && !c.Next.After(now)
	if !once && !period {
		return nil
	}
	cfg, err := parseLibraryPlugin(values)
	if err != nil {
		return err
	}
	if once {
		c.OnceAt = time.Time{}
	}
	if period {
		zone, err := rssWorkerLocation()
		if err != nil {
			return err
		}
		schedule, err := parseRSSSchedule(cfg.Cron, zone)
		if err != nil {
			return err
		}
		c.Next = schedule.next(now).UTC()
		if c.Next.IsZero() {
			return api.pluginPause(ctx, raw, c, "no future cron occurrence; explicit configuration required", now)
		}
	}
	c.Batch, err = newIndexerClaim()
	if err != nil {
		return err
	}
	c.Created = now.UTC().Format(time.RFC3339Nano)
	c.State, c.Reason, c.Jobs = "scanning", "", []string{}
	c.Empty = 0
	run := libraryRun(c, "scanning", "durable scan intent; no files written", now)
	if err := api.pluginCommit(ctx, raw, c, &run, nil, ""); err != nil {
		return err
	}
	raw[libraryControlKey], _ = libraryControlJSON(c)
	scanCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	plans := []organization.LibraryPrepared{}
	queuedIDs := []string{}
	count := 0
	for _, template := range c.Lease.Templates {
		if scanCtx.Err() != nil {
			err = scanCtx.Err()
			break
		}
		d := template
		d.RequestID = organization.Digest([]string{c.Epoch, c.Batch, d.Root.ID})[:32]
		d.Schedule = &organization.LibraryScheduleOrigin{Epoch: c.Epoch, Batch: c.Batch}
		var scan organization.LibraryScan
		scan, err = organization.ScanLibrary(scanCtx, d)
		if err != nil {
			break
		}
		count += len(scan.Files)
		if count > 50000 {
			err = organization.ErrLimit
			break
		}
		if len(scan.Files) == 0 {
			run.Empty++
			c.Empty++
			continue
		}
		d.Count = len(scan.Files)
		plans = append(plans, organization.LibraryPrepared{Fingerprint: organization.Digest([]any{d, scan.Files}), Definition: d, Files: scan.Files})
		queuedIDs = append(queuedIDs, organization.Digest([]string{d.Actor, d.RequestID}))
	}
	if err == nil {
		fresh, e := api.pluginLease(scanCtx, values, c.Lease.Actor)
		if e != nil || organization.Digest(fresh) != organization.Digest(c.Lease) {
			err = organization.ErrState
		}
	}
	if err != nil {
		persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if scanCtx.Err() != nil || errors.Is(err, organization.ErrState) {
			return api.pluginPause(persist, raw, c, "scan interrupted or scope changed; no jobs published, explicit review required", now)
		}
		c.State, c.Reason, c.Jobs = "scheduled", "scan failed or exceeded limits; no jobs published; next occurrence is a new scan", []string{}
		if c.Next.IsZero() && c.OnceAt.IsZero() {
			c.State = "idle"
		}
		run.State, run.Reason, run.Jobs = "failed", c.Reason, []string{}
		return api.pluginCommit(persist, raw, c, &run, nil, "")
	}
	c.State, c.Reason = "queued", "frozen media worklists queued; no automatic replay after execution starts"
	c.Jobs = queuedIDs
	run.State, run.Reason, run.Jobs = "queued", c.Reason, c.Jobs
	if len(plans) == 0 {
		run.State, run.Reason = "completed", "selected libraries contained no eligible media"
		c.State, c.Reason = "scheduled", run.Reason
		if c.Next.IsZero() && c.OnceAt.IsZero() {
			c.State = "idle"
		}
	}
	return api.pluginCommit(ctx, raw, c, &run, plans, "")
}
