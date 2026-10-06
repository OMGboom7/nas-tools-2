// Package mediameta parses release metadata without resolving online identities.
package mediameta

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

type EpisodeInfo struct {
	TV                                     bool
	Count                                  int
	Season, EndSeason, Episode, EndEpisode *int
}

var (
	seasonPattern             = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])S([0-9]{1,2})(?:[ .]*-[ .]*S([0-9]{1,2}))?(?:E|[^a-z0-9]|$)`)
	englishSeasonPattern      = regexp.MustCompile(`(?i)\bSeason\s+([0-9]{1,2})\b`)
	chineseSeasonRangePattern = regexp.MustCompile(`第\s*([0-9零〇一二两三四五六七八九十百千]+)(?:\s*-\s*([0-9零〇一二两三四五六七八九十百千]+))?\s*季`)
	episodePattern            = regexp.MustCompile(`(?i)(?:^|[^a-z0-9]|S[0-9]{1,2})EP?([0-9]{1,4})(?:[ .]*-[ .]*(?:EP?)?([0-9]{1,4}))?(?:[^a-z0-9]|$)`)
	chainedEpisodePattern     = regexp.MustCompile(`(?i)(?:^|[^a-z0-9]|S[0-9]{1,2})EP?([0-9]{1,4})(?:EP?([0-9]{1,4}))+(?:[^a-z0-9]|$)`)
	chainNumberPattern        = regexp.MustCompile(`(?i)EP?([0-9]{1,4})`)
	englishEpisodePattern     = regexp.MustCompile(`(?i)\bEpisode\s+([0-9]{1,4})(?:\s*-\s*([0-9]{1,4}))?\b`)
	bracketEpisodePattern     = regexp.MustCompile(`(?i)[\[【](?:TV\s*)?((?:0[0-9]{1,3}|[0-9]{1,3}))(?:\s*-\s*((?:0[0-9]{1,3}|[0-9]{1,3})))?(?:v[0-9]+)?[\]】]`)
	chineseSeasonPattern      = regexp.MustCompile(`(?:第|全|共)\s*[0-9零〇一二两三四五六七八九十百千]+\s*季|[0-9零〇一二两三四五六七八九十百千]+\s*季\s*全`)
	chineseEpisodePattern     = regexp.MustCompile(`第\s*([0-9零〇一二两三四五六七八九十百千]+)(?:\s*-\s*([0-9零〇一二两三四五六七八九十百千]+))?\s*[集话話期]`)
	completeEpisodePattern    = regexp.MustCompile(`(?:全|共)\s*([0-9零〇一二两三四五六七八九十百千]+)\s*[集话話期]|([0-9零〇一二两三四五六七八九十百千]+)\s*集\s*全`)
	animeEpisodePattern       = regexp.MustCompile(`(?i)\s+-\s+([0-9]{1,4})(?:\s*-\s*([0-9]{1,4}))?(?:v[0-9]+)?(?:\s|\[|$)`)
)

// Episodes distinguishes movie totals from TV per-episode sizes. A season pack
// without an explicit episode count stays unknown rather than guessing a count.
func Episodes(title, subtitle string) (EpisodeInfo, error) {
	if len(title)+len(subtitle) > 128<<10 {
		return EpisodeInfo{}, errors.New("release metadata too large")
	}
	info := EpisodeInfo{}
	for _, input := range []string{title, subtitle} {
		if info.Season == nil {
			season := seasonPattern.FindStringSubmatch(input)
			if season == nil {
				season = chineseSeasonRangePattern.FindStringSubmatch(input)
			}
			if season == nil {
				if match := englishSeasonPattern.FindStringSubmatch(input); match != nil {
					season = append(match, "")
				}
			}
			if season != nil {
				begin, err := episodeNumber(season[1])
				if err != nil {
					return info, err
				}
				info.Season, info.TV = &begin, true
				if season[2] != "" {
					end, err := episodeNumber(season[2])
					if err != nil || end < begin {
						return info, errors.New("invalid season range")
					}
					info.EndSeason = &end
				}
			}
		}
		if seasonPattern.MatchString(input) || chineseSeasonPattern.MatchString(input) {
			info.TV = true
		}
		if info.Count > 0 {
			continue
		}
		match := chainedEpisodePattern.FindStringSubmatch(input)
		if match != nil {
			previous := -1
			for _, number := range chainNumberPattern.FindAllStringSubmatch(match[0], -1) {
				value, err := episodeNumber(number[1])
				if err != nil || value < previous {
					return info, errors.New("invalid episode sequence")
				}
				previous = value
			}
		}
		if match == nil {
			match = episodePattern.FindStringSubmatch(input)
		}
		if match == nil {
			match = chineseEpisodePattern.FindStringSubmatch(input)
		}
		if match == nil {
			match = englishEpisodePattern.FindStringSubmatch(input)
		}
		if match == nil {
			match = bracketEpisodePattern.FindStringSubmatch(input)
		}
		if match == nil {
			match = animeEpisodePattern.FindStringSubmatch(input)
		}
		if match != nil {
			begin, err := episodeNumber(match[1])
			if err != nil {
				return info, err
			}
			end := begin
			if match[2] != "" {
				end, err = episodeNumber(match[2])
			}
			if err != nil || end < begin {
				return info, errors.New("invalid episode range")
			}
			info.TV, info.Count = true, end-begin+1
			info.Episode = &begin
			if end != begin {
				info.EndEpisode = &end
			}
			continue
		}
		if match := completeEpisodePattern.FindStringSubmatch(input); match != nil {
			raw := match[1]
			if raw == "" {
				raw = match[2]
			}
			count, err := episodeNumber(raw)
			if err != nil || count == 0 {
				return info, errors.New("invalid total episode count")
			}
			info.TV, info.Count = true, count
			continue
		}
	}
	return info, nil
}

func episodeNumber(raw string) (int, error) {
	if value, err := strconv.Atoi(raw); err == nil {
		if value >= 0 && value <= 10000 {
			return value, nil
		}
		return 0, errors.New("episode number out of range")
	}
	if len(raw) > 48 {
		return 0, errors.New("episode number too large")
	}
	digits := map[rune]int{'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	if !strings.ContainsAny(raw, "十百千") {
		value := 0
		for _, character := range raw {
			digit, ok := digits[character]
			if !ok {
				return 0, errors.New("invalid episode numeral")
			}
			value = value*10 + digit
			if value > 10000 {
				return 0, errors.New("episode number out of range")
			}
		}
		return value, nil
	}
	value, digit, previousUnit := 0, 0, 10000
	for _, character := range raw {
		if number, ok := digits[character]; ok {
			digit = number
			continue
		}
		unit := map[rune]int{'十': 10, '百': 100, '千': 1000}[character]
		if unit == 0 || unit >= previousUnit {
			return 0, errors.New("invalid episode numeral")
		}
		if digit == 0 {
			digit = 1
		}
		value += digit * unit
		digit, previousUnit = 0, unit
	}
	return value + digit, nil
}
