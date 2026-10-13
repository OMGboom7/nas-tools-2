package httpserver

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type organizationEpisodeDisposition struct{ Status, Reason string }

func organizationEpisodeSelection(ctx context.Context, input *organization.EpisodeSelection, files []organization.File, individual []organizationMediaAssignment) (*organization.EpisodeSelection, map[string]organizationMediaAssignment, map[string]organizationEpisodeDisposition, *recognitionFailure) {
	selected := map[string]organizationMediaAssignment{}
	excluded := map[string]organizationEpisodeDisposition{}
	if input == nil {
		return nil, selected, excluded, nil
	}
	fail := func(message string) (*organization.EpisodeSelection, map[string]organizationMediaAssignment, map[string]organizationEpisodeDisposition, *recognitionFailure) {
		return nil, nil, nil, &recognitionFailure{422, message}
	}
	if len(individual) != 0 {
		return fail("batch episode selection cannot be combined with individual assignments")
	}
	rule := *input
	id, err := strconv.ParseInt(rule.TMDBID, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != rule.TMDBID || rule.Season == nil || *rule.Season < 0 || *rule.Season > 99 || rule.EpisodeOffset < -99999 || rule.EpisodeOffset > 99999 {
		return fail("invalid batch TV identity, season or offset")
	}
	if (rule.StartEpisode == nil) != (rule.EndEpisode == nil) {
		return fail("batch episode filter requires both start and end")
	}
	if rule.StartEpisode != nil && (*rule.StartEpisode < 1 || *rule.EndEpisode < *rule.StartEpisode || *rule.EndEpisode > 9999) {
		return fail("invalid batch episode filter range")
	}
	if rule.PartOverride != nil {
		part, err := mediameta.NormalizePart(*rule.PartOverride)
		if err != nil {
			return fail(err.Error())
		}
		rule.PartOverride = &part
	}
	compiled, err := mediameta.CompileEpisodeFormat(rule.EpisodeFormat)
	if err != nil {
		return fail(err.Error())
	}
	for _, file := range files {
		if ctx.Err() != nil {
			return nil, nil, nil, &recognitionFailure{504, "organization episode selection was cancelled or timed out"}
		}
		if file.Kind != "media" {
			continue
		}
		start, end, err := compiled.EpisodesContext(ctx, filepath.Base(file.Path), 0)
		if ctx.Err() != nil {
			return nil, nil, nil, &recognitionFailure{504, "organization episode selection was cancelled or timed out"}
		}
		if err != nil {
			status := "blocked"
			if errors.Is(err, mediameta.ErrEpisodeTemplateNoMatch) {
				status = "filtered"
			}
			excluded[file.Path] = organizationEpisodeDisposition{status, err.Error()}
			continue
		}
		// Legacy range filtering uses the extracted start, BEFORE offset.
		// A multi-episode file is indivisible, so its full range is retained.
		if rule.StartEpisode != nil && (start < *rule.StartEpisode || start > *rule.EndEpisode) {
			excluded[file.Path] = organizationEpisodeDisposition{"filtered", "source episode is outside the selected pre-offset range"}
			continue
		}
		start, end, err = mediameta.OffsetEpisodes(start, end, rule.EpisodeOffset)
		if err != nil {
			excluded[file.Path] = organizationEpisodeDisposition{"blocked", err.Error()}
			continue
		}
		selected[file.Path] = organizationMediaAssignment{Source: file.Path, Identity: file.Identity, Modified: strconv.FormatInt(file.Modified, 10), Size: file.Size, Kind: "tv", TMDBID: rule.TMDBID, Season: rule.Season, Episode: &start, EndEpisode: end, EpisodeFormat: rule.EpisodeFormat, EpisodeOffset: rule.EpisodeOffset, PartOverride: rule.PartOverride}
	}
	return &rule, selected, excluded, nil
}
