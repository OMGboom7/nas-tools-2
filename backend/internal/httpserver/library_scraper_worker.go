package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
)

func (api *libraryScraper) identify(ctx context.Context, d organization.LibraryDefinition, m organization.LibraryMedia) (scraper.Input, error) {
	in := scraper.Input{Kind: d.Root.Kind, Stem: strings.TrimSuffix(filepath.Base(m.Path), filepath.Ext(m.Path))}
	auto := in.Kind == "auto"
	if auto {
		ep, err := mediameta.Episodes(in.Stem, filepath.Base(filepath.Dir(m.Path)))
		if err != nil {
			return in, err
		}
		in.Kind = "movie"
		if ep.TV {
			in.Kind = "tv"
		}
		d.Root.Kind = in.Kind
	}
	var episodes *mediameta.EpisodeInfo
	if in.Kind == "tv" {
		ep, err := mediameta.Episodes(in.Stem, "")
		if err != nil {
			return in, err
		}
		if ep.Season == nil {
			parent, e := mediameta.Episodes(filepath.Base(filepath.Dir(m.Path)), "")
			if e != nil || parent.EndSeason != nil {
				return in, organization.ErrState
			}
			ep.Season = parent.Season
		}
		// The folder scraper's documented legacy default is season one. This
		// applies only to its automatic classifier, with an explicit episode;
		// the scraper client still verifies every requested episode in TMDB.
		if auto && ep.Season == nil && ep.Episode != nil {
			season := 1
			ep.Season = &season
		}
		if ep.Season == nil || ep.Episode == nil || ep.EndSeason != nil {
			return in, organization.ErrState
		}
		ep.TV = true
		in.Season, in.First, in.Last = *ep.Season, *ep.Episode, *ep.Episode
		if ep.EndEpisode != nil {
			in.Last = *ep.EndEpisode
		}
		episodes = &ep
	}
	if d.Mode == "no_force" {
		candidates := []string{filepath.Join(filepath.Dir(m.Path), "movie.nfo"), strings.TrimSuffix(m.Path, filepath.Ext(m.Path)) + ".nfo"}
		if in.Kind == "tv" {
			target, err := organization.LibraryTarget(d, m, "show", "tvshow.nfo")
			if err != nil {
				return in, err
			}
			candidates = []string{target}
		}
		for _, name := range candidates {
			data, err := organization.LibraryReadNFO(ctx, d, m, name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return in, err
			}
			id, err := scraper.NFOIdentity(data, in.Kind)
			if err != nil {
				return in, err
			}
			if id != "" {
				in.ID = id
				break
			}
		}
	}
	title := in.Stem
	if in.Kind == "tv" && in.ID == "" {
		meta, err := mediameta.Parse(title, "")
		if err != nil {
			return in, err
		}
		if meta.Title == "" {
			folder := filepath.Dir(m.Path)
			parent, err := mediameta.Episodes(filepath.Base(folder), "")
			if err != nil {
				return in, err
			}
			if parent.Season != nil {
				folder = filepath.Dir(folder)
			}
			if folder == "." {
				return in, organization.ErrState
			}
			title = filepath.Base(folder)
		}
	}
	forced := in.Kind
	if auto && in.ID == "" {
		forced = ""
	}
	result, failure := api.intake.organizer.recognition.recognizeSelected(ctx, title, "", forced, in.ID, episodes, nil)
	if failure != nil || result == nil || !auto && result.kind != in.Kind {
		return in, organization.ErrState
	}
	if auto && result.kind != in.Kind {
		in.Kind = result.kind
		if in.Kind == "tv" {
			ep := result.meta.Episodes
			if ep.Season == nil || ep.Episode == nil || ep.EndSeason != nil {
				return in, organization.ErrState
			}
			in.Season, in.First, in.Last = *ep.Season, *ep.Episode, *ep.Episode
			if ep.EndEpisode != nil {
				in.Last = *ep.EndEpisode
			}
		}
	}
	in.ID = strconv.FormatInt(result.detail.ID, 10)
	return in, nil
}
func libraryArtifactOwner(in scraper.Input, a scraper.Artifact) string {
	group := "show"
	if a.Scope != "show" && a.Name != "season.nfo" {
		group = in.Stem
	}
	if a.Name == "season.nfo" || strings.HasPrefix(a.Name, "season") {
		group = "season:" + strconv.Itoa(in.Season)
	}
	return organization.Digest([]string{in.Kind, in.ID, group})
}
func (api *libraryScraper) saveOutput(ctx context.Context, job organization.LibraryJob, target, owner, state, reason string, p organization.LibraryOutputProof) error {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return api.intake.organizer.jobs.SaveLibraryOutput(persist, job.ID, job.Definition.Root, target, owner, state, reason, p)
}
func (api *libraryScraper) runMedia(ctx context.Context, job organization.LibraryJob, m organization.LibraryMedia) (string, string) {
	d := job.Definition
	store := api.intake.organizer.jobs
	if api.current(ctx, job) != nil || organization.VerifyLibraryMedia(ctx, d, m) != nil {
		return "blocked", "reviewed media, configuration or source is no longer current"
	}
	in, err := api.identify(ctx, d, m)
	if err != nil {
		return "blocked", "media identity or existing NFO could not be verified; no metadata written"
	}
	d.Root.Kind = in.Kind
	opts, warnings, err := scraper.Parse(d.Settings)
	if err != nil {
		return "blocked", "scraper options invalid"
	}
	snapshot, err := api.intake.organizer.service.configStore.Snapshot()
	if err != nil || organization.Digest(snapshot) != d.ConfigDigest {
		return "blocked", "application configuration changed"
	}
	client := api.intake.client(snapshot)
	plan, err := client.Build(ctx, in, opts, time.Now())
	if err != nil {
		return "blocked", "verified online metadata unavailable; no metadata written"
	}
	warnings = append(warnings, plan.Warnings...)
	if len(plan.Artifacts) == 0 {
		warnings = append(warnings, "no selected metadata could be generated")
	}
	for _, a := range plan.Artifacts {
		if api.current(ctx, job) != nil || organization.VerifyLibraryMedia(ctx, d, m) != nil {
			return "partial", "job stopped or reviewed media/configuration changed; remaining outputs not replayed"
		}
		target, err := organization.LibraryTarget(d, m, a.Scope, a.Name)
		if err != nil {
			warnings = append(warnings, "metadata path rejected")
			continue
		}
		owner := libraryArtifactOwner(in, a)
		previous, e := store.FindLibraryOutput(ctx, job.ID, target)
		if e == nil {
			valid := previous.Owner == owner
			if valid && previous.State == "preserved" {
				present, err := organization.LibraryExisting(ctx, d, m, a.Scope, a.Name)
				valid = err == nil && present
			} else if valid {
				actual, err := organization.ObserveLibrarySidecar(ctx, d, previous.Proof)
				valid = err == nil && actual == previous.State && (actual == "created" || actual == "replaced")
			}
			if !valid {
				warnings = append(warnings, "shared output identity conflicted, changed or requires review; not written again")
			}
			continue
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return "needs_review", "output journal unavailable; no further metadata written"
		}
		if !organization.LibraryOverwrite(d.Mode, a.Name) {
			present, err := organization.LibraryExisting(ctx, d, m, a.Scope, a.Name)
			if err != nil {
				warnings = append(warnings, "existing metadata check failed")
				continue
			}
			if present {
				if api.saveOutput(ctx, job, target, owner, "preserved", "existing metadata retained; its contents were not validated", organization.LibraryOutputProof{}) != nil {
					return "needs_review", "preserved output receipt unavailable"
				}
				continue
			}
		}
		data := a.Data
		if a.Thumbnail {
			render := api.intake.thumbnail
			if render == nil {
				render = scraper.RenderThumbnail
			}
			data, err = organization.ReadLibraryMedia(ctx, d, m, func(f *os.File) ([]byte, error) { return render(ctx, f) })
			if err == nil {
				err = scraper.ValidateImage(data, ".jpg")
			}
		} else if a.URL != "" {
			data, err = client.Download(ctx, a.URL)
		} else {
			err = nil
		}
		if err != nil {
			if api.saveOutput(ctx, job, target, owner, "blocked", "image unavailable or media changed; no image published", organization.LibraryOutputProof{}) != nil {
				return "needs_review", "image failure receipt unavailable"
			}
			warnings = append(warnings, "one or more selected images unavailable")
			continue
		}
		proof, state, publishErr := organization.PublishLibrarySidecar(ctx, d, m, a.Scope, a.Name, data, func(p organization.LibraryOutputProof) error {
			return store.SaveLibraryOutput(ctx, job.ID, d.Root, target, owner, "writing", "durable publication intent; outcome must be checked", p)
		}, func() error { return api.current(ctx, job) })
		reason := ""
		if publishErr != nil {
			reason = "metadata publication failed or became uncertain; no automatic retry"
		}
		if err := api.saveOutput(ctx, job, target, owner, state, reason, proof); err != nil {
			return "needs_review", "publication receipt could not be committed; inspect original intent before any recovery"
		}
		if state == "uncertain" {
			return "needs_review", "publication outcome uncertain; target reserved until explicit readonly reconciliation"
		}
		if publishErr != nil || state == "blocked" {
			warnings = append(warnings, reason)
		}
	}
	if len(warnings) > 0 {
		why := strings.Join(warnings, "; ")
		return "partial", "selected metadata incomplete or requires review: " + why[:min(len(why), 1600)]
	}
	return "completed", ""
}
func (api *libraryScraper) tick(ctx context.Context) error {
	if api == nil || api.intake == nil || api.intake.organizer == nil || !api.intake.organizer.pureGo || api.intake.organizer.jobs == nil {
		return nil
	}
	store := api.intake.organizer.jobs
	guard, err := store.AcquireJob(ctx, "library-scraper-publication")
	if err != nil {
		return err
	}
	defer guard.Close()
	jobs, err := store.LibraryJobs(ctx, true)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := store.ClaimLibraryJob(ctx, job.ID); err != nil {
			if errors.Is(err, organization.ErrState) {
				continue
			}
			return err
		}
		job.State = "running"
		if job.Total != job.Definition.Count || job.Total < 1 || job.Total > 50000 {
			if err := store.FinishLibraryJob(ctx, job.ID, "needs_review", "saved media count is inconsistent; no files handled"); err != nil {
				return err
			}
			continue
		}
		state, reason := "completed", ""
		for page := 1; page <= 500; page++ {
			items, err := store.LibraryItems(ctx, job.ID, page)
			if err != nil {
				state, reason = "needs_review", "media journal could not be read; unfinished items were not replayed"
				break
			}
			stop := false
			for _, item := range items {
				if ctx.Err() != nil || store.CheckLibraryRunning(ctx, job.ID) != nil {
					stop = true
					state, reason = "needs_review", "job interrupted; unfinished items are not automatically replayed"
					break
				}
				if item.State != "pending" {
					state, reason = "needs_review", "unexpected previous execution state; no item replayed"
					stop = true
					break
				}
				if err := store.LibraryItemState(ctx, job.ID, item.Ordinal, "pending", "running", ""); err != nil {
					return err
				}
				outcome, why := api.runMedia(ctx, job, item.Media)
				if outcome == "needs_review" {
					state, reason = "needs_review", why
					stop = true
					outcome = "partial"
				} else if outcome != "completed" && state != "needs_review" {
					state, reason = "partial", "one or more media files have incomplete or blocked metadata; inspect receipts"
				}
				persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				err = store.LibraryItemState(persist, job.ID, item.Ordinal, "running", outcome, why)
				if err != nil {
					current, e := store.LibraryJob(persist, job.ID)
					cancel()
					if e == nil && current.State == "cancelled" {
						stop = true
						break
					}
					return err
				}
				cancel()
				if stop {
					break
				}
			}
			if stop || len(items) < 100 {
				break
			}
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		current, e := store.LibraryJob(persist, job.ID)
		if e == nil && current.State == "running" {
			e = store.FinishLibraryJob(persist, job.ID, state, reason)
		}
		cancel()
		if e != nil {
			return e
		}
	}
	return nil
}
func startLibraryScraper(ctx context.Context, api *libraryScraper) func() {
	if api == nil || api.intake == nil || api.intake.organizer == nil || !api.intake.organizer.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	api.workers.Add(1)
	go func() {
		defer close(done)
		defer api.workers.Add(-1)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		runLibraryScraper(ctx, api, time.Now(), ticker.C)
	}()
	return func() { <-done }
}
func runLibraryScraper(ctx context.Context, api *libraryScraper, now time.Time, ticks <-chan time.Time) {
	for {
		if ctx.Err() != nil {
			return
		}
		_ = api.pluginTick(ctx, now)
		_ = api.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case t, ok := <-ticks:
			if !ok {
				return
			}
			now = t
		}
	}
}
