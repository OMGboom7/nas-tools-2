package httpserver

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

func (api organizationAPI) serveLegacyRenamePreview(w http.ResponseWriter, r *http.Request) {
	api.serveLegacyIdentificationPreview(w, r, true)
}
func (api organizationAPI) serveLegacyCustomPreview(w http.ResponseWriter, r *http.Request) {
	api.serveLegacyIdentificationPreview(w, r, false)
}
func (api organizationAPI) serveLegacyIdentificationPreview(w http.ResponseWriter, r *http.Request, linked bool) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	input, ok := decodeLegacyIdentification(w, r, linked)
	if !ok {
		return
	}
	var modeErr error
	input.Mode, modeErr = api.legacyRequestMode(input.Mode)
	if modeErr != nil {
		writeAPIError(w, 503, 503, "organization configuration unavailable")
		return
	}
	if !organization.SupportedMode(input.Mode) {
		writeAPIError(w, 501, 501, "configured default transfer mode is not migrated; no fallback")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid or expired authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	planner := api.legacyCustomPlan
	if linked {
		planner = api.legacyRenamePlan
	}
	plan, failure := planner(ctx, input, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	d := plan.definition
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"plan": plan, "explicitTarget": d.LegacyTargetOverride, "sourceRoot": d.SourceRoot, "targetRoot": d.TargetRoot, "origin": d.LegacyOrigin, "selection": d.LegacySelection, "options": d.LegacyRename, "unknown": d.Unknown, "history": d.History}})
}

// The legacy row is provenance, not authority for arbitrary filesystem paths.
// A new literal mode is explicit request intent; old values remain untouched.
func (api organizationAPI) legacyRenamePlan(ctx context.Context, request legacyCustomInput, actor string) (organizationPlan, *recognitionFailure) {
	fail := func(status int, message string) (organizationPlan, *recognitionFailure) {
		return organizationPlan{}, &recognitionFailure{status, message}
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return fail(503, "configured organization directories are unavailable")
	}
	input := organizationPlanInput{Mode: request.Mode, LegacySelection: &request.Selection, LegacyRename: &organization.LegacyRenameOptions{}, AllowProcessedUnknown: true}
	var dest string
	if request.RecordKind == "history" {
		record, err := api.jobs.GetHistory(ctx, request.RecordID)
		if err != nil {
			return fail(409, "historical original record unavailable; refresh before renaming")
		}
		projected := record
		projected.Mode = ""
		projection := projectHistory(projected, roots, request.Mode)
		if projection.Reason != "" {
			return fail(422, "historical original source or destination is outside configured scope")
		}
		input.SourceID, input.Path = projection.SourceID, projection.Relative
		input.HistoryRecord = &organizationHistorySelector{ID: record.ID, Fingerprint: organization.Digest(record)}
		dest = record.Dest
	} else if request.RecordKind == "unidentification" {
		record, err := api.jobs.GetUnknown(ctx, request.RecordID)
		if err != nil {
			return fail(409, "unknown record unavailable; refresh before renaming")
		}
		if record.State != "N" && record.State != "Y" {
			return fail(422, "unknown record has no supported state")
		}
		projected := record
		projected.Mode = ""
		projection := projectUnknown(projected, roots, request.Mode)
		if projection.Reason != "" {
			return fail(422, "unknown source or destination is outside configured scope")
		}
		input.SourceID, input.Path = projection.SourceID, projection.Relative
		input.UnknownRecord = &organizationUnknownSelector{ID: record.ID, Fingerprint: organization.Digest(record)}
		dest = record.Dest
	} else {
		return fail(422, "select historical or unknown provenance")
	}
	var source organizationRoot
	for _, root := range roots.Sources {
		if root.ID == input.SourceID {
			source = root
			break
		}
	}
	root, err := organization.OpenRoot(source.Path)
	if err != nil {
		return fail(422, "original source root is unavailable")
	}
	info, err := root.Lstat(input.Path)
	root.Close()
	if err != nil {
		return fail(422, "original source is missing or unavailable; published media is not a substitute")
	}
	if info.Mode().IsRegular() && request.Selection.EpisodeFormat != "" {
		if !request.ConfirmParentScope {
			return fail(400, "filename episode templates expand a recorded file to its parent; separate confirmParentScope consent is required")
		}
		scan, err := organization.Scan(ctx, source.Path, input.Path)
		if err != nil || scan.Skipped != 0 || len(scan.Files) != 1 || scan.Files[0].Kind != "media" {
			return fail(422, "only an exact safe media file may expand to its parent")
		}
		input.Path = filepath.Dir(input.Path)
		input.LegacyRename.ExpandedScope = true
		input.LegacyRename.PreserveUnknown = input.UnknownRecord != nil
	} else if request.ConfirmParentScope {
		return fail(400, "parent-scope consent only applies to a recorded single media file with an episode template")
	}
	return api.legacyIdentificationTargets(ctx, roots, input, dest, &organization.LegacyOrganizationOrigin{Action: "rename", Kind: request.RecordKind, RecordID: strconv.FormatInt(request.RecordID, 10), Actor: actor})
}
