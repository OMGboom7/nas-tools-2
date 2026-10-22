package httpserver

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
	"github.com/dlclark/regexp2"
)

type directSyncInput struct {
	ID          string `json:"id"`
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint,omitempty"`
}
type directSyncRules struct{ paths, files *regexp2.Regexp }

func compileDirectSyncRules(media map[string]any) (directSyncRules, error) {
	r := directSyncRules{}
	for _, rule := range []struct {
		key string
		out **regexp2.Regexp
	}{{"ignored_paths", &r.paths}, {"ignored_files", &r.files}} {
		value := media[rule.key]
		if value == nil {
			continue
		}
		pattern, ok := value.(string)
		if !ok || len(pattern) > 4096 {
			return r, organization.ErrState
		}
		if pattern == "" {
			continue
		}
		compiled, err := regexcompat.Compile(strings.ReplaceAll(strings.TrimSuffix(pattern, ";"), ";", "|"), regexp2.None)
		if err != nil {
			return r, err
		}
		*rule.out = compiled
	}
	return r, nil
}
func (rules directSyncRules) ignored(ctx context.Context, target string) (bool, error) {
	for _, item := range []struct {
		pattern *regexp2.Regexp
		value   string
	}{{rules.paths, filepath.Dir(target)}, {rules.files, filepath.Base(target)}} {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if item.pattern == nil {
			continue
		}
		match, err := item.pattern.MatchString(item.value)
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	return false, ctx.Err()
}
func insideSyncPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Canonicalize only configured paths. A not-yet-created configured directory
// is checked against its nearest existing ancestor, without creating it.
func configuredSyncPath(path string) (string, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 || filepath.Clean(path) == string(filepath.Separator) || strings.ContainsAny(path, "\\\x00\r\n") {
		return "", organization.ErrPath
	}
	current := filepath.Clean(path)
	suffix := []string{}
	for i := 0; i < 64; i++ {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for j := len(suffix) - 1; j >= 0; j-- {
				resolved = filepath.Join(resolved, suffix[j])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", organization.ErrPath
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", organization.ErrPath
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
	return "", organization.ErrLimit
}

func validateDirectSyncGraph(current organization.SyncRecord, records []organization.SyncRecord, media map[string]any) error {
	source, err := configuredSyncPath(current.Source)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Enabled != 1 {
			continue
		}
		other, err := configuredSyncPath(record.Source)
		if err != nil {
			return err
		}
		if record.ID != current.ID && (insideSyncPath(source, other) || insideSyncPath(other, source)) {
			return organization.ErrPath
		}
		for index, dest := range []string{record.Dest, record.Unknown} {
			if dest == "" || !filepath.IsAbs(dest) {
				continue
			}
			mode, _ := syncConfigMode(record.Mode)
			if index == 0 && organization.RemoteMode(mode) {
				if _, err := logicalRemoteRoot(dest); err != nil {
					return err
				}
				continue
			}
			physical, err := configuredSyncPath(dest)
			if err != nil {
				return err
			}
			if insideSyncPath(source, physical) || insideSyncPath(physical, source) {
				return organization.ErrPath
			}
		}
	}
	for _, key := range []string{"movie_path", "tv_path", "anime_path"} {
		paths, err := mediaLibraryPaths(media, key)
		if err != nil {
			return err
		}
		for _, path := range paths {
			mode, _ := syncConfigMode(current.Mode)
			if organization.RemoteMode(mode) {
				if _, err := logicalRemoteRoot(path); err != nil {
					return err
				}
				continue
			}
			physical, err := configuredSyncPath(path)
			if err != nil {
				return err
			}
			if insideSyncPath(physical, source) || insideSyncPath(source, physical) {
				return organization.ErrPath
			}
		}
	}
	return nil
}

func (api organizationAPI) serveSyncRecords(w http.ResponseWriter, r *http.Request) {
	if !api.recordsAuthorized(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	items, err := api.records.ListSync(ctx)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"items": items}})
}
func (api organizationAPI) serveDirectSyncPlan(w http.ResponseWriter, r *http.Request) {
	api.serveDirectSync(w, r, false)
}
func (api organizationAPI) serveDirectSyncCreate(w http.ResponseWriter, r *http.Request) {
	api.serveDirectSync(w, r, true)
}
func (api organizationAPI) serveDirectSync(w http.ResponseWriter, r *http.Request, create bool) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input directSyncInput
	if !decodeOrganizationRequest(w, r, &input, "invalid direct sync request") {
		return
	}
	id, err := strconv.ParseInt(input.ID, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != input.ID {
		writeAPIError(w, 400, 400, "select an exact positive sync configuration ID")
		return
	}
	if input.Path == "" {
		input.Path = "."
	}
	if !create && input.Fingerprint != "" || create && len(input.Fingerprint) != 64 {
		writeAPIError(w, 400, 400, "preview first and supply its exact fingerprint only when saving")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid or expired authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	plan, failure := api.directSyncPlan(ctx, id, input.Path, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	if !create {
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": plan})
		return
	}
	if plan.Fingerprint != input.Fingerprint {
		writeAPIError(w, 409, 409, "sync scope, configuration, source or history changed; review a new preview")
		return
	}
	if len(plan.definition.Entries) == 0 {
		writeAPIError(w, 422, 422, "no available direct sync files")
		return
	}
	jobID, err := api.jobs.Create(ctx, plan.Fingerprint, plan.definition)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	job, err := api.jobs.Get(ctx, jobID)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"code": 0, "success": true, "data": job})
}

func (api organizationAPI) directSyncPlan(ctx context.Context, id int64, scope, actor string) (organizationPlan, *recognitionFailure) {
	fail := func(status int, message string) (organizationPlan, *recognitionFailure) {
		return organizationPlan{}, &recognitionFailure{status, message}
	}
	record, err := api.jobs.GetSync(ctx, id)
	if err != nil {
		return fail(409, "sync record unavailable; refresh configuration")
	}
	if record.Enabled != 1 {
		return fail(422, "sync configuration must be enabled")
	}
	if record.Rename != 0 {
		return fail(501, "this endpoint is direct file sync only; recognition must not silently degrade to raw copying")
	}
	mode := transferModes[record.Mode]
	if mode == "" && organization.SupportedMode(record.Mode) {
		mode = record.Mode
	}
	if !organization.SupportedMode(mode) {
		return fail(501, "sync transfer mode is not migrated; no fallback")
	}
	if record.Source == "" || record.Dest == "" {
		return fail(422, "direct sync requires explicit configured source and destination roots")
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return fail(503, "configuration unavailable")
	}
	media := objectValue(snapshot["media"])
	records, err := api.jobs.ListSync(ctx)
	if err != nil {
		return fail(503, "sync configurations unavailable")
	}
	if validateDirectSyncGraph(record, records, media) != nil {
		return fail(422, "sync sources overlap, point into media libraries, or form a loop through destinations/unknown directories")
	}
	rules, err := compileDirectSyncRules(media)
	if err != nil {
		return fail(422, "invalid sync ignore rule; nothing was executed")
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return fail(503, "configured roots unavailable")
	}
	sourceID, targetID := subscriptionResourceKey("source:"+filepath.Clean(record.Source)+":"), subscriptionResourceKey("target:"+filepath.Clean(record.Dest)+":")
	matchedSource, matchedTarget := false, false
	for _, root := range roots.Sources {
		matchedSource = matchedSource || root.ID == sourceID
	}
	for _, root := range roots.Targets {
		matchedTarget = matchedTarget || root.ID == targetID
	}
	if !matchedSource || !matchedTarget {
		return fail(422, "sync must use its enabled configured root pair")
	}
	source, err := filepath.EvalSymlinks(record.Source)
	if err != nil {
		return fail(422, "source root must exist")
	}
	var target string
	var remote *remotestore.Client
	if organization.RemoteMode(mode) {
		target, err = logicalRemoteRoot(record.Dest)
		if err != nil {
			return fail(422, "remote logical root is unsafe")
		}
		remote, err = api.remoteRoot(ctx, target, mode)
		if err != nil {
			return fail(422, "reviewed remote provider configuration or root unavailable")
		}
		defer remote.Close()
	} else {
		target, err = filepath.EvalSymlinks(record.Dest)
	}
	if err != nil {
		return fail(422, "destination root must exist; preview does not create it")
	}
	si, err := os.Stat(source)
	if err != nil || !si.IsDir() {
		return fail(422, "source root unavailable")
	}
	targetIdentity := organization.RemoteTargetIdentityFor(mode, target)
	if remote == nil {
		ti, err := os.Stat(target)
		if err != nil || !ti.IsDir() {
			return fail(422, "destination root unavailable")
		}
		targetIdentity = organization.Identity(ti)
	}
	scan, err := organization.ScanAll(ctx, source, scope)
	if err != nil {
		if ctx.Err() != nil {
			return fail(504, "sync preview cancelled")
		}
		return fail(422, "unsafe, changed or oversized source scope; select a smaller directory")
	}
	root, err := organization.OpenRoot(source)
	if err != nil {
		return fail(422, "source root changed")
	}
	selected, err := root.Lstat(scope)
	root.Close()
	if err != nil {
		return fail(422, "source scope changed")
	}
	definition := organization.Definition{Sync: &organization.SyncOrigin{Record: record, Scope: scope, ScopeIdentity: organization.Identity(selected), GraphDigest: organization.SyncGraphDigest(records), Actor: actor}, SourceID: sourceID, TargetID: targetID, SourceRoot: source, TargetRoot: target, SourceIdentity: organization.Identity(si), TargetIdentity: targetIdentity, Mode: mode, ConfigDigest: organization.Digest(snapshot)}
	if remote != nil {
		binding := remote.Binding()
		definition.Remote = &binding
	}
	plan := organizationPlan{PreviewOnly: true, Mode: mode, Skipped: scan.Skipped, Items: []organizationPlanItem{}, definition: definition}
	if remote != nil {
		plan.Remote = remote.Binding().Summary()
	}
	for _, file := range scan.Files {
		if ctx.Err() != nil {
			return fail(504, "sync preview cancelled")
		}
		item := organizationPlanItem{Source: file.Path, Target: file.Path, Kind: "file", Size: file.Size, Identity: file.Identity, Modified: strconv.FormatInt(file.Modified, 10), Status: "available"}
		recorded, err := api.jobs.SyncRecorded(ctx, definition, file.Path)
		if err != nil {
			return fail(503, "legacy sync history unavailable")
		}
		if recorded {
			item.Status, item.Reason = "recorded", "old sync history suppresses re-execution; it does not prove the target currently exists"
		} else if strings.Contains(strings.ToLower(file.Path), ".!qb") {
			item.Status, item.Reason = "filtered", "download temporary path; not executed"
		} else {
			ignored, err := rules.ignored(ctx, filepath.Join(record.Dest, file.Path))
			if err != nil {
				if ctx.Err() != nil {
					return fail(504, "sync preview cancelled")
				}
				return fail(422, "sync ignore rule exceeded its matching budget; nothing was executed")
			}
			if ignored {
				item.Status, item.Reason = "filtered", "destination path/name matches an explicit ignore rule"
			} else {
				item.Status, err = organizationTargetStatus(ctx, target, item.Target, remote)
				if err != nil {
					return fail(422, "destination path is unavailable or unsafe")
				}
				if item.Status == "available" {
					if err := organization.ValidateMode(ctx, definition, item.entry()); err != nil {
						item.Status, item.Reason = "blocked", "literal sync mode is unavailable; no fallback"
					}
				}
			}
		}
		plan.Items = append(plan.Items, item)
		if item.Status == "available" {
			plan.definition.Entries = append(plan.definition.Entries, item.entry())
		}
	}
	if ctx.Err() != nil {
		return fail(504, "sync preview cancelled")
	}
	plan.Fingerprint = organization.Digest(struct {
		Definition organization.Definition
		Items      []organizationPlanItem
		Skipped    int
	}{plan.definition, plan.Items, plan.Skipped})
	return plan, nil
}
