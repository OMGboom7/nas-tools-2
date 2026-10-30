package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
	"github.com/dlclark/regexp2"
)

const fuzzyCandidateLimit = 200

// A rule can select several different films/episodes in the same occurrence.
// Never reuse exact-subscription's "first movie wins" or missing-episode set.
type fuzzyCandidateView struct {
	Title    string `json:"title"`
	Name     string `json:"name"`
	Year     string `json:"year"`
	Kind     string `json:"kind"`
	Season   string `json:"season"`
	Episodes string `json:"episodes"`
	Site     string `json:"site"`
	Priority int    `json:"priority"`
	TopOrder int    `json:"topOrder"`
	GroupID  int64  `json:"groupId,string"`
}
type fuzzyCandidateRecord struct {
	View       fuzzyCandidateView
	Resource   externalindexer.Resource
	SiteID     int64
	SiteDigest string
}

// Search resources deliberately hide their download locator from public JSON.
// This private document must explicitly retain it, including its resolver,
// so review fingerprints and recovered batches bind the original submission.
type fuzzyCandidateDocument struct {
	View             fuzzyCandidateView
	Resource         externalindexer.Resource
	DownloadURL      string
	DownloadResolver string
	SiteID           int64
	SiteDigest       string
	Browser          *browserruntime.Session `json:",omitempty"`
}

func (c fuzzyCandidateRecord) MarshalJSON() ([]byte, error) {
	return json.Marshal(fuzzyCandidateDocument{c.View, c.Resource, c.Resource.DownloadURL, c.Resource.DownloadResolver, c.SiteID, c.SiteDigest, c.Resource.Browser})
}
func (c *fuzzyCandidateRecord) UnmarshalJSON(b []byte) error {
	var document fuzzyCandidateDocument
	if err := decodeFuzzyDocument(string(b), &document, 3<<20); err != nil {
		return err
	}
	c.View, c.Resource, c.SiteID, c.SiteDigest = document.View, document.Resource, document.SiteID, document.SiteDigest
	c.Resource.DownloadURL, c.Resource.DownloadResolver = document.DownloadURL, document.DownloadResolver
	c.Resource.Browser = document.Browser
	return nil
}

type fuzzySubscriptionMatcher struct {
	name, year, kind, season string
	namePattern              *regexp2.Regexp
	patterns                 []*regexp2.Regexp
	group                    int64
	groups                   []filterconfig.GroupInfo
	seederFirst              bool
}

func newFuzzyMatcher(raw map[string]any, kind, order string, groups []filterconfig.GroupInfo) (*fuzzySubscriptionMatcher, error) {
	v := normalizeNativeSubscriptionRow(raw)
	m := &fuzzySubscriptionMatcher{name: text(v["name"]), year: text(v["year"]), kind: kind, season: strings.ToUpper(text(v["season"])), groups: groups, seederFirst: order == "seeder"}
	if !truthy(v["fuzzy_match"]) || kind != "MOV" && kind != "TV" || m.name == "" || len(m.name) > 16<<10 {
		return nil, errors.New("invalid fuzzy subscription rule")
	}
	if m.year != "" {
		n, err := strconv.Atoi(m.year)
		if err != nil || n < 1 || n > 9999 {
			return nil, errors.New("invalid fuzzy subscription year")
		}
	}
	if m.season != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(m.season, "S"))
		if err != nil || n < 0 || n > 1000 {
			return nil, errors.New("invalid fuzzy subscription season")
		}
		m.season = fmt.Sprintf("S%02d", n)
	}
	var err error
	m.namePattern, err = regexcompat.Compile(m.name, regexp2.IgnoreCase)
	if err != nil {
		return nil, err // Invalid regex must not silently become a literal rule.
	}
	quality := map[string]string{"BLURAY": `Blu-?Ray|BD|BDRIP`, "REMUX": "REMUX", "DOLBY": `DOLBY|DOVI|\s+DV$|\s+DV\s+`, "WEB": `WEB-?DL|WEBRIP`, "HDTV": "U?HDTV", "UHD": "UHD", "HDR": "HDR", "3D": "3D"}
	resolution := map[string]string{"8k": "8K", "4k": "4K|2160P|X2160", "1080p": "1080[PIX]|X1080", "720p": "720P"}
	q, p := text(v["filter_restype"]), text(v["filter_pix"])
	if q != "" && quality[q] == "" || p != "" && resolution[p] == "" {
		return nil, errors.New("unsupported fuzzy subscription labels")
	}
	if value := text(v["filter_rule"]); value != "" {
		m.group, err = strconv.ParseInt(value, 10, 64)
		if err != nil || m.group < -1 {
			return nil, errors.New("invalid fuzzy subscription filter group")
		}
	}
	if _, err := filterconfig.MatchGroups(groups, m.group, filterconfig.TorrentMetadata{RequireKnown: true}); err != nil {
		return nil, err
	}
	for _, value := range []string{quality[q], resolution[p], text(v["filter_team"]), text(v["filter_include"]), text(v["filter_exclude"])} {
		var pattern *regexp2.Regexp
		if value != "" {
			if len(value) > 16<<10 {
				return nil, errors.New("fuzzy subscription filter too large")
			}
			pattern, err = regexcompat.Compile(value, regexp2.IgnoreCase)
			if err != nil {
				return nil, err
			}
		}
		m.patterns = append(m.patterns, pattern)
	}
	return m, nil
}

func fuzzySeason(meta mediameta.Metadata) string {
	if !meta.Episodes.TV {
		return ""
	}
	n := 1
	if meta.Episodes.Season != nil {
		n = *meta.Episodes.Season
	}
	value := fmt.Sprintf("S%02d", n)
	if meta.Episodes.EndSeason != nil && *meta.Episodes.EndSeason != n {
		value += fmt.Sprintf("-S%02d", *meta.Episodes.EndSeason)
	}
	return value
}
func fuzzyEpisodes(item identifiedSubscriptionResource) (string, int, error) {
	info := item.meta.Episodes
	if !info.TV || info.Episode == nil {
		return "", info.Count, nil
	}
	chain := candidateEpisodeChain.FindString(item.revised)
	if chain == "" {
		chain = candidateEpisodeChain.FindString(item.subtitle)
	}
	numbers := []int{}
	if chain != "" {
		seen := map[int]bool{}
		for _, match := range candidateEpisodeNumber.FindAllStringSubmatch(chain, -1) {
			n, err := strconv.Atoi(match[1])
			if err != nil || n < 1 || n > 10000 || seen[n] {
				return "", 0, errors.New("invalid fuzzy subscription episode chain")
			}
			seen[n] = true
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
	} else {
		last := *info.Episode
		if info.EndEpisode != nil {
			last = *info.EndEpisode
		}
		if *info.Episode < 1 || last < *info.Episode || last > 10000 {
			return "", 0, errors.New("invalid fuzzy subscription episode range")
		}
		for n := *info.Episode; n <= last; n++ {
			numbers = append(numbers, n)
		}
	}
	return subscriptionEpisodeString(numbers), len(numbers), nil
}
func (m *fuzzySubscriptionMatcher) identity(ctx context.Context, item identifiedSubscriptionResource) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if item.meta.Episodes.TV != (m.kind == "TV") || m.year != "" && m.year != item.meta.Year {
		return false, nil
	}
	if m.kind == "TV" && m.season != "" && m.season != "S00" && m.season != fuzzySeason(item.meta) {
		return false, nil
	}
	title := item.revised + " " + item.meta.Title + " " + item.meta.Year
	if len(title) > 64<<10 {
		return false, errors.New("fuzzy subscription title too large")
	}
	matched, err := m.namePattern.MatchString(title)
	if err != nil {
		return false, err
	}
	return matched || strings.Contains(title, m.name), ctx.Err()
}
func (m *fuzzySubscriptionMatcher) selectCandidates(ctx context.Context, resources []identifiedSubscriptionResource) ([]fuzzyCandidateRecord, error) {
	type ranked struct {
		record            fuzzyCandidateRecord
		siteOrder         int
		seeders           *int64
		seasons, episodes int
	}
	all := []ranked{}
	for _, item := range resources {
		matched, err := m.identity(ctx, item)
		if err != nil {
			return nil, err
		}
		if !matched || item.site == nil || !rssDownloadURLAllowed(item.resource.DownloadURL) {
			continue
		}
		texts := []string{item.meta.Source + " " + item.meta.Effect, item.meta.Resolution, item.meta.Team, item.revised + " " + item.subtitle, item.revised + " " + item.subtitle}
		if texts[2] == "" {
			texts[2] = item.revised
		}
		for i, pattern := range m.patterns {
			if pattern == nil {
				continue
			}
			found, err := pattern.MatchString(texts[i])
			if err != nil {
				return nil, err
			}
			matched = matched && (found != (i == 4))
		}
		if !matched {
			continue
		}
		episodes, count, err := fuzzyEpisodes(item)
		if err != nil {
			return nil, err
		}
		group, groupID, top := m.group, int64(0), 0
		if group == 0 && item.siteGroup != nil {
			group = *item.siteGroup
		}
		for _, entry := range m.groups {
			if group == entry.Group.ID || group == 0 && entry.Group.Default {
				groupID = entry.Group.ID
				if len(entry.Rules) > 0 {
					priority, err := strconv.Atoi(entry.Rules[0].Priority)
					if entry.Rules[0].Priority == "" {
						priority, err = 0, nil
					}
					if err != nil {
						return nil, err
					}
					top = 100 - priority
				}
				break
			}
		}
		result, err := filterconfig.MatchGroups(m.groups, group, filterconfig.TorrentMetadata{Title: item.revised, Subtitle: item.subtitle, SizeBytes: float64(item.resource.Size), Movie: m.kind == "MOV", Episodes: count, UploadFactor: item.resource.UploadFactor, DownloadFactor: item.resource.DownloadFactor, RequireKnown: true})
		if err != nil {
			return nil, err
		}
		if !result.Matched {
			continue
		}
		seasonCount := 1
		if item.meta.Episodes.Season != nil && item.meta.Episodes.EndSeason != nil {
			seasonCount = *item.meta.Episodes.EndSeason - *item.meta.Episodes.Season + 1
		}
		view := fuzzyCandidateView{Title: item.resource.Title, Name: item.meta.Title, Year: item.meta.Year, Kind: m.kind, Season: fuzzySeason(item.meta), Episodes: episodes, Site: item.resource.Indexer, Priority: result.Order, GroupID: groupID, TopOrder: top}
		all = append(all, ranked{fuzzyCandidateRecord{View: view, Resource: item.resource, SiteID: item.site.ID, SiteDigest: fuzzyDigest(item.site)}, item.siteOrder, item.resource.Seeders, seasonCount, count})
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.record.View.Name != b.record.View.Name {
			return a.record.View.Name > b.record.View.Name
		}
		if a.record.View.Priority != b.record.View.Priority {
			return a.record.View.Priority > b.record.View.Priority
		}
		if m.seederFirst {
			if (a.seeders != nil) != (b.seeders != nil) {
				return a.seeders != nil
			}
			if a.seeders != nil && *a.seeders != *b.seeders {
				return *a.seeders > *b.seeders
			}
		}
		if a.siteOrder != b.siteOrder {
			return a.siteOrder > b.siteOrder
		}
		if (a.seeders != nil) != (b.seeders != nil) {
			return a.seeders != nil
		}
		if a.seeders != nil && *a.seeders != *b.seeders {
			return *a.seeders > *b.seeders
		}
		if a.seasons != b.seasons {
			return a.seasons > b.seasons
		}
		if a.episodes != b.episodes {
			return a.episodes > b.episodes
		}
		return a.record.View.Title < b.record.View.Title
	})
	selected, seen, urls := []fuzzyCandidateRecord{}, map[string]bool{}, map[string]bool{}
	for _, item := range all {
		v := item.record.View
		key := fuzzyDigest([]string{v.Kind, v.Name, v.Year, v.Season, v.Episodes})
		if seen[key] || urls[item.record.Resource.DownloadURL] {
			continue
		}
		if len(selected) >= fuzzyCandidateLimit {
			return nil, errors.New("fuzzy subscription candidate limit exceeded; no truncated batch")
		}
		seen[key], urls[item.record.Resource.DownloadURL] = true, true
		selected = append(selected, item.record)
	}
	return selected, nil
}

// Legacy fuzzy rules can match unrecognized releases. TMDB enriches names
// when configured, but a missing ID/key is not permission to discard a match
// or to reinterpret the pattern itself as one TMDB media identity.
func (api mediaNameAPI) recognizeFuzzy(ctx context.Context, title, subtitle string) (*nativeRecognition, *recognitionFailure) {
	service := api.service
	_, words, err := service.words.List(ctx)
	if err != nil {
		return nil, &recognitionFailure{503, "custom words unavailable"}
	}
	name, err := wordconfig.ProcessWords(ctx, title, words)
	if err != nil {
		return nil, &recognitionFailure{422, "invalid fuzzy resource title"}
	}
	sub, err := wordconfig.ProcessWords(ctx, subtitle, words)
	if err != nil {
		return nil, &recognitionFailure{422, "invalid fuzzy resource subtitle"}
	}
	options, err := mediameta.ReadLabelOptions(ctx, service.systemConfig)
	if err != nil {
		return nil, &recognitionFailure{503, "media labels unavailable"}
	}
	meta, err := mediameta.ParseWithOptions(ctx, name.Title, sub.Title, options)
	if err != nil {
		return nil, &recognitionFailure{422, "fuzzy resource could not be parsed"}
	}
	if meta.Title == "" {
		return nil, nil
	}
	result := &nativeRecognition{meta: meta, subtitle: sub.Title, data: map[string]any{"rev_string": name.Title}}
	snapshot, err := service.configStore.Snapshot()
	if err != nil {
		return nil, &recognitionFailure{503, "media configuration unavailable"}
	}
	if strings.TrimSpace(text(objectValue(snapshot["app"])["rmt_tmdbkey"])) != "" {
		known, failure := api.recognizeKind(ctx, title, subtitle, "")
		if ctx.Err() != nil {
			return nil, &recognitionFailure{504, "fuzzy recognition cancelled"}
		}
		if failure != nil {
			return nil, failure
		}
		if failure == nil && known != nil {
			result = known
			result.meta.Title, result.meta.Year = text(known.data["title"]), text(known.data["year"])
		}
	}
	return result, nil
}
