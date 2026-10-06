package httpserver

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/dlclark/regexp2"
)

type subscriptionCandidate struct {
	Title    string `json:"title"`
	Site     string `json:"site"`
	Priority int    `json:"priority"`
	Episodes []int  `json:"episodes,omitempty"`
	resource externalindexer.Resource
}

type subscriptionCandidateInput struct {
	TV                                          bool
	Season, Total                               int
	Missing                                     []int
	Group                                       int64
	Quality, Resolution, Team, Include, Exclude string
}

type identifiedSubscriptionResource struct {
	resource externalindexer.Resource
	meta     mediameta.Metadata
	revised  string
	subtitle string
}

var candidateEpisodeChain = regexp.MustCompile(`(?i)(?:s[0-9]{1,2})?(e[0-9]{1,4}(?:e[0-9]{1,4})+)`)
var candidateEpisodeNumber = regexp.MustCompile(`(?i)e([0-9]{1,4})`)

// The caller verifies TMDB identity first. Private URLs stay in unexported
// fields; planning never claims a resource or changes download progress.
func planSubscriptionCandidates(ctx context.Context, input subscriptionCandidateInput, groups []filterconfig.GroupInfo, resources []identifiedSubscriptionResource) ([]subscriptionCandidate, []int, error) {
	patterns := []*regexp2.Regexp{}
	quality := map[string]string{"BLURAY": `Blu-?Ray|BD|BDRIP`, "REMUX": `REMUX`, "DOLBY": `DOLBY|DOVI|\s+DV$|\s+DV\s+`, "WEB": `WEB-?DL|WEBRIP`, "HDTV": `U?HDTV`, "UHD": `UHD`, "HDR": `HDR`, "3D": `3D`}
	resolution := map[string]string{"8k": `8K`, "4k": `4K|2160P|X2160`, "1080p": `1080[PIX]|X1080`, "720p": `720P`}
	q, p := quality[input.Quality], resolution[input.Resolution]
	if input.Quality != "" && q == "" || input.Resolution != "" && p == "" {
		return nil, nil, errors.New("unsupported subscription quality or resolution")
	}
	for _, raw := range []string{q, p, input.Team, input.Include, input.Exclude} {
		var pattern *regexp2.Regexp
		if raw != "" {
			if len(raw) > 16<<10 {
				return nil, nil, errors.New("subscription pattern exceeded limit")
			}
			var err error
			pattern, err = regexcompat.Compile(raw, regexp2.IgnoreCase)
			if err != nil {
				return nil, nil, err
			}
		}
		patterns = append(patterns, pattern)
	}
	if _, err := filterconfig.MatchGroups(groups, input.Group, filterconfig.TorrentMetadata{RequireKnown: true}); err != nil {
		return nil, nil, err
	}
	remaining := map[int]bool{}
	if input.TV {
		if input.Season < 0 || input.Season > 1000 || input.Total <= 0 || input.Total > 10000 {
			return nil, nil, errors.New("invalid subscription season totals")
		}
		for _, n := range input.Missing {
			if n < 1 || n > input.Total || remaining[n] {
				return nil, nil, errors.New("invalid subscription missing episodes")
			}
			remaining[n] = true
		}
	}
	candidates := []subscriptionCandidate{}
	for _, item := range resources {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if item.meta.Episodes.TV != input.TV || !rssDownloadURLAllowed(item.resource.DownloadURL) {
			continue
		}
		texts := []string{item.meta.Source + " " + item.meta.Effect, item.meta.Resolution, item.meta.Team, item.revised + " " + item.subtitle, item.revised + " " + item.subtitle}
		if texts[2] == "" {
			texts[2] = item.revised
		} // Legacy release-group fallback searches the processed title.
		matched := true
		for i, pattern := range patterns {
			if pattern == nil {
				continue
			}
			found, err := pattern.MatchString(texts[i])
			if err != nil {
				return nil, nil, err
			}
			matched = matched && (found != (i == 4))
		}
		if !matched {
			continue
		}
		episodes := []int{}
		episodeCount := item.meta.Episodes.Count
		if input.TV {
			info := item.meta.Episodes
			season := 1
			if info.Season != nil {
				season = *info.Season
			}
			if season != input.Season || info.EndSeason != nil && *info.EndSeason != season {
				continue
			}
			begin, end := 1, input.Total
			if info.Episode != nil {
				begin, end = *info.Episode, *info.Episode
				if info.EndEpisode != nil {
					end = *info.EndEpisode
				}
			} else if info.Count > 0 {
				end = info.Count
			} else if info.Season == nil {
				continue
			}
			if begin < 1 || end < begin || end > input.Total {
				continue
			}
			chain := ""
			if info.Episode != nil {
				chain = candidateEpisodeChain.FindString(item.revised)
				if chain == "" {
					chain = candidateEpisodeChain.FindString(item.subtitle)
				}
			}
			if chain != "" {
				seen := map[int]bool{}
				for _, match := range candidateEpisodeNumber.FindAllStringSubmatch(chain, -1) {
					n, err := strconv.Atoi(match[1])
					if err != nil || n < 1 || n > input.Total || seen[n] {
						return nil, nil, errors.New("invalid explicit episode chain")
					}
					seen[n] = true
					if remaining[n] {
						episodes = append(episodes, n)
					}
				}
				episodeCount = len(seen)
			} else {
				for n := begin; n <= end; n++ {
					if remaining[n] {
						episodes = append(episodes, n)
					}
				}
			}
			if len(episodes) == 0 {
				continue
			}
		}
		result, err := filterconfig.MatchGroups(groups, input.Group, filterconfig.TorrentMetadata{Title: item.revised, Subtitle: item.subtitle, SizeBytes: float64(item.resource.Size), Movie: !input.TV, Episodes: episodeCount, UploadFactor: item.resource.UploadFactor, DownloadFactor: item.resource.DownloadFactor, RequireKnown: true})
		if err != nil {
			return nil, nil, err
		}
		if !result.Matched {
			continue
		}
		candidates = append(candidates, subscriptionCandidate{Title: item.resource.Title, Site: item.resource.Indexer, Priority: result.Order, Episodes: episodes, resource: item.resource})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		if len(a.Episodes) != len(b.Episodes) {
			return len(a.Episodes) > len(b.Episodes)
		}
		if (a.resource.Seeders != nil) != (b.resource.Seeders != nil) {
			return a.resource.Seeders != nil
		}
		if a.resource.Seeders != nil && *a.resource.Seeders != *b.resource.Seeders {
			return *a.resource.Seeders > *b.resource.Seeders
		}
		return strings.Compare(a.Title, b.Title) < 0
	})
	selected := []subscriptionCandidate{}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate.resource.DownloadURL] {
			continue
		}
		seen[candidate.resource.DownloadURL] = true
		if !input.TV {
			selected = append(selected, candidate)
			break
		}
		needed := []int{}
		for _, n := range candidate.Episodes {
			if remaining[n] {
				needed = append(needed, n)
			}
		}
		if len(needed) == 0 {
			continue
		}
		candidate.Episodes = needed
		selected = append(selected, candidate)
		for _, n := range needed {
			delete(remaining, n)
		}
	}
	missing := []int{}
	for n := range remaining {
		missing = append(missing, n)
	}
	sort.Ints(missing)
	return selected, missing, nil
}
