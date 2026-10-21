package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type legacyCustomInput struct {
	Source, Target, Mode       string
	RecordKind                 string
	RecordID                   int64
	ConfirmParentScope         bool
	ReviewFingerprint          string
	Selection                  organization.LegacyMediaSelection
	ConfirmSourceRemoval       bool
	ConfirmRemoteExclusive     bool
	ConfirmRemoteRecoveryCopy  bool
	ConfirmOldExecutorsStopped bool
}

// The old form and JSON clients use both strings and integers. Keep numbers
// exact; never route a database/TMDB ID through a floating-point decoder.
func decodeLegacyIdentification(w http.ResponseWriter, r *http.Request, linked bool) (legacyCustomInput, bool) {
	input := legacyCustomInput{}
	bad := func(message string) (legacyCustomInput, bool) {
		writeAPIError(w, 400, 400, message)
		return input, false
	}
	fields := map[string]string{}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media == "application/json" {
		var values map[string]json.RawMessage
		if !decodeOrganizationRequest(w, r, &values, "invalid custom identification request") {
			return input, false
		}
		for key, raw := range values {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				value = string(raw)
				if value == "null" || strings.ContainsAny(value, "{}[]") {
					return bad("invalid custom identification value")
				}
			}
			fields[key] = value
		}
	} else if media == "application/x-www-form-urlencoded" {
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if r.ParseForm() != nil {
			return bad("invalid custom identification form")
		}
		for key, values := range r.PostForm {
			if len(values) != 1 {
				return bad("duplicate custom identification field")
			}
			fields[key] = values[0]
		}
	} else {
		return bad("unsupported custom identification content type")
	}
	allowed := map[string]bool{}
	for _, key := range []string{"syncmod", "tmdb", "type", "season", "episode_format", "episode_details", "episode_offset", "episode_part", "min_filesize", "confirmSourceRemoval", "title", "year", "reviewFingerprint", "confirmRemoteExclusive", "confirmRemoteRecoveryCopy", "confirmOldExecutorsStopped"} {
		allowed[key] = true
	}
	if linked {
		allowed["logid"], allowed["unknown_id"], allowed["confirmParentScope"] = true, true, true
	} else {
		allowed["inpath"], allowed["outpath"] = true, true
	}
	for key := range fields {
		if !allowed[key] {
			return bad("unsupported custom identification field")
		}
	}
	input.Source, input.Target = fields["inpath"], fields["outpath"]
	if linked {
		id := fields["logid"]
		input.RecordKind = "history"
		if fields["unknown_id"] != "" {
			if id != "" {
				return bad("select exactly one historical or unknown record")
			}
			id = fields["unknown_id"]
			input.RecordKind = "unidentification"
		}
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n < 1 || strconv.FormatInt(n, 10) != id {
			return bad("select a canonical positive record ID")
		}
		input.RecordID = n
		if value, exists := fields["confirmParentScope"]; exists {
			if value != "true" && value != "false" {
				return bad("invalid parent-scope consent")
			}
			input.ConfirmParentScope = value == "true"
		}
	}
	input.ReviewFingerprint = fields["reviewFingerprint"]
	if input.ReviewFingerprint != "" && len(input.ReviewFingerprint) != 64 {
		return bad("invalid reviewed rename fingerprint")
	}
	input.Mode = transferModes[fields["syncmod"]]
	if input.Mode == "" && organization.SupportedMode(fields["syncmod"]) {
		input.Mode = fields["syncmod"]
	}
	if fields["syncmod"] != "" && !organization.SupportedMode(input.Mode) {
		writeAPIError(w, 501, 501, "transfer mode is not migrated; no fallback")
		return input, false
	}
	paths := []string{}
	if !linked {
		paths = []string{input.Source}
		if input.Target != "" {
			paths = append(paths, input.Target)
		}
	}
	for _, path := range paths {
		if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\\\x00\r\n") {
			return bad("select canonical absolute configured paths")
		}
	}
	if fields["title"] != "" || fields["year"] != "" {
		return bad("custom title/year are not authoritative; select a verified TMDB identity")
	}
	choice := &input.Selection
	switch fields["type"] {
	case "":
	case "MOV", "电影":
		choice.Kind = "movie"
	case "TV", "电视剧":
		choice.Kind = "tv"
	case "ANIME", "动漫":
		choice.Kind = "anime"
	default:
		return bad("invalid custom media type")
	}
	choice.TMDBID = fields["tmdb"]
	if choice.TMDBID != "" && choice.Kind == "" {
		return bad("explicit TMDB identity requires MOV, TV or ANIME type")
	}
	choice.EpisodeFormat, choice.EpisodeDetails = fields["episode_format"], fields["episode_details"]
	parse := func(key string) (int, error) { return strconv.Atoi(fields[key]) }
	if fields["season"] != "" {
		n, err := parse("season")
		if err != nil {
			return bad("invalid season")
		}
		choice.Season = &n
	}
	if fields["episode_offset"] != "" {
		n, err := parse("episode_offset")
		if err != nil {
			return bad("invalid episode offset")
		}
		choice.EpisodeOffset = n
	}
	if fields["min_filesize"] != "" {
		n, err := strconv.ParseInt(fields["min_filesize"], 10, 64)
		if err != nil {
			return bad("invalid minimum file size")
		}
		choice.MinimumMB = &n
	}
	if value, exists := fields["episode_part"]; exists {
		choice.PartOverride = &value
	}
	if value, exists := fields["confirmSourceRemoval"]; exists {
		if value != "true" && value != "false" {
			return bad("invalid source-removal consent")
		}
		input.ConfirmSourceRemoval = value == "true"
	}
	for key, destination := range map[string]*bool{"confirmRemoteExclusive": &input.ConfirmRemoteExclusive, "confirmRemoteRecoveryCopy": &input.ConfirmRemoteRecoveryCopy, "confirmOldExecutorsStopped": &input.ConfirmOldExecutorsStopped} {
		if value, exists := fields[key]; exists {
			if value != "true" && value != "false" {
				return bad("invalid remote transfer consent")
			}
			*destination = value == "true"
		}
	}
	if err := validateLegacySelection(choice); err != nil {
		return bad(err.Error())
	}
	return input, true
}

func (api organizationAPI) legacyCustomPlan(ctx context.Context, request legacyCustomInput, actor string) (organizationPlan, *recognitionFailure) {
	fail := func(status int, message string) (organizationPlan, *recognitionFailure) {
		return organizationPlan{}, &recognitionFailure{status, message}
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return fail(503, "configured organization directories are unavailable")
	}
	input := organizationPlanInput{Mode: request.Mode, LegacySelection: &request.Selection}
	longest := -1
	for _, root := range roots.Sources {
		// Expand only configured root aliases. Child symlinks are still rejected
		// by the rooted scanner and executor, never resolved into authority.
		aliases := []string{root.Path}
		if physical, err := filepath.EvalSymlinks(root.Path); err == nil {
			aliases = append(aliases, physical)
		}
		for _, alias := range aliases {
			rel, err := filepath.Rel(alias, request.Source)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && len(alias) > longest {
				input.SourceID, input.Path, longest = root.ID, rel, len(alias)
			}
		}
	}
	if input.SourceID == "" {
		return fail(422, "custom identification must select enabled configured source and destination roots")
	}
	return api.legacyIdentificationTargets(ctx, roots, input, request.Target, &organization.LegacyOrganizationOrigin{Action: "renameudf", Kind: "custom", Actor: actor})
}

func (api organizationAPI) legacyIdentificationTargets(ctx context.Context, roots organizationRoots, input organizationPlanInput, dest string, origin *organization.LegacyOrganizationOrigin) (organizationPlan, *recognitionFailure) {
	return api.legacyTargetPlans(ctx, roots, input, dest, origin, true)
}

func (api organizationAPI) serveLegacyCustomRename(w http.ResponseWriter, r *http.Request) {
	api.serveLegacyIdentification(w, r, false)
}

func (api organizationAPI) serveLegacyRename(w http.ResponseWriter, r *http.Request) {
	api.serveLegacyIdentification(w, r, true)
}

func (api organizationAPI) serveLegacyIdentification(w http.ResponseWriter, r *http.Request, linked bool) {
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
	if organization.MoveMode(input.Mode) && !input.ConfirmSourceRemoval {
		writeAPIError(w, 400, 400, "separate source-removal consent is required before creating or executing a move job")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid or expired authorization")
		return
	}
	if organization.RemoteMode(input.Mode) {
		origin := "legacy-custom"
		if linked {
			origin = "legacy-rename"
		}
		if len(input.ReviewFingerprint) != 64 || api.approveLegacyRemote(claims.Username, origin, input.ConfirmRemoteExclusive, input.ConfirmRemoteRecoveryCopy, input.ConfirmOldExecutorsStopped) != nil {
			writeAPIError(w, 400, 400, "remote legacy execution requires a fresh reviewed fingerprint, exclusive namespace/non-atomic publication, retained-copy and stopped-writer consent")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	planner := api.legacyCustomPlan
	if linked {
		planner = api.legacyRenamePlan
	}
	plan, failure := planner(ctx, input, claims.Username)
	if failure == nil && input.ReviewFingerprint != "" && plan.Fingerprint != input.ReviewFingerprint {
		failure = &recognitionFailure{409, "reviewed rename plan changed; preview again before execution"}
	}
	if failure == nil {
		fresh, f := planner(ctx, input, claims.Username)
		failure = f
		if failure == nil && fresh.Fingerprint != plan.Fingerprint {
			failure = &recognitionFailure{409, "source or plan changed; submit a fresh reviewed request"}
		}
		plan = fresh
	}
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	if len(plan.partitions) > 1 && (len(input.ReviewFingerprint) != 64 || !input.ConfirmOldExecutorsStopped) {
		writeAPIError(w, 400, 400, "multi-target execution requires fresh full-scope review and stopped-writer confirmation")
		return
	}
	job, batch, err := api.reserveRunLegacyPlan(ctx, plan, claims.Username, input.ConfirmSourceRemoval)
	id := job.ID
	if err == nil && job.State != "completed" {
		err = errors.New("custom identification did not complete")
	}
	if err != nil {
		failure = legacyRedoFailure(err, "execution requires review; inspect saved job and do not automatically retry")
		data := map[string]any{"jobId": id, "state": job.State}
		if batch != nil {
			data["batchId"], data["jobs"] = batch.ID, batch.Jobs
			data["state"] = "needs_review"
		}
		writeJSON(w, failure.status, map[string]any{"code": failure.status, "success": false, "message": failure.message, "data": data})
		return
	}
	filtered := 0
	for _, item := range plan.Items {
		if item.Status == "filtered" {
			filtered++
		}
	}
	data := map[string]any{"jobId": id, "state": job.State, "filtered": filtered, "unknownState": job.UnknownState}
	if batch != nil {
		data["batchId"], data["jobs"] = batch.ID, batch.Jobs
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "转移成功", "data": data})
}
