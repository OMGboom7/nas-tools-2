package httpserver

import (
	"context"
	"net/http"
	"path/filepath"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

// Restoring an owned original does not depend on target availability or old
// naming options. It still requires current administrator and source authority;
// removing a configured source never grants access via its old job snapshot.
func (api organizationAPI) validateSourceRestoreConfiguration(ctx context.Context, job organization.Job, actor string) error {
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return err
	}
	if text(objectValue(snapshot["app"])["login_user"]) != actor {
		return organization.ErrState
	}
	roots, err := api.roots(ctx)
	if err != nil {
		return err
	}
	for _, root := range roots.Sources {
		if root.ID == job.Definition.SourceID {
			path, err := filepath.EvalSymlinks(root.Path)
			if err == nil && path == job.Definition.SourceRoot {
				return nil
			}
		}
	}
	return organization.ErrState
}
func (api organizationAPI) serveSourceRestoreReview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization expired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	job, err := api.jobs.Get(ctx, r.PathValue("id"))
	if err == nil {
		err = api.validateSourceRestoreConfiguration(ctx, job, claims.Username)
	}
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	review, err := api.jobs.ReviewSourceRestore(ctx, job)
	if err != nil {
		writeOrganizationError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
}
