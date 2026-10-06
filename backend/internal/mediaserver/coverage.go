package mediaserver

import (
	"errors"
	"sort"
)

var ErrUnknownCoverage = errors.New("season episode totals are unavailable")

type CoverageSelection struct {
	TV                bool
	Seasons, Episodes []int
	SeasonTotals      map[int]int
}

type Coverage struct {
	Complete bool
	Missing  map[int][]int
}

// Coverage checks only verified library inventory. It does not treat in-flight
// download tasks as library media or infer complete seasons from a single item.
func (inventory Inventory) Coverage(selection CoverageSelection) (Coverage, error) {
	result := Coverage{Missing: map[int][]int{}}
	if !selection.TV {
		result.Complete = len(inventory.ItemIDs) > 0
		return result, nil
	}
	if len(selection.Seasons) > 1000 || len(selection.Episodes) > 10000 || len(selection.SeasonTotals) > 1000 {
		return result, ErrConfiguration
	}
	seasons := append([]int(nil), selection.Seasons...)
	if len(seasons) == 0 {
		if len(selection.Episodes) > 0 {
			seasons = []int{1}
		} else {
			for season := range selection.SeasonTotals {
				if season > 0 {
					seasons = append(seasons, season)
				}
			}
		}
	}
	if len(seasons) == 0 {
		return result, ErrUnknownCoverage
	}
	sort.Ints(seasons)
	seen := map[int]bool{}
	result.Complete = true
	checked := 0
	for _, season := range seasons {
		if season < 0 || season > 10000 {
			return Coverage{}, ErrConfiguration
		}
		if seen[season] {
			continue
		}
		seen[season] = true
		episodes := append([]int(nil), selection.Episodes...)
		if len(episodes) == 0 {
			total, known := selection.SeasonTotals[season]
			if !known || total <= 0 {
				return Coverage{}, ErrUnknownCoverage
			}
			if total > 10000 {
				return Coverage{}, ErrConfiguration
			}
			for number := 1; number <= total; number++ {
				episodes = append(episodes, number)
			}
		}
		checked += len(episodes)
		if checked > 100000 {
			return Coverage{}, ErrConfiguration
		}
		sort.Ints(episodes)
		seenEpisodes := map[int]bool{}
		for _, episode := range episodes {
			if episode < 0 || episode > 10000 {
				return Coverage{}, ErrConfiguration
			}
			if seenEpisodes[episode] {
				continue
			}
			seenEpisodes[episode] = true
			if len(inventory.ItemIDs) == 0 || !inventory.Episodes[season][episode] {
				result.Complete = false
				result.Missing[season] = append(result.Missing[season], episode)
			}
		}
	}
	return result, nil
}
