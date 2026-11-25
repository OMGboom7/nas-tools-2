package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/bus"
	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/torrentfiles"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

var errDoubanIncomplete = errors.New("direct search has outstanding media")

func (api *doubanSyncAPI) directDownload(ctx context.Context, raw map[string]string, s douban.Subject, input subscriptionUpsertRequest, current func() error) (*nativeSubscriptionMetadata, error) {
	control, err := doubanState(raw[doubanControlKey])
	if err != nil || control.Actor == "" || api.runner == nil || api.runner.search.planner.search == nil {
		return nil, douban.ErrSettings
	}
	input, err = api.subscriptions.resolveDoubanTitles(ctx, input, s.Original, s.Title, s.Year)
	if err != nil {
		return nil, err
	}
	detail, err := fetchNativeTMDBDetails(ctx, api.subscriptions.configStore, api.subscriptions.client.Transport, s.Kind, input.MediaID)
	if err != nil {
		return nil, err
	}
	name, date := detail.Title, detail.ReleaseDate
	if s.Kind == "tv" {
		name, date = detail.Name, detail.FirstAirDate
	}
	if name == "" || len(date) < 4 {
		return nil, douban.ErrResponse
	}
	identity := &nativeSubscriptionMetadata{ID: input.MediaID, Title: name, Year: date[:4]}
	meta := mediameta.Metadata{Title: name, Year: identity.Year}
	var exactEpisodes []int
	if s.Kind == "tv" {
		meta.Episodes.TV = true
		// An unqualified show covers all regular seasons. Keep a season or
		// episode explicitly present in either Douban title.
		for _, title := range []string{s.Title, s.Original} {
			if title == "" {
				continue
			}
			ep, err := mediameta.Episodes(title, "")
			if err != nil {
				return nil, err
			}
			if ep.Episode != nil {
				units, err := mediameta.ExplicitEpisodeChain(title)
				if err != nil || len(units) > 0 && len(exactEpisodes) > 0 && !slices.Equal(units, exactEpisodes) {
					return nil, douban.ErrResponse
				}
				if len(units) > 0 {
					exactEpisodes = units
				}
			}
			for _, pair := range []struct {
				dest  **int
				value *int
			}{{&meta.Episodes.Season, ep.Season}, {&meta.Episodes.EndSeason, ep.EndSeason}, {&meta.Episodes.Episode, ep.Episode}, {&meta.Episodes.EndEpisode, ep.EndEpisode}} {
				if pair.value == nil {
					continue
				}
				if *pair.dest != nil && **pair.dest != *pair.value {
					return nil, douban.ErrResponse
				}
				*pair.dest = pair.value
			}
		}
	}
	coverage, err := api.subscriptions.nativeMediaExistence(ctx, meta, detail, s.Kind)
	if err != nil || coverage.Complete {
		return identity, err
	}
	if s.Kind == "movie" {
		coverage.Missing = map[int][]int{0: {0}}
	}
	totals := map[int]int{}
	for _, v := range detail.Seasons {
		if v.Episodes != nil {
			totals[v.Number] = *v.Episodes
		}
	}
	for season, units := range coverage.Missing {
		recorded, err := api.runner.download.downloaders.DirectCoverage(ctx, s.Kind, identity.ID, season)
		if err != nil {
			return nil, err
		}
		units = slices.DeleteFunc(units, func(n int) bool {
			return recorded[n] || len(exactEpisodes) > 0 && !slices.Contains(exactEpisodes, n)
		})
		if len(units) == 0 {
			delete(coverage.Missing, season)
		} else {
			coverage.Missing[season] = units
		}
	}
	if len(coverage.Missing) == 0 {
		return identity, nil
	}
	if err := current(); err != nil {
		return nil, err
	}
	groups, err := api.runner.filters.List(ctx)
	if err != nil {
		return nil, err
	}
	resources, err := api.directResources(ctx, s.Kind, *identity, detail, meta.Episodes, exactEpisodes, coverage.Missing, totals, groups, current)
	if err != nil {
		return nil, err
	}
	var outstanding []int
	for _, season := range slices.Sorted(maps.Keys(coverage.Missing)) {
		m := downloaderconfig.DirectMedia{Kind: s.Kind, TMDBID: identity.ID, Name: name, Year: identity.Year, Season: season, Units: coverage.Missing[season]}
		recorded, err := api.runner.download.downloaders.DirectCoverage(ctx, s.Kind, identity.ID, season)
		if err != nil {
			return nil, err
		}
		m.Units = slices.DeleteFunc(slices.Clone(m.Units), func(n int) bool { return recorded[n] })
		if len(m.Units) == 0 {
			continue
		}
		ok, err := api.downloadMissing(ctx, control, m, totals, coverage.Missing, resources, groups, current)
		if err != nil {
			return nil, err
		}
		if !ok {
			outstanding = append(outstanding, season)
		}
	}
	// A later multi-season selection can also finish an earlier season.
	for _, season := range outstanding {
		recorded, err := api.runner.download.downloaders.DirectCoverage(ctx, s.Kind, identity.ID, season)
		if err != nil {
			return nil, err
		}
		for _, episode := range coverage.Missing[season] {
			if !recorded[episode] {
				return nil, errDoubanIncomplete
			}
		}
	}
	return identity, nil
}

func (api *doubanSyncAPI) directResources(ctx context.Context, kind string, identity nativeSubscriptionMetadata, detail tmdbMediaDetails, episodes mediameta.EpisodeInfo, exactEpisodes []int, missing map[int][]int, totals map[int]int, groups []filterconfig.GroupInfo, current func() error) ([]identifiedSubscriptionResource, error) {
	cfg, err := api.subscriptions.configStore.Snapshot()
	if err != nil {
		return nil, err
	}
	englishFirst := truthy(objectValue(cfg["laboratory"])["search_en_title"])
	sites, err := api.runner.download.sites.List(ctx)
	if err != nil {
		return nil, err
	}
	search := func(title string) ([]identifiedSubscriptionResource, error) {
		if err := current(); err != nil {
			return nil, err
		}
		if err := api.publishDirectSearch(ctx, title, kind, identity, episodes, exactEpisodes); err != nil {
			return nil, err
		}
		found, err := api.runner.search.planner.search.fetchResources(ctx, title, nil)
		if err != nil {
			return nil, err
		}
		var identified []identifiedSubscriptionResource
		for _, r := range found {
			if r.Seeders != nil && *r.Seeders == 0 && !r.Public {
				continue
			}
			item, failure := api.runner.recognition.recognizeKind(ctx, r.Title, r.Description, kind)
			if failure != nil {
				return nil, errors.New(failure.message)
			}
			if item == nil || strconv.FormatInt(item.detail.ID, 10) != identity.ID {
				continue
			}
			entry := identifiedSubscriptionResource{resource: r, meta: item.meta, subtitle: item.subtitle, revised: text(item.data["rev_string"])}
			for _, site := range sites {
				if site.Name == r.Indexer {
					_, entry.siteGroup, entry.siteOrder, err = subscriptionRSSSiteNote(site)
					if err != nil {
						return nil, err
					}
					break
				}
			}
			identified = append(identified, entry)
		}
		for season, wanted := range missing {
			candidates, _, err := planSubscriptionCandidates(ctx, subscriptionCandidateInput{TV: kind == "tv", Season: season, Total: totals[season], Missing: wanted}, groups, identified)
			if err != nil {
				return nil, err
			}
			if len(candidates) > 0 {
				return identified, nil
			}
		}
		return nil, nil
	}
	if !englishFirst {
		r, err := search(identity.Title)
		if err != nil || len(r) > 0 {
			return r, err
		}
	}
	english := ""
	if text(detail.Attributes["original_language"]) == "en" {
		english = text(detail.Attributes["original_title"])
		if kind == "tv" {
			english = text(detail.Attributes["original_name"])
		}
	}
	if english == "" {
		d, err := fetchNativeTMDBDetailsLanguage(ctx, api.subscriptions.configStore, api.subscriptions.client.Transport, kind, identity.ID, "en")
		if err != nil {
			return nil, err
		}
		english = d.Title
		if kind == "tv" {
			english = d.Name
		}
	}
	if english != "" && (!strings.EqualFold(english, identity.Title) || englishFirst) {
		r, err := search(english)
		if err != nil || len(r) > 0 || strings.EqualFold(english, identity.Title) {
			return r, err
		}
	}
	if englishFirst {
		return search(identity.Title)
	}
	return nil, nil
}

func (api *doubanSyncAPI) downloadMissing(ctx context.Context, control doubanControl, m downloaderconfig.DirectMedia, totals map[int]int, missing map[int][]int, resources []identifiedSubscriptionResource, groups []filterconfig.GroupInfo, current func() error) (bool, error) {
	// Removing an unusable pack must allow a lower ranked release of the
	// same episode to be considered. Do not mutate the other seasons' list.
	resources = slices.Clone(resources)
	remaining := slices.Clone(m.Units)
	download := api.runner.download.service
	for len(remaining) > 0 {
		sel := subscriptionCandidateInput{TV: m.Kind == "tv", Season: m.Season, Total: totals[m.Season], Missing: remaining}
		candidates, _, err := planSubscriptionCandidates(ctx, sel, groups, resources)
		if err != nil {
			return false, err
		}
		if len(candidates) == 0 {
			return false, nil
		}
		c := candidates[0]
		var source identifiedSubscriptionResource
		resources = slices.DeleteFunc(resources, func(r identifiedSubscriptionResource) bool {
			if r.resource.DownloadURL != c.resource.DownloadURL {
				return false
			}
			source = r
			return true
		})
		if err := current(); err != nil {
			return false, err
		}
		item, opts, err := download.searchDownloadSettings(ctx, c.Site, addResourceRequest{})
		if err != nil {
			return false, err
		}
		opts, err = download.preparePTSource(ctx, item, c.resource, opts)
		if err != nil {
			return false, err
		}
		magnet, torrent, err := download.prepareSubscriptionTorrent(ctx, c)
		if err != nil {
			return false, err
		}
		units := c.Episodes
		if m.Kind == "movie" {
			units = []int{0}
		}
		if c.SelectFiles {
			if item.Type != "qbittorrent" && item.Type != "transmission" {
				continue
			}
			if torrent == nil {
				torrent, err = torrentmeta.ResolveMagnet(ctx, magnet, magnetInfoHash(magnet))
				if err != nil {
					continue
				}
			}
			metadata, files, err := torrentmeta.Manifest(torrent)
			if err != nil || magnet != "" && magnetInfoHash(magnet) != metadata.InfoHash {
				continue
			}
			targets := map[int][]int{m.Season: c.Episodes}
			if len(c.SeasonRange) == 2 {
				for season, wanted := range missing {
					if season == m.Season || season < c.SeasonRange[0] || season > c.SeasonRange[1] {
						continue
					}
					done, err := download.downloaders.DirectCoverage(ctx, m.Kind, m.TMDBID, season)
					if err != nil {
						return false, err
					}
					requested := slices.DeleteFunc(slices.Clone(wanted), func(n int) bool { return done[n] })
					allowed, _, err := planSubscriptionCandidates(ctx, subscriptionCandidateInput{TV: true, Season: season, Total: totals[season], Missing: requested}, groups, []identifiedSubscriptionResource{source})
					if err != nil {
						return false, err
					}
					if len(allowed) > 0 {
						targets[season] = allowed[0].Episodes
					}
				}
			}
			pack := directFileProof{ExactSeason: len(c.SeasonRange) > 0, Totals: totals}
			selected, actual, err := pack.selectFiles(files, targets)
			if errors.Is(err, torrentfiles.ErrManifest) {
				continue
			}
			if err != nil {
				return false, err
			}
			if len(actual[m.Season]) == 0 {
				continue
			}
			units = actual[m.Season]
			delete(actual, m.Season)
			m.Units = units
			m.ExtraSeasons = actual
			err = api.submitDirectFiles(ctx, control, item, opts, c.Title, metadata.InfoHash, torrent, selected, m, pack, current)
			if err != nil {
				return false, err
			}
		} else {
			m.Units = units
			m.ExtraSeasons = nil
			_, handled, err := download.submitRecorded(ctx, control.Actor, magnet, torrent, &item, opts, c.Title, "豆瓣同步", &m, current)
			if err != nil {
				return false, err
			}
			if !handled {
				return false, errDownloadOptionsUnsupported
			}
		}
		remaining = slices.DeleteFunc(remaining, func(n int) bool { _, found := slices.BinarySearch(units, n); return found })
	}
	return true, nil
}

func (api *doubanSyncAPI) publishDirectSearch(ctx context.Context, title, kind string, identity nativeSubscriptionMetadata, episodes mediameta.EpisodeInfo, exactEpisodes []int) error {
	numbers := func(begin, end *int) []int {
		if begin == nil {
			return nil
		}
		last := *begin
		if end != nil {
			last = *end
		}
		var out []int
		for i := *begin; i <= last; i++ {
			out = append(out, i)
		}
		return out
	}
	seasons := numbers(episodes.Season, episodes.EndSeason)
	eps := exactEpisodes
	if len(eps) == 0 {
		eps = numbers(episodes.Episode, episodes.EndEpisode)
	}
	if len(eps) > 0 && len(seasons) == 0 {
		seasons = []int{1}
	}
	if eps == nil {
		eps = []int{}
	}
	mtype := "电影"
	if kind == "tv" {
		mtype = "电视剧"
	}
	key, err := refreshNonce()
	if err != nil {
		return err
	}
	id, err := strconv.ParseInt(identity.ID, 10, 64)
	if err != nil {
		return err
	}
	args := map[string]any{"season": seasons, "episode": eps, "year": identity.Year, "type": mtype, "site": nil, "seeders": true}
	return api.plugins.system.Transaction(ctx, func(tx *sql.Tx) error {
		return bus.PublishSignal(ctx, tx, "search.start", key, "DoubanSync", map[string]any{"key_word": title, "media_info": map[string]any{"id": id}, "filter_args": args, "search_type": "豆瓣"})
	})
}
