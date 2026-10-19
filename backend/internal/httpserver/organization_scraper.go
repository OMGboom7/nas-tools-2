package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/scraper"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
)

type intakeScraper struct {
	organizer *organizationAPI
	system    *systemconfig.Store
	transport http.RoundTripper
}

func (api *intakeScraper) authorize(w http.ResponseWriter, r *http.Request) bool {
	if api == nil || api.organizer == nil || api.system == nil {
		writeAPIError(w, 503, 503, "native intake scraper unavailable")
		return false
	}
	return api.organizer.jobsAuthorized(w, r)
}
func (api *intakeScraper) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	raw, err := api.system.Get(r.Context(), scraper.ConfigKey)
	if err != nil {
		writeAPIError(w, 503, 503, "scraper settings unavailable")
		return
	}
	o, warnings, err := scraper.Parse(raw)
	if err != nil {
		warnings = []string{"stored scraper configuration is invalid; no sidecars will be generated"}
	}
	if warnings == nil {
		warnings = []string{}
	}
	snapshot, err := api.organizer.service.configStore.Snapshot()
	if err != nil {
		writeAPIError(w, 503, 503, "scraper settings unavailable")
		return
	}
	enabled, _ := objectValue(snapshot["media"])["nfo_poster"].(bool)
	rows, err := api.organizer.jobs.Scrapes(r.Context(), false)
	if err != nil {
		writeAPIError(w, 503, 503, "scraper receipts unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"enabled": enabled, "options": o, "fields": scraper.Fields, "fingerprint": organization.ScrapeConfigDigest(raw), "warnings": warnings, "receipts": rows}})
}
func (api *intakeScraper) saveOptions(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var in struct {
		Options     scraper.Options `json:"options"`
		Fingerprint string          `json:"fingerprint"`
		Confirm     bool            `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid scraper options") {
		return
	}
	if !in.Confirm || len(in.Fingerprint) != 64 || in.Options.ValidatePatch() != nil {
		writeAPIError(w, 400, 400, "reviewed options and explicit confirmation required")
		return
	}
	err := api.system.UpdateMany(r.Context(), []string{scraper.ConfigKey}, func(raw map[string]string) (map[string]string, error) {
		if organization.ScrapeConfigDigest(raw[scraper.ConfigKey]) != in.Fingerprint {
			return nil, organization.ErrState
		}
		var values map[string]any
		if raw[scraper.ConfigKey] != "" && json.Unmarshal([]byte(raw[scraper.ConfigKey]), &values) != nil {
			return nil, scraper.ErrConfig
		}
		if values == nil {
			values = map[string]any{}
		}
		for family, kinds := range map[string]map[string]scraper.Flags{"scraper_nfo": in.Options.NFO, "scraper_pic": in.Options.Pictures} {
			group := objectValue(values[family])
			if values[family] != nil {
				if _, ok := values[family].(map[string]any); !ok {
					return nil, scraper.ErrConfig
				}
			}
			for kind, flags := range kinds {
				v := objectValue(group[kind])
				if group[kind] != nil {
					if _, ok := group[kind].(map[string]any); !ok {
						return nil, scraper.ErrConfig
					}
				}
				for name, enabled := range flags {
					v[name] = enabled
				}
				group[kind] = v
			}
			values[family] = group
		}
		b, err := json.Marshal(values)
		if err != nil || len(b) > 64<<10 {
			return nil, scraper.ErrConfig
		}
		if _, _, err := scraper.Parse(string(b)); err != nil {
			return nil, err
		}
		return map[string]string{scraper.ConfigKey: string(b)}, nil
	})
	if err != nil {
		status := 503
		if errors.Is(err, organization.ErrState) {
			status = 409
		}
		if errors.Is(err, scraper.ErrConfig) {
			status = 400
		}
		writeAPIError(w, status, status, "scraper settings not changed; refresh before any retry")
		return
	}
	api.serveStatus(w, r)
}

// The YAML enable switch and SQLite options are deliberately independent
// operations. Saving options never turns a background writer on implicitly.
func (api *intakeScraper) setEnabled(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
		Confirm bool  `json:"confirm"`
	}
	if !decodeOrganizationRequest(w, r, &in, "invalid scraper switch") {
		return
	}
	if in.Enabled == nil || !in.Confirm {
		writeAPIError(w, 400, 400, "explicit scraper switch confirmation required")
		return
	}
	if err := api.organizer.service.configStore.Update(map[string]any{"media.nfo_poster": *in.Enabled}); err != nil {
		writeAPIError(w, 503, 503, "scraper switch write uncertain; refresh configuration")
		return
	}
	api.serveStatus(w, r)
}
func (api *intakeScraper) current(ctx context.Context, job organization.Job, row organization.ScrapeReceipt) error {
	if !job.Definition.ScraperEnabled || api.organizer.validateJobConfiguration(ctx, job) != nil {
		return organization.ErrState
	}
	raw, err := api.system.Get(ctx, scraper.ConfigKey)
	if err != nil {
		return err
	}
	if organization.ScrapeConfigDigest(raw) != row.Config {
		return organization.ErrState
	}
	return nil
}
func (api *intakeScraper) client(snapshot map[string]any) scraper.Client {
	app, media := objectValue(snapshot["app"]), objectValue(snapshot["media"])
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text(app["tmdb_domain"])), "https://"), "http://"), "/")
	if host == "" {
		host = "api.themoviedb.org"
	}
	language := strings.TrimSpace(text(media["tmdb_language"]))
	if language == "" {
		language = "zh"
	}
	base := strings.TrimRight(strings.TrimSpace(text(app["tmdb_image_url"])), "/")
	if base == "" {
		base = "https://image.tmdb.org"
	}
	return scraper.Client{Host: host, Key: strings.TrimSpace(text(app["rmt_tmdbkey"])), Language: language, ImageBase: base, Transport: api.transport}
}
func (api *intakeScraper) tick(ctx context.Context) error {
	if api == nil || api.organizer == nil || !api.organizer.pureGo || api.organizer.jobs == nil {
		return nil
	}
	guard, err := api.organizer.jobs.AcquireJob(ctx, "intake-enrichment-outbox")
	if err != nil {
		return err
	}
	defer guard.Close()
	rows, err := api.organizer.jobs.Scrapes(ctx, true)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		job, err := api.organizer.jobs.Get(ctx, row.JobID)
		valid := err == nil && job.State == "completed" && row.Ordinal >= 0 && row.Ordinal < len(job.Items) && len(job.Items) == len(job.Definition.Entries)
		var item organization.JobItem
		if valid {
			item = job.Items[row.Ordinal]
			valid = item.Kind == "media" && item.Index == row.Ordinal && organization.SubtitleOriginDigest(job, item) == row.Origin && api.current(ctx, job, row) == nil && organization.VerifyCompletedTarget(ctx, job, item) == nil
		}
		if err := api.organizer.jobs.ClaimScrape(ctx, row); err != nil {
			if errors.Is(err, organization.ErrState) {
				continue
			}
			return err
		}
		state, reason := "blocked", "original completed media or configuration changed; no sidecars written"
		if valid {
			state, reason = api.run(ctx, job, item, row)
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = api.organizer.jobs.FinishScrape(persist, row.ID, state, reason)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
func (api *intakeScraper) run(ctx context.Context, job organization.Job, item organization.JobItem, row organization.ScrapeReceipt) (string, string) {
	snapshot, err := api.organizer.service.configStore.Snapshot()
	if err != nil || organization.Digest(snapshot) != job.Definition.ConfigDigest {
		return "blocked", "application configuration changed; no sidecars written"
	}
	o, warnings, err := scraper.Parse(row.Options)
	if err != nil {
		return "blocked", "scraper configuration unavailable"
	}
	kind := displayMediaKind(item.MediaType)
	in := scraper.Input{Kind: kind, ID: item.TMDBID, Stem: strings.TrimSuffix(filepath.Base(item.Target), filepath.Ext(item.Target))}
	if kind == "tv" {
		ep, err := mediameta.Episodes(item.SeasonEpisode, "")
		if err != nil || ep.Season == nil || ep.Episode == nil || ep.EndSeason != nil {
			return "blocked", "saved season and episode identity unavailable"
		}
		in.Season, in.First, in.Last = *ep.Season, *ep.Episode, *ep.Episode
		if ep.EndEpisode != nil {
			in.Last = *ep.EndEpisode
		}
	}
	client := api.client(snapshot)
	plan, err := client.Build(ctx, in, o, time.Now())
	if err != nil {
		return "blocked", "verified scraper metadata unavailable; no sidecars written"
	}
	warnings = append(warnings, plan.Warnings...)
	if api.current(ctx, job, row) != nil || organization.VerifyCompletedTarget(ctx, job, item) != nil {
		return "blocked", "completed media or scraper configuration changed during metadata lookup"
	}
	for _, artifact := range plan.Artifacts {
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, "scraping interrupted; unfinished artifacts were not replayed")
			break
		}
		data := artifact.Data
		if artifact.URL != "" {
			data, err = client.Download(ctx, artifact.URL)
			if err != nil {
				out := organization.SidecarRecord{Name: artifact.Name, Scope: artifact.Scope, State: "blocked", Reason: "image unavailable or invalid; no file written"}
				if api.organizer.jobs.SaveScrapeOutput(ctx, row.ID, out) != nil {
					return "partial", "image failure receipt could not be saved; inspect original scope"
				}
				warnings = append(warnings, out.Reason)
				continue
			}
		}
		out := organization.SidecarRecord{Name: artifact.Name, Scope: artifact.Scope}
		proof, state, writeErr := organization.PublishSidecar(ctx, job, item, artifact.Scope, artifact.Name, data, func(p organization.SidecarProof) error {
			out.Proof, out.State = p, "writing"
			return api.organizer.jobs.SaveScrapeOutput(ctx, row.ID, out)
		}, func() error { return api.current(ctx, job, row) })
		out.Proof, out.State = proof, state
		if writeErr != nil {
			out.Reason = "sidecar publication not positively completed; inspect receipt without replay"
			warnings = append(warnings, out.Reason)
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = api.organizer.jobs.SaveScrapeOutput(persist, row.ID, out)
		cancel()
		if err != nil {
			return "partial", "sidecar result receipt uncertain; original media and any published sidecars retained"
		}
	}
	if len(warnings) > 0 {
		reason := strings.Join(warnings, "; ")
		if len(reason) > 3900 {
			reason = reason[:3900]
		}
		return "partial", reason
	}
	return "completed", ""
}
func startIntakeScraper(ctx context.Context, api *intakeScraper) func() {
	if api == nil || api.organizer == nil || !api.organizer.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			_ = api.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { <-done }
}
