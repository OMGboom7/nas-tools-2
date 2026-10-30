package httpserver

import (
	"context"
	"errors"
	"path"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/opensubtitles"
	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
)

const oldSubtitleAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/98.0.4758.102 Safari/537.36"

type openSubtitleWork struct {
	ctx     context.Context
	library organization.LibraryDefinition
	media   organization.LibraryMedia
	base    func() error
	full    func() error
	close   func()
	query   opensubtitles.Query
}

func (api *openSubtitleAPI) scopeAuthority(scope opensubtitles.Snapshot) error {
	authority, e := api.authority(scope.Actor)
	if e != nil || authority != scope.Authority {
		return organization.ErrState
	}
	return nil
}
func (api *openSubtitleAPI) query(ctx context.Context, p organization.OpenSubtitleReceipt) (opensubtitles.Query, error) {
	d := p.Definition
	kind := "tv"
	movie := d.Media["type"] == "电影"
	if movie {
		kind = "movie"
	}
	id := text(d.Media["tmdb_id"])
	detail, e := fetchNativeTMDBDetails(ctx, api.subtitle.organizer.service.configStore, api.transport, kind, id)
	if e != nil || strconv.FormatInt(detail.ID, 10) != id {
		return opensubtitles.Query{}, organization.ErrState
	}
	attrs := detail.Attributes
	name := text(d.Media["en_name"])
	if name == "" {
		name = text(attrs["original_title"])
	}
	if name == "" {
		name = text(attrs["original_name"])
	}
	if name == "" {
		name = text(d.Media["cn_name"])
	}
	if name == "" {
		name = text(d.Media["title"])
	}
	imdb := text(attrs["imdb_id"])
	if imdb == "" {
		imdb = text(objectValue(attrs["external_ids"])["imdb_id"])
	}
	if imdb == "" {
		imdb = text(d.Media["imdb_id"])
	}
	q := opensubtitles.Query{Movie: movie, IMDb: imdb, Name: name, Year: text(d.Media["year"]), Seasons: []int{}, Episodes: []int{}}
	if !movie {
		episode, e := mediameta.Episodes(text(d.Media["season_episode"]), "")
		if e != nil {
			return q, e
		}
		if episode.Season != nil {
			end := *episode.Season
			if episode.EndSeason != nil {
				end = *episode.EndSeason
			}
			if end < *episode.Season || end-*episode.Season > 99 {
				return q, organization.ErrLimit
			}
			for i := *episode.Season; i <= end; i++ {
				q.Seasons = append(q.Seasons, i)
			}
		}
		if episode.Episode != nil {
			end := *episode.Episode
			if episode.EndEpisode != nil {
				end = *episode.EndEpisode
			}
			if end < *episode.Episode || end-*episode.Episode > 99 {
				return q, organization.ErrLimit
			}
			for i := *episode.Episode; i <= end; i++ {
				q.Episodes = append(q.Episodes, i)
			}
		}
	}
	if !q.Valid() {
		return q, organization.ErrState
	}
	return q, nil
}
func (api *openSubtitleAPI) work(ctx context.Context, p organization.OpenSubtitleReceipt) (openSubtitleWork, error) {
	work := openSubtitleWork{ctx: ctx, close: func() {}}
	d := p.Definition
	organizer := api.subtitle.organizer
	if e := api.scopeAuthority(d.Provider); e != nil {
		return work, e
	}
	var c *remotestore.Client
	var lib organization.LibraryDefinition
	lib.Mode = "no_force"
	lib.Actor = d.Provider.Actor
	lib.RequestID = p.ID[:32]
	lib.Count = 1
	if d.Kind == "manual" {
		parent, e := organizer.jobs.SubtitleRequest(ctx, d.Reference)
		if e != nil || parent.Fingerprint != d.Origin || parent.Definition.OpenSubtitles == nil {
			return work, organization.ErrState
		}
		lib = organization.SubtitleRequestLibrary(parent.Definition)
		lib.Mode = "no_force"
		lib.Actor = d.Provider.Actor
		lib.RequestID = p.ID[:32]
		lib.Count = 1
		lib.Remote = parent.Definition.Scope.Remote
		var closeRemote func()
		c, closeRemote, e = api.subtitle.requestContext(ctx, parent.Definition)
		if e != nil {
			return work, e
		}
		work.close = closeRemote
		work.base = func() error {
			if e := api.scopeAuthority(d.Provider); e != nil {
				return e
			}
			return api.subtitle.requestCurrent(ctx, parent.Definition)
		}
		work.full = func() error {
			if e := work.base(); e != nil {
				return e
			}
			return organization.VerifySubtitleRequestSource(ctx, parent.Definition, c)
		}
	} else {
		job, e := organizer.jobs.Get(ctx, d.Reference)
		if e != nil || job.State != "completed" || d.Ordinal < 0 || d.Ordinal >= len(job.Items) {
			return work, organization.ErrState
		}
		item := job.Items[d.Ordinal]
		if organization.SubtitleOriginDigest(job, item) != d.Origin || path.Join(job.Definition.TargetRoot, item.Target) != d.File {
			return work, organization.ErrState
		}
		lib.Root = organization.LibraryRoot{ID: job.Definition.TargetID, Path: job.Definition.TargetRoot, Identity: job.Definition.TargetIdentity}
		lib.Selection = item.Target
		lib.ConfigDigest = job.Definition.ConfigDigest
		lib.Remote = job.Definition.Remote
		lib.AllowDiscExpansion = item.Disc != nil
		if lib.Remote != nil {
			if !d.RemoteWrite || !d.Provider.Remote {
				return work, organization.ErrState
			}
			c, e = organizer.remoteFactory(job.Mode).Open(lib.Remote)
			if e != nil {
				return work, e
			}
			work.close = func() { _ = c.Close() }
			work.ctx = organization.RemoteContext(ctx, c, job.Definition)
		} else {
			roots, e := organizer.roots(ctx)
			if e != nil {
				return work, e
			}
			for _, r := range append(append([]organizationRoot{}, roots.Sources...), roots.Targets...) {
				if r.ID != lib.Root.ID {
					if known, e := libraryRootSnapshot(r); e == nil {
						lib.Allowed = append(lib.Allowed, known)
					}
				}
			}
		}
		work.base = func() error {
			if e := api.scopeAuthority(d.Provider); e != nil {
				return e
			}
			return organizer.validateJobConfiguration(work.ctx, job)
		}
		work.full = func() error {
			if e := work.base(); e != nil {
				return e
			}
			return organization.VerifyCompletedTarget(work.ctx, job, item)
		}
	}
	if d.Media["type"] == "电影" {
		lib.Root.Kind = "movie"
	} else {
		lib.Root.Kind = "tv"
	}
	if lib.Remote != nil {
		if !d.RemoteWrite {
			work.close()
			return work, organization.ErrState
		}
		lib.RemoteApproval = &organization.RemoteApproval{Actor: lib.Actor, Created: d.Created, Exclusive: true, RecoveryCopy: true, Origin: "open-subtitles"}
		work.ctx = organization.LibraryRemoteContext(work.ctx, c, lib)
	}
	work.library = lib
	if e := work.full(); e != nil {
		work.close()
		return work, e
	}
	var media organization.LibraryMedia
	if lib.Remote == nil && !d.Bluray {
		m, e := organization.CaptureLibraryMedia(work.ctx, lib, lib.Selection)
		if e != nil {
			work.close()
			return work, e
		}
		media = m
	} else {
		scanned, e := organization.ScanLibrary(work.ctx, lib)
		if e != nil || len(scanned.Files) != 1 || scanned.Files[0].Path != lib.Selection || scanned.Files[0].Blocked != "" {
			work.close()
			return work, organization.ErrState
		}
		media = scanned.Files[0]
		if lib.Remote != nil {
			m, e := organization.PrepareLibraryRemoteMedia(work.ctx, lib, media)
			if e != nil {
				work.close()
				return work, e
			}
			media = m
		}
	}
	if (media.Disc != nil) != d.Bluray {
		work.close()
		return work, organization.ErrState
	}
	work.media = media
	if e := work.full(); e != nil {
		work.close()
		return work, e
	}
	q, e := api.query(work.ctx, p)
	if e != nil {
		work.close()
		return work, e
	}
	work.query = q
	work.library.OpenSubtitle = &organization.OpenSubtitleLibrary{Request: p.ID, Fingerprint: p.Fingerprint, Query: q}
	return work, nil
}
func (api *openSubtitleAPI) rate(ctx context.Context, _ string, kind string) error {
	if kind != "login" {
		return ctx.Err()
	}
	api.mu.Lock()
	at := time.Now()
	if earliest := api.lastLogin.Add(time.Second); earliest.After(at) {
		at = earliest
	}
	api.lastLogin = at
	api.mu.Unlock()
	if delay := time.Until(at); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
func (api *openSubtitleAPI) run(ctx context.Context, p organization.OpenSubtitleReceipt) error {
	store := api.subtitle.organizer.jobs
	call, cancel := context.WithTimeout(ctx, organization.ExecutionTimeout)
	defer cancel()
	api.mu.Lock()
	if api.active == nil {
		api.active = map[string]context.CancelFunc{}
	}
	api.active[p.ID] = cancel
	api.mu.Unlock()
	defer func() { api.mu.Lock(); delete(api.active, p.ID); api.mu.Unlock() }()
	closeGuards, e := api.subtitle.requestGuards(call, true)
	if e != nil {
		return e
	}
	defer closeGuards()
	finishPending := func(reason string) error {
		persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		return store.FinishOpenSubtitle(persist, p.ID, p.Fingerprint, "blocked", reason)
	}
	if e = store.OpenSubtitleReady(call, p); e != nil {
		if errors.Is(e, bus.ErrEventWaiting) {
			return store.WaitOpenSubtitle(call, p)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return finishPending("original provider, intake or media scope changed; no subtitle operation started")
	}
	cfg, scope, _, ready, e := store.OpenSubtitleProvider(call)
	if e != nil || !ready || organization.Digest(scope) != organization.Digest(p.Definition.Provider) {
		return finishPending("original provider configuration changed; no subtitle operation started")
	}
	work, e := api.work(call, p)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return finishPending("original video, media identity or remote write permission unavailable; no subtitle operation started")
	}
	defer work.close()
	claimed, e := store.ClaimOpenSubtitle(work.ctx, p, work.library, work.media, work.base)
	if e != nil {
		if errors.Is(e, bus.ErrEventWaiting) {
			return store.WaitOpenSubtitle(call, p)
		}
		if errors.Is(e, organization.ErrState) || errors.Is(e, opensubtitles.ErrQuota) {
			return finishPending("original scope or this provider authorization was stopped or denied; no new subtitle operation")
		}
		return e
	}
	if cfg.Provider == opensubtitles.Legacy && cfg.UserAgent == "" {
		snapshot, e := api.subtitle.organizer.service.configStore.Snapshot()
		if e != nil {
			return store.FinishOpenSubtitle(context.WithoutCancel(ctx), claimed.ID, claimed.Fingerprint, "blocked", "original application configuration unavailable")
		}
		cfg.UserAgent = text(objectValue(snapshot["app"])["user_agent"])
		if cfg.UserAgent == "" {
			cfg.UserAgent = oldSubtitleAgent
		}
	}
	current := func() error {
		if e := work.base(); e != nil {
			return e
		}
		if work.library.Remote == nil {
			return organization.VerifyLibraryMedia(work.ctx, work.library, work.media)
		}
		return nil
	}
	client := &opensubtitles.Client{Config: cfg, Transport: api.transport, Gate: api.rate}
	api.mu.Lock()
	client.Session = api.sessions[scope.Epoch]
	if !time.Now().Before(client.Session.CachedUntil) {
		client.Session = opensubtitles.Session{}
	}
	api.mu.Unlock()
	client.Step = func(op context.Context, kind, signature string) (func(string, string) error, error) {
		index, e := store.OpenSubtitleStep(op, claimed.ID, claimed.Fingerprint, claimed.Binding, kind, signature, current)
		if e != nil {
			return nil, e
		}
		return func(state, digest string) error {
			persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer done()
			return store.FinishOpenSubtitleStep(persist, claimed.ID, claimed.Fingerprint, claimed.Binding, index, state, digest)
		}, nil
	}
	state, reason := "not_found", "no matching Chinese subtitle candidate; original video retained"
	created := 0
	process := func() error {
		if cfg.Provider == opensubtitles.REST {
			session, e := client.Login(work.ctx)
			if e != nil {
				return e
			}
			api.mu.Lock()
			if api.sessions == nil {
				api.sessions = map[string]opensubtitles.Session{}
			}
			api.sessions[scope.Epoch] = session
			api.mu.Unlock()
		}
		candidates, e := client.Search(work.ctx, work.query)
		if e != nil {
			return e
		}
		handled := map[string]bool{}
		candidatesSeen := map[string]bool{}
		attempts := 0
		for _, candidate := range candidates {
			if candidatesSeen[candidate.ID] {
				continue
			}
			candidatesSeen[candidate.ID] = true
			if attempts >= 3 {
				break
			}
			attempts++
			if e = work.full(); e != nil {
				return e
			}
			files, e := client.Download(work.ctx, candidate)
			if e != nil {
				return e
			}
			for _, file := range files {
				name := organization.OpenSubtitleName(claimed.Definition, file.Extension)
				if name == "" {
					return organization.ErrState
				}
				if handled[name] {
					continue
				}
				handled[name] = true
				if e = work.full(); e != nil {
					return e
				}
				target, e := organization.LibraryTarget(work.library, work.media, "media", name)
				if e != nil {
					return e
				}
				owner := organization.Digest([]string{"open-subtitle-output", claimed.ID, target})
				proof, result, publishErr := organization.PublishLibrarySidecar(work.ctx, work.library, work.media, "media", name, file.Data, func(proof organization.LibraryOutputProof) error {
					return store.SaveOpenSubtitleOutput(work.ctx, claimed, work.library.Root, target, owner, "writing", "original subtitle publication intent; no replay", proof, current)
				}, func() error {
					if e := store.OpenSubtitleRunning(work.ctx, claimed); e != nil {
						return e
					}
					return current()
				})
				if publishErr != nil {
					if proof.Stage != "" {
						result = "uncertain"
					} else {
						result = "blocked"
					}
				}
				persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				saved := store.SaveOpenSubtitleOutput(persist, claimed, work.library.Root, target, owner, result, "original subtitle publication result; existing files retained", proof, current)
				done()
				if saved != nil {
					return saved
				}
				if publishErr != nil {
					return publishErr
				}
				if result == "created" || result == "preserved" {
					created++
				}
			}
		}
		if created > 0 {
			state = "completed"
			reason = "original subtitle outputs created or preserved; inspect each durable file receipt"
		}
		return nil
	}
	e = process()
	if e != nil {
		state = "uncertain"
		reason = "provider response, publication or receipt not positively complete; no automatic replay"
		if errors.Is(e, organization.ErrState) || errors.Is(e, opensubtitles.ErrQuota) {
			state = "blocked"
			reason = "original scope changed or provider denied this request; previous facts retained"
		}
		probe, probeDone := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		latest, probeErr := store.OpenSubtitle(probe, claimed.ID)
		probeDone()
		if probeErr == nil && len(latest.Operations) > 0 {
			last := latest.Operations[len(latest.Operations)-1]
			if last.State == "intent" || last.State == "uncertain" {
				state = "uncertain"
				reason = "original external intent has no durable acknowledgement; no replay"
			}
		}
		if created > 0 {
			state = "partial"
			reason = "some original subtitle files created or preserved; remaining steps not replayed"
		}
	}
	persist, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	return store.FinishOpenSubtitle(persist, claimed.ID, claimed.Fingerprint, state, reason)
}
func (api *openSubtitleAPI) tick(ctx context.Context) error {
	if api == nil || api.subtitle == nil || api.subtitle.organizer == nil || !api.subtitle.organizer.pureGo || api.subtitle.organizer.jobs == nil {
		return nil
	}
	store := api.subtitle.organizer.jobs
	guard, e := store.AcquireJob(ctx, "intake-enrichment-outbox")
	if e != nil {
		return e
	}
	defer guard.Close()
	rows, e := store.OpenSubtitles(ctx, true, 1)
	if e != nil {
		return e
	}
	for _, p := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e = api.run(ctx, p); e != nil && !errors.Is(e, organization.ErrState) {
			return e
		}
	}
	return nil
}
func startOpenSubtitleWorker(ctx context.Context, api *openSubtitleAPI) func() {
	if api == nil || api.subtitle == nil || !api.subtitle.organizer.pureGo {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTicker(15 * time.Second)
		defer timer.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			_ = api.tick(ctx)
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
	}()
	return func() { <-done }
}
