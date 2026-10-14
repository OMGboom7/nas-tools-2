package httpserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func legacyEpisodeDetails(value string) (int, *int, bool, error) {
	if value == "" {
		return 0, nil, false, nil
	}
	separator := "-"
	filter := false
	if strings.Contains(value, ",") {
		separator = ","
		filter = true
	}
	parts := strings.Split(value, separator)
	if len(parts) > 2 {
		return 0, nil, false, errors.New("invalid legacy episode details")
	}
	parse := func(text string) (int, error) {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > 9999 {
			return 0, errors.New("invalid legacy episode details")
		}
		return n, nil
	}
	start, err := parse(parts[0])
	if err != nil {
		return 0, nil, false, err
	}
	var end *int
	if len(parts) == 2 {
		n, err := parse(parts[1])
		if err != nil || n < start {
			return 0, nil, false, errors.New("invalid legacy episode details range")
		}
		end = &n
	}
	return start, end, filter, nil
}

func validateLegacySelection(choice *organization.LegacyMediaSelection) error {
	if choice == nil {
		return nil
	}
	if choice.Kind != "" && choice.Kind != "movie" && choice.Kind != "tv" {
		return errors.New("invalid legacy media type")
	}
	if choice.TMDBID != "" {
		id, err := strconv.ParseInt(choice.TMDBID, 10, 64)
		if err != nil || id < 1 || strconv.FormatInt(id, 10) != choice.TMDBID {
			return errors.New("invalid legacy TMDB ID")
		}
	}
	if choice.Season != nil && (*choice.Season < 0 || *choice.Season > 99) || choice.EpisodeOffset < -99999 || choice.EpisodeOffset > 99999 || choice.MinimumMB != nil && (*choice.MinimumMB < 0 || *choice.MinimumMB > 1000000) {
		return errors.New("legacy media selection is out of bounds")
	}
	if _, _, filter, err := legacyEpisodeDetails(choice.EpisodeDetails); err != nil {
		return err
	} else if filter && choice.EpisodeFormat == "" {
		return errors.New("legacy episode filtering requires a filename template")
	}
	if choice.EpisodeFormat != "" {
		if _, err := mediameta.CompileEpisodeFormat(choice.EpisodeFormat); err != nil {
			return err
		}
	}
	if choice.PartOverride != nil {
		part, err := mediameta.NormalizePart(*choice.PartOverride)
		if err != nil {
			return err
		}
		choice.PartOverride = &part
	}
	if choice.Kind == "movie" && (choice.Season != nil || choice.EpisodeFormat != "" || choice.EpisodeDetails != "" || choice.EpisodeOffset != 0) {
		return errors.New("movie selection must not include TV season/episode rules")
	}
	return nil
}

func (api organizationAPI) legacySelectedRecognition(ctx context.Context, title string, choice *organization.LegacyMediaSelection) (*nativeRecognition, *recognitionFailure, bool) {
	start, end, filter, err := legacyEpisodeDetails(choice.EpisodeDetails)
	if err != nil {
		return nil, &recognitionFailure{422, err.Error()}, false
	}
	var extracted int
	var extractedEnd *int
	if choice.EpisodeFormat != "" {
		compiled, err := mediameta.CompileEpisodeFormat(choice.EpisodeFormat)
		if err != nil {
			return nil, &recognitionFailure{422, err.Error()}, false
		}
		extracted, extractedEnd, err = compiled.EpisodesContext(ctx, title, 0)
		if errors.Is(err, mediameta.ErrEpisodeTemplateNoMatch) {
			return nil, nil, true
		}
		if err != nil {
			return nil, &recognitionFailure{422, err.Error()}, false
		}
		// Filtering is based on the original extracted start, before offset.
		if filter && (extracted < start || end != nil && extracted > *end) {
			return nil, nil, true
		}
	}
	var selectedEpisodes *mediameta.EpisodeInfo
	if choice.Kind == "movie" {
		selectedEpisodes = &mediameta.EpisodeInfo{}
	}
	recognition, failure := api.recognition.recognizeSelected(ctx, title, "", choice.Kind, choice.TMDBID, selectedEpisodes, choice.PartOverride)
	if failure != nil || recognition == nil {
		return recognition, failure, false
	}
	if recognition.kind == "movie" {
		if choice.Season != nil || choice.EpisodeFormat != "" || choice.EpisodeDetails != "" || choice.EpisodeOffset != 0 {
			return nil, &recognitionFailure{422, "TV episode rules cannot be applied to an automatically recognized movie"}, false
		}
		return recognition, nil, false
	}
	e := recognition.meta.Episodes
	if choice.Season != nil {
		e.Season = choice.Season
		e.EndSeason = nil
	}
	if e.Season == nil {
		season := 1
		e.Season = &season
	} // Legacy TV/anime default, still checked against actual TMDB seasons.
	if start != 0 && !filter {
		e.Episode = &start
		e.EndEpisode = end
	} else if extracted != 0 {
		e.Episode = &extracted
		e.EndEpisode = extractedEnd
	}
	if e.Episode == nil {
		return nil, &recognitionFailure{422, "legacy TV source has no verified episode number"}, false
	}
	first, last, err := mediameta.OffsetEpisodes(*e.Episode, e.EndEpisode, choice.EpisodeOffset)
	if err != nil {
		return nil, &recognitionFailure{422, err.Error()}, false
	}
	e.Episode, e.EndEpisode, e.Count = &first, last, 1
	if last != nil {
		e.Count = *last - first + 1
	}
	recognition.meta.Episodes = e
	se := fmt.Sprintf("S%02dE%02d", *e.Season, first)
	if last != nil {
		se += fmt.Sprintf("-E%02d", *last)
	}
	recognition.data["season_episode"] = se
	return recognition, nil, false
}
