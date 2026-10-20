package httpserver

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type downloadOrganizationReview struct {
	Completion  downloadCompletionReview `json:"completion"`
	Plans       []organizationPlan       `json:"plans"`
	Omitted     []organization.File      `json:"omitted"`
	Fingerprint string                   `json:"fingerprint"`
	origin      organization.DownloadOrigin
}

func (api organizationAPI) planDownload(ctx context.Context, input downloadCompletionInput, actor string) (downloadOrganizationReview, *recognitionFailure) {
	fail := func(status int, message string) (downloadOrganizationReview, *recognitionFailure) {
		return downloadOrganizationReview{}, &recognitionFailure{status, message}
	}
	completion, failure := api.reviewDownloadCompletion(ctx, input, actor)
	if failure != nil {
		return downloadOrganizationReview{}, failure
	}
	if !completion.Eligible {
		return fail(422, "download completion evidence is not eligible; use the readonly evidence review")
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return fail(503, "configured media roots unavailable")
	}
	o := organization.DownloadOrigin{DownloaderID: completion.DownloaderID, DownloaderName: completion.DownloaderName, DownloaderType: completion.DownloaderType, ConfigDigest: completion.configDigest, SourceRoot: completion.SourceRoot, SourceIdentity: completion.RootIdentity, SourceID: subscriptionResourceKey("source:" + filepath.Clean(completion.Location.Root) + ":"), Actor: actor, Task: completion.Task, Files: completion.Files}
	result := downloadOrganizationReview{Completion: completion, Plans: []organizationPlan{}, Omitted: []organization.File{}, origin: o}
	covered := map[string]bool{}
	filtered := map[string]bool{}
	inputPlan := organizationPlanInput{SourceID: o.SourceID, Path: ".", Mode: completion.Mode, Download: &o}
	for _, target := range roots.Targets {
		if target.Type == "" {
			continue
		}
		inputPlan.TargetID = target.ID
		plan, failure := api.plan(ctx, inputPlan)
		if failure != nil {
			if ctx.Err() != nil {
				return fail(504, "download organization planning interrupted")
			}
			continue
		}
		selected := map[string]bool{}
		for _, item := range plan.Items {
			if item.Status == "filtered" {
				filtered[item.Source] = true
				if item.Disc != nil {
					for _, node := range plan.definition.Discs[item.Source].Nodes {
						if !node.Directory {
							filtered[filepath.Join(item.Source, node.Path)] = true
						}
					}
				}
			}
			if item.Kind == "media" && item.Status == "available" && !covered[item.Source] {
				selected[item.Source] = true
			}
		}
		for _, item := range plan.Items {
			if item.Kind == "media" || item.Status == "available" || item.Status == "filtered" {
				continue
			}
			for media := range selected {
				stem := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
				if filepath.Dir(media) == filepath.Dir(item.Source) && strings.HasPrefix(filepath.Base(item.Source), stem+".") {
					return fail(422, "selected download companion is conflicting or ambiguous; no partial media group execution")
				}
			}
		}
		entries := []organization.Entry{}
		for _, entry := range plan.definition.Entries {
			take := selected[entry.Source]
			if entry.Kind != "media" {
				for media := range selected {
					stem := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
					if filepath.Dir(media) == filepath.Dir(entry.Source) && strings.HasPrefix(filepath.Base(entry.Source), stem+".") {
						take = true
					}
				}
			}
			if take {
				entries = append(entries, entry)
				covered[entry.Source] = true
				if entry.Disc != nil {
					for _, node := range plan.definition.Discs[entry.Source].Nodes {
						if !node.Directory {
							covered[filepath.Join(entry.Source, node.Path)] = true
						}
					}
				}
			}
		}
		if len(entries) == 0 {
			continue
		}
		plan.definition.Entries = entries
		plan.Fingerprint = organization.Digest(struct {
			Definition organization.Definition
			Items      []organizationPlanItem
		}{plan.definition, plan.Items})
		result.Plans = append(result.Plans, plan)
	}
	for _, file := range completion.Files {
		if covered[file.Path] {
			continue
		}
		if file.Kind == "media" && !filtered[file.Path] {
			return fail(422, "selected media requires identity/naming/conflict review; no fallback or partial success")
		}
		if (file.Kind == "subtitle" || file.Kind == "audio") && !filtered[file.Path] {
			return fail(422, "selected companion has no unique executable media; explicit correction required")
		}
		result.Omitted = append(result.Omitted, file)
	}
	if len(result.Plans) == 0 {
		return fail(422, "no verified executable selected media; do not mark this download organized")
	}
	result.Fingerprint = organization.Digest(result)
	return result, nil
}
