package httpserver

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloadcompletion"
	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type downloadCompletionInput struct {
	DownloaderID string `json:"downloaderId"`
	TaskID       string `json:"taskId"`
}
type downloadCompletionReview struct {
	configDigest   string
	ReviewOnly     bool                        `json:"reviewOnly"`
	DownloaderID   string                      `json:"downloaderId"`
	DownloaderName string                      `json:"downloaderName"`
	DownloaderType string                      `json:"downloaderType"`
	Mode           string                      `json:"mode"`
	Actor          string                      `json:"actor"`
	Task           downloadcompletion.Task     `json:"task"`
	Location       downloadcompletion.Location `json:"location"`
	RootIdentity   string                      `json:"rootIdentity"`
	SourceRoot     string                      `json:"sourceRoot"`
	Files          []organization.File         `json:"files"`
	Eligible       bool                        `json:"eligible"`
	Reasons        []string                    `json:"reasons"`
	Fingerprint    string                      `json:"fingerprint"`
}

func (api organizationAPI) reviewDownloadCompletion(ctx context.Context, input downloadCompletionInput, actor string) (downloadCompletionReview, *recognitionFailure) {
	fail := func(status int, message string) (downloadCompletionReview, *recognitionFailure) {
		return downloadCompletionReview{}, &recognitionFailure{status, message}
	}
	id, err := syncConfigID(input.DownloaderID)
	if err != nil {
		return fail(400, "select an exact configured downloader ID")
	}
	item, err := api.service.downloaders.Get(ctx, id)
	if err != nil {
		return fail(422, "configured downloader unavailable")
	}
	if item.Enabled != 1 || item.Transfer != 1 {
		return fail(422, "downloader must explicitly enable completion transfer")
	}
	mode, known := syncConfigMode(item.RmtMode)
	if !known || !organization.SupportedMode(mode) || organization.RemoteMode(mode) && !api.rclone.Available() {
		return fail(501, "remote/unknown completion transfer mode is not migrated; no fallback")
	}
	if item.OnlyNastool < 0 || item.OnlyNastool > 1 || item.MatchPath < 0 || item.MatchPath > 1 {
		return fail(422, "invalid downloader isolation flags")
	}
	service := downloadService{client: api.service.client, downloaders: api.service.downloaders, systemConfig: api.service.systemConfig}
	if item.Type == "qbittorrent" || item.Type == "transmission" || item.Type == "aria2" {
		if !downloadcompletion.ValidID(item.Type, input.TaskID) {
			return fail(400, "exact task hash or GID required")
		}
	}
	var readTask func(context.Context, string) (downloadcompletion.Task, error)
	switch item.Type {
	case "qbittorrent":
		client, _, _, e := service.nativeQbittorrent(ctx, input.DownloaderID)
		if e != nil || client == nil {
			return fail(422, "invalid downloader configuration")
		}
		readTask = client.Completion
	case "transmission":
		client, _, _, e := service.nativeTransmission(ctx, input.DownloaderID)
		if e != nil || client == nil {
			return fail(422, "invalid downloader configuration")
		}
		readTask = client.Completion
	case "aria2":
		client, _, _, e := service.nativeAria2(ctx, input.DownloaderID)
		if e != nil || client == nil {
			return fail(422, "invalid downloader configuration")
		}
		readTask = client.Completion
	default:
		return fail(501, "this downloader completion protocol is not migrated; no legacy fallback")
	}
	task, err := readTask(ctx, input.TaskID)
	if err != nil {
		if ctx.Err() != nil {
			return fail(504, "completion evidence request cancelled or timed out")
		}
		return fail(502, "downloader completion evidence is missing, unavailable or invalid; no completion assumed")
	}
	review := downloadCompletionReview{ReviewOnly: true, DownloaderID: input.DownloaderID, DownloaderName: item.Name, DownloaderType: item.Type, Mode: mode, Actor: actor, Task: task, Files: []organization.File{}, Eligible: true, Reasons: []string{}}
	block := func(reason string) { review.Eligible = false; review.Reasons = append(review.Reasons, reason) }
	if !task.Complete {
		block("downloader has not positively verified every selected file as complete")
	}
	if item.Type != "aria2" && !task.LabelsKnown {
		block("downloader labels unavailable; organized-state and ownership cannot be verified")
	}
	if task.HasLabel("已整理") {
		block("downloader already marks this task organized; this is not evidence that library files still exist")
	}
	if item.OnlyNastool == 1 {
		if item.Type == "aria2" {
			owned, err := api.service.downloaders.HasTrackedTask(ctx, input.DownloaderID, task.ID)
			if err != nil {
				return fail(503, "task ownership history unavailable")
			}
			if !owned {
				block("only_nastool requires an exact persisted downloader/task history record for Aria2")
			}
		} else if !task.HasLabel("NASTOOL") {
			block("only_nastool requires the exact NASTOOL label")
		}
	}
	location, err := downloadcompletion.Resolve(item.DownloadDir, task.Directory, item.MatchPath == 1)
	if err != nil {
		return fail(422, "download directory mapping is ambiguous, unsafe or outside configured local roots")
	}
	review.Location = location
	physical, err := filepath.EvalSymlinks(location.Root)
	if err != nil {
		return fail(422, "configured local source root must exist")
	}
	info, err := os.Stat(physical)
	if err != nil || !info.IsDir() {
		return fail(422, "configured local source root is unavailable")
	}
	review.RootIdentity = organization.Identity(info)
	review.SourceRoot = physical
	bound, err := organization.OpenRoot(physical)
	if err != nil {
		return fail(422, "configured source root cannot be safely opened")
	}
	defer bound.Close()
	relativeDir, err := filepath.Rel(location.Root, location.Directory)
	if err != nil || relativeDir == ".." || strings.HasPrefix(relativeDir, ".."+string(filepath.Separator)) {
		return fail(422, "mapped task escaped its source root")
	}
	for _, file := range task.Files {
		if !file.Selected {
			continue
		}
		relative := filepath.Join(relativeDir, filepath.FromSlash(file.Path))
		if !safeCompletionRelative(relative) {
			if !safeCompletionRelative(relative, true) {
				return fail(422, "selected task path is temporary or reserved; no directory expansion")
			}
			if _, err := organization.DiscContaining(ctx, physical, relative); err != nil {
				return fail(422, "hidden selected file is not a verified Blu-ray ancillary file; no directory expansion")
			}
		}
		fileInfo, err := bound.Lstat(relative)
		if err != nil || !fileInfo.Mode().IsRegular() {
			return fail(422, "selected task entry is not an existing regular file; no directory expansion")
		}
		local, err := organization.SnapshotSelected(ctx, physical, relative)
		if err != nil {
			if ctx.Err() != nil {
				return fail(504, "local completion review cancelled or timed out")
			}
			return fail(422, "selected task file is missing or unsafe; no alternative path is guessed")
		}
		if local.Path != relative {
			return fail(422, "selected task file is hidden, temporary or not a safe regular file")
		}
		local.Kind = organization.FileKind(local.Path)
		if local.Kind == "" {
			local.Kind = "file"
		}
		review.Files = append(review.Files, local)
		if local.Size != file.Size {
			block("local file size does not match downloader evidence: " + file.Path)
		}
		if time.Since(time.Unix(0, local.Modified)) < 30*time.Second {
			block("local file has not reached the quiet interval: " + file.Path)
		}
	}
	fresh, err := readTask(ctx, input.TaskID)
	if err != nil || organization.Digest(task) != organization.Digest(fresh) {
		if ctx.Err() != nil {
			return fail(504, "completion review cancelled or timed out")
		}
		return fail(409, "downloader task changed during completion review; no old evidence reused")
	}
	for _, file := range review.Files {
		current, err := organization.SnapshotSelected(ctx, physical, file.Path)
		if err != nil {
			return fail(409, "local completion evidence changed during review")
		}
		if file.Identity != current.Identity || file.Size != current.Size || file.Modified != current.Modified {
			return fail(409, "local completion evidence changed during review")
		}
	}
	info, err = os.Stat(physical)
	if err != nil || !info.IsDir() || organization.Identity(info) != review.RootIdentity {
		return fail(409, "local source root changed during completion review")
	}
	current, err := api.service.downloaders.Get(ctx, id)
	if err != nil || organization.Digest(current) != organization.Digest(item) {
		return fail(409, "downloader configuration changed during completion review")
	}
	// A server-only digest binds credentials/configuration without exposing them.
	review.Fingerprint = organization.Digest(struct {
		Review downloadCompletionReview
		Config string
	}{review, organization.Digest(item)})
	review.configDigest = organization.Digest(item)
	return review, nil
}

func safeCompletionRelative(relative string, hidden ...bool) bool {
	if !downloadcompletion.Relative(filepath.ToSlash(relative)) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if strings.HasPrefix(part, ".") && !(len(hidden) == 1 && hidden[0]) || strings.HasPrefix(part, ".nastool-") || part == "@Recycle" || part == "#recycle" || part == "$Recycle.Bin" || strings.HasPrefix(part, "@eaDir") || strings.Contains(strings.ToLower(part), ".!qb") {
			return false
		}
	}
	return true
}
func (api organizationAPI) serveDownloadCompletionReview(w http.ResponseWriter, r *http.Request) {
	if !api.jobsAuthorized(w, r) {
		return
	}
	var input downloadCompletionInput
	if !decodeOrganizationRequest(w, r, &input, "invalid completion review request") {
		return
	}
	if len(input.TaskID) > 128 || input.TaskID == "" {
		writeAPIError(w, 400, 400, "exact task hash or GID required")
		return
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	review, failure := api.reviewDownloadCompletion(ctx, input, claims.Username)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": review})
}
