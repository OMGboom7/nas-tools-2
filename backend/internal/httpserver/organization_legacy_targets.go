package httpserver

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type organizationPartition struct {
	TargetID   string                 `json:"targetId"`
	TargetRoot string                 `json:"targetRoot"`
	Items      []organizationPlanItem `json:"items"`
}

func planDefinitions(p organizationPlan) []organization.Definition {
	if len(p.partitions) == 0 {
		return []organization.Definition{p.definition}
	}
	defs := []organization.Definition{}
	for _, part := range p.partitions {
		defs = append(defs, part.definition)
	}
	return defs
}
func legacyCompanion(media, source string) bool {
	stem := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
	return filepath.Dir(media) == filepath.Dir(source) && strings.HasPrefix(filepath.Base(source), stem+".")
}
func legacySourceFacts(p organizationPlan) string {
	rows := []any{p.definition.SourceID, p.definition.SourceRoot, p.definition.SourceIdentity, p.definition.ConfigDigest, p.definition.UnknownScope, p.definition.UnknownScopeIdentity}
	for _, item := range p.Items {
		identity := []any{item.Source, item.Kind, item.Identity, item.Modified, item.Size, item.Disc, item.Status == "filtered"}
		if item.Kind == "media" {
			identity = append(identity, item.TMDBID, item.Title, item.Year, item.MediaType, item.Category, item.SeasonEpisode, item.Part, item.Assigned, item.EpisodeFormat, item.EpisodeOffset, item.PartOverride)
		}
		rows = append(rows, identity)
	}
	return organization.Digest(rows)
}
func finalizeLegacyPlan(p *organizationPlan, origin *organization.LegacyOrganizationOrigin, preserve bool) {
	p.definition.LegacyOrigin = origin
	if p.definition.LegacyRename != nil {
		copy := *p.definition.LegacyRename
		copy.PreserveUnknown = copy.PreserveUnknown || preserve && p.definition.Unknown != nil
		p.definition.LegacyRename = &copy
	}
	p.Fingerprint = organization.Digest(struct {
		Definition organization.Definition
		Items      []organizationPlanItem
	}{p.definition, p.Items})
}

// Every scanned source is represented exactly once. Configured roots select
// verified media types; sidecars stay with their one primary, never by guessing
// a target from an old history row or accepting a successful media subset.
func (api organizationAPI) legacyTargetPlans(ctx context.Context, roots organizationRoots, input organizationPlanInput, dest string, origin *organization.LegacyOrganizationOrigin, allowFiltered bool) (organizationPlan, *recognitionFailure) {
	fail := func(message string) (organizationPlan, *recognitionFailure) {
		return organizationPlan{}, &recognitionFailure{422, message}
	}
	input.LegacyTargetOverride = dest != ""
	candidates := []organizationPlan{}
	var last *recognitionFailure
	stamp := ""
	for _, target := range roots.Targets {
		if dest != "" && !organizationTargetMatches(target.Path, ".", dest, input.Mode) || dest == "" && target.Type == "" {
			continue
		}
		input.TargetID = target.ID
		p, f := api.plan(ctx, input)
		if f != nil {
			if ctx.Err() != nil {
				return p, f
			}
			last = f
			continue
		}
		p.definition.LegacyScope = input.Path
		if p.Skipped != 0 {
			return fail("the complete original source contains skipped/unsafe items; no partial record success")
		}
		current := legacySourceFacts(p)
		if stamp != "" && stamp != current {
			return organizationPlan{}, &recognitionFailure{409, "source, identity, filtering or configuration changed across target previews"}
		}
		stamp = current
		candidates = append(candidates, p)
		if dest != "" && len(p.definition.Entries) > 0 {
			complete := true
			for _, item := range p.Items {
				complete = complete && (item.Status == "available" || allowFiltered && item.Status == "filtered")
			}
			if complete {
				break
			}
		}
	}
	if len(candidates) == 0 {
		if last != nil {
			return organizationPlan{}, last
		}
		return fail("no compatible configured destination")
	}
	covered := map[string]bool{}
	filtered := map[string]organizationPlanItem{}
	parts := []organizationPlan{}
	all := []organizationPlanItem{}
	for _, candidate := range candidates {
		selected := map[string]bool{}
		for _, item := range candidate.Items {
			if item.Status == "filtered" {
				if !allowFiltered {
					return fail("full re-identification must not silently discard filtered source items")
				}
				filtered[item.Source] = item
			}
			if item.Kind == "media" && item.Status == "available" && !covered[item.Source] {
				selected[item.Source] = true
			}
		}
		for _, item := range candidate.Items {
			if item.Kind == "media" || item.Status == "available" || item.Status == "filtered" {
				continue
			}
			for media := range selected {
				if legacyCompanion(media, item.Source) {
					return fail("a companion is ambiguous, conflicting or unavailable; review the full media group")
				}
			}
		}
		entries := []organization.Entry{}
		items := []organizationPlanItem{}
		chosen := map[string]bool{}
		for _, entry := range candidate.definition.Entries {
			take := selected[entry.Source]
			if entry.Kind != "media" {
				for media := range selected {
					take = take || legacyCompanion(media, entry.Source)
				}
			}
			if take {
				if covered[entry.Source] {
					return fail("a source would be assigned to multiple targets")
				}
				entries = append(entries, entry)
				chosen[entry.Source] = true
				covered[entry.Source] = true
			}
		}
		if len(entries) == 0 {
			continue
		}
		for _, item := range candidate.Items {
			if chosen[item.Source] {
				items = append(items, item)
				all = append(all, item)
			}
		}
		candidate.definition.Entries = entries
		candidate.Items = items
		parts = append(parts, candidate)
	}
	for _, item := range candidates[0].Items {
		if covered[item.Source] {
			continue
		}
		if f, ok := filtered[item.Source]; ok {
			all = append(all, f)
			continue
		}
		return fail("every original media and companion must have one verified destination; no partial success")
	}
	if len(parts) == 0 {
		return fail("no executable media remains in the reviewed scope")
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Source < all[j].Source })
	for i := range parts {
		finalizeLegacyPlan(&parts[i], origin, len(filtered) > 0)
	}
	if len(parts) == 1 {
		p := parts[0]
		p.Items = all
		finalizeLegacyPlan(&p, origin, len(filtered) > 0)
		return p, nil
	}
	result := organizationPlan{PreviewOnly: true, Mode: input.Mode, Items: all, definition: parts[0].definition, partitions: parts, ScraperEnabled: parts[0].ScraperEnabled, Remote: parts[0].Remote}
	if result.Remote != nil {
		copy := *result.Remote
		result.Remote = &copy
	}
	for _, p := range parts {
		result.Partitions = append(result.Partitions, organizationPartition{TargetID: p.definition.TargetID, TargetRoot: p.definition.TargetRoot, Items: p.Items})
		if p.Remote != nil && !p.Remote.Directories {
			result.Remote.Directories = false
		}
	}
	result.Fingerprint = organization.Digest(struct {
		Definitions []organization.Definition
		Items       []organizationPlanItem
		Partitions  []organizationPartition
	}{planDefinitions(result), all, result.Partitions})
	return result, nil
}
