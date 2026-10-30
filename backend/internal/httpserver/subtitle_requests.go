package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/organization"
	"github.com/0xforee/nas-tools/backend/internal/remotestore"
	"github.com/0xforee/nas-tools/backend/internal/subtitles"
)

const subtitleRequestsBase = "/api/v1/organization/subtitles"

type subtitleRequestStatus struct {
	Ready       bool               `json:"ready"`
	Reason      string             `json:"reason"`
	Fingerprint string             `json:"fingerprint"`
	Service     string             `json:"service"`
	Origin      string             `json:"origin"`
	HTTP        bool               `json:"http"`
	Roots       []organizationRoot `json:"roots"`
	Drivers     []string           `json:"drivers"`
	failure     error
}
type subtitleRequestIdentity struct {
	Kind       string `json:"kind"`
	TMDBID     string `json:"tmdbId"`
	Season     *int   `json:"season,omitempty"`
	Episode    *int   `json:"episode,omitempty"`
	EndEpisode *int   `json:"endEpisode,omitempty"`
}
type subtitleRequestInput struct {
	RootID                     string                   `json:"rootId"`
	Driver                     string                   `json:"driver"`
	Path                       string                   `json:"path"`
	Name                       string                   `json:"name"`
	Identity                   *subtitleRequestIdentity `json:"identity,omitempty"`
	Fingerprint                string                   `json:"fingerprint"`
	ReviewID                   string                   `json:"reviewId"`
	Review                     bool                     `json:"review"`
	ConfirmRead                bool                     `json:"confirmRead"`
	ConfirmRemoteRead          bool                     `json:"confirmRemoteRead"`
	Confirm                    bool                     `json:"confirm"`
	ConfirmPath                bool                     `json:"confirmPath"`
	ConfirmExternalWrite       bool                     `json:"confirmExternalWrite"`
	ConfirmOldExecutorsStopped bool                     `json:"confirmOldExecutorsStopped"`
	HTTP                       bool                     `json:"http"`
}

// Keep duplicate keys, trailing documents and deep/large inputs out of a
// reviewed action, including the old JSON command envelope.
func subtitleJSON(raw []byte, target any) error {
	if len(raw) > 128<<10 || !utf8.Valid(raw) || !json.Valid(raw) {
		return organization.ErrPath
	}
	walk := json.NewDecoder(bytes.NewReader(raw))
	walk.UseNumber()
	var value func(int) error
	count := 0
	value = func(depth int) error {
		count++
		if count > 2048 || depth > 16 {
			return organization.ErrLimit
		}
		token, e := walk.Token()
		if e != nil {
			return e
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for walk.More() {
				key, e := walk.Token()
				if e != nil {
					return e
				}
				k, ok := key.(string)
				if !ok || seen[k] {
					return organization.ErrPath
				}
				seen[k] = true
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			end, e := walk.Token()
			if e != nil || end != json.Delim('}') {
				return organization.ErrPath
			}
		case '[':
			for walk.More() {
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			end, e := walk.Token()
			if e != nil || end != json.Delim(']') {
				return organization.ErrPath
			}
		default:
			return organization.ErrPath
		}
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return organization.ErrPath
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := walk.Token(); e != io.EOF {
		return organization.ErrPath
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	return d.Decode(target)
}
func decodeSubtitleJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if e != nil || subtitleJSON(raw, target) != nil {
		writeAPIError(w, 400, 400, "无效字幕请求；字段、重复键或报文范围未核实")
		return false
	}
	return true
}
func subtitleRequestError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, subtitles.ErrConsumer):
		writeAPIError(w, 501, 501, "已启用的 OpenSubtitles 消费者尚未迁移，未提交字幕请求")
	case errors.Is(e, subtitles.ErrConfig):
		writeAPIError(w, 409, 409, "先安装并保存有效 ChineseSubFinder 原配置，再重新审核")
	case errors.Is(e, subtitles.ErrState):
		writeAPIError(w, 409, 409, "原字幕配置或回执变化，请只读查回原编号，不要重复提交")
	default:
		writeOrganizationError(w, e)
	}
}
func (api *subtitleAPI) authorizeRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	if api == nil || api.organizer == nil {
		writeAPIError(w, 503, 503, "原生字幕请求不可用")
		return "", false
	}
	if !api.organizer.jobsAuthorized(w, r) {
		return "", false
	}
	if api.queue == nil {
		writeAPIError(w, 503, 503, "原生字幕回执不可用")
		return "", false
	}
	claims, e := api.organizer.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if e != nil {
		writeAPIError(w, 401, 401, "登录状态已失效")
		return "", false
	}
	if _, e := api.requestAuthority(claims.Username); e != nil {
		writeAPIError(w, 403, 403, "仅当前主管理员可操作字幕请求")
		return "", false
	}
	return claims.Username, true
}
func (api *subtitleAPI) requestAuthority(actor string) (string, error) {
	return (&eventWebhookAPI{organizer: api.organizer}).authority(actor)
}
func (api *subtitleAPI) requestStatus(ctx context.Context, actor string) (subtitleRequestStatus, error) {
	out := subtitleRequestStatus{Service: subtitles.Plugin, Roots: []organizationRoot{}, Drivers: []string{""}}
	roots, e := api.organizer.roots(ctx)
	if e != nil {
		return out, e
	}
	seen := map[string]bool{}
	for _, r := range append(append([]organizationRoot{}, roots.Targets...), roots.Sources...) {
		if !seen[r.ID] {
			out.Roots = append(out.Roots, r)
			seen[r.ID] = true
		}
	}
	for _, driver := range []string{"rclone", "minio"} {
		if api.organizer.remoteFactory(organization.LibraryRemoteMode(driver)).Available() {
			out.Drivers = append(out.Drivers, driver)
		}
	}
	authority, e := api.requestAuthority(actor)
	if e != nil {
		return out, e
	}
	config, e := api.organizer.service.configStore.Snapshot()
	if e != nil {
		return out, e
	}
	provider, e := api.queue.Snapshot(ctx, "/native-subtitle-readonly.mkv", "电影", false)
	out.Fingerprint = organization.Digest([]any{authority, config, provider.Basis})
	if e != nil {
		out.failure = e
		out.Reason = "先安装并保存有效 ChineseSubFinder 原配置；旧配置不会提交文件请求。"
		if errors.Is(e, subtitles.ErrConsumer) {
			out.Reason = "已启用的 OpenSubtitles 消费者尚未迁移；本次不会绕过它只运行部分消费者。"
		}
		return out, nil
	}
	out.Ready = true
	out.Origin = provider.Origin
	out.HTTP = provider.HTTP
	return out, nil
}
func (api *subtitleAPI) requestGuards(ctx context.Context, worker bool) (func(), error) {
	closeAll := func() {}
	names := organization.RecoveryOwnerGuardNames(organization.RecoveryDefinition{})
	for _, name := range names {
		if worker && name == "intake-enrichment-outbox" {
			continue
		}
		guard, e := api.organizer.jobs.AcquireJob(ctx, name)
		if e != nil {
			closeAll()
			return func() {}, e
		}
		previous := closeAll
		closeAll = func() { guard.Close(); previous() }
	}
	return closeAll, nil
}
func (api *subtitleAPI) requestCurrent(ctx context.Context, d organization.SubtitleRequestDefinition) error {
	authority, e := api.requestAuthority(d.Actor)
	if e != nil || authority != d.Authority {
		return organization.ErrState
	}
	if e = api.organizer.recoveryScopeCurrent(ctx, d.Scope); e != nil {
		return e
	}
	if d.Scope.Remote == nil {
		return organization.VerifyLibraryMedia(ctx, organization.SubtitleRequestLibrary(d), *d.Local)
	}
	return nil
}
func (api *subtitleAPI) requestContext(ctx context.Context, d organization.SubtitleRequestDefinition) (*remotestore.Client, func(), error) {
	c, e := api.organizer.openRecoveryScope(ctx, d.Scope)
	if e != nil {
		return nil, func() {}, e
	}
	return c, func() {
		if c != nil {
			_ = c.Close()
		}
	}, nil
}
func (api *subtitleAPI) normalizeRequest(ctx context.Context, in subtitleRequestInput, roots []organizationRoot) (subtitleRequestInput, error) {
	if in.Driver != "" && organization.LibraryRemoteMode(in.Driver) == "" {
		return in, organization.ErrMode
	}
	if !utf8.ValidString(in.Path) || len(in.Path) > 4096 || strings.ContainsAny(in.Path, "\\\x00\r\n") {
		return in, organization.ErrPath
	}
	if filepath.IsAbs(in.Path) {
		if filepath.Clean(in.Path) != in.Path {
			return in, organization.ErrPath
		}
		sorted := append([]organizationRoot{}, roots...)
		sort.Slice(sorted, func(i, j int) bool { return len(sorted[i].Path) > len(sorted[j].Path) })
		found := false
		for _, r := range sorted {
			if in.RootID != "" && in.RootID != r.ID {
				continue
			}
			paths := []string{r.Path}
			if in.Driver == "" {
				if resolved, e := filepath.EvalSymlinks(r.Path); e == nil {
					paths = append(paths, resolved)
				}
			}
			for _, root := range paths {
				rel, e := filepath.Rel(root, in.Path)
				if e == nil && rel != "." && !strings.HasPrefix(rel, "../") && rel != ".." && !filepath.IsAbs(rel) {
					in.RootID = r.ID
					in.Path = filepath.ToSlash(rel)
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return in, organization.ErrPath
		}
	}
	if in.RootID == "" || !organization.SubtitleRequestPath(in.Path) {
		return in, organization.ErrPath
	}
	if in.Name == "" {
		in.Name = path.Base(in.Path)
	}
	if len(in.Name) > 600 || !utf8.ValidString(in.Name) || strings.ContainsAny(in.Name, "\x00\r\n") {
		return in, organization.ErrPath
	}
	return in, nil
}
func (api *subtitleAPI) reviewRequest(ctx context.Context, actor string, in subtitleRequestInput) (organization.SubtitleRequest, error) {
	zero := organization.SubtitleRequest{}
	if !in.ConfirmRead || in.Driver != "" && !in.ConfirmRemoteRead {
		return zero, organization.ErrPath
	}
	status, e := api.requestStatus(ctx, actor)
	if e != nil {
		return zero, e
	}
	if !status.Ready && status.failure != nil {
		return zero, status.failure
	}
	if !status.Ready || in.Fingerprint != status.Fingerprint {
		return zero, organization.ErrState
	}
	originalPath := in.Path
	in, e = api.normalizeRequest(ctx, in, status.Roots)
	if e != nil {
		return zero, e
	}
	closeGuards, e := api.requestGuards(ctx, false)
	if e != nil {
		return zero, e
	}
	defer closeGuards()
	scope, c, e := api.organizer.recoveryScope(ctx, in.RootID, in.Driver)
	if e != nil {
		return zero, e
	}
	if c != nil {
		defer c.Close()
	}
	request, e := newIndexerClaim()
	if e != nil {
		return zero, e
	}
	authority, e := api.requestAuthority(actor)
	if e != nil {
		return zero, e
	}
	d := organization.SubtitleRequestDefinition{Version: 1, Actor: actor, Authority: authority, RequestID: request, Scope: scope, Selection: in.Path, InputPath: originalPath, Name: in.Name, Allowed: []organization.LibraryRoot{}, RemoteNodes: []organization.RecoveryNode{}}
	proofFile := organization.File{Path: in.Path, Kind: "media"}
	if c == nil {
		for _, r := range status.Roots {
			if r.ID != scope.Root.ID {
				if root, e := libraryRootSnapshot(r); e == nil {
					d.Allowed = append(d.Allowed, root)
				}
			}
		}
		library := organization.SubtitleRequestLibrary(d)
		media, e := organization.CaptureLibraryMedia(ctx, library, in.Path)
		if e != nil {
			return zero, e
		}
		d.Local = &media
		d.ContentDigest, e = organization.SubtitleRequestLocalDigest(ctx, library, media)
		if e != nil {
			return zero, e
		}
		proofFile.Identity = media.Node.Identity
		proofFile.Modified = media.Node.Modified
		proofFile.Size = media.Input.Size
	} else {
		node, e := c.Entry(ctx, scope.Root.Path, in.Path)
		if e != nil {
			return zero, e
		}
		if node == nil || node.IsDir {
			return zero, organization.ErrPath
		}
		d.RemoteNodes, e = organization.MediaRemovalSnapshot(ctx, scope, in.Path, c)
		if e != nil {
			return zero, e
		}
		if len(d.RemoteNodes) != 1 || d.RemoteNodes[0].Kind != "file" {
			return zero, organization.ErrPath
		}
		d.ContentDigest = d.RemoteNodes[0].Digest
		proofFile.Identity = organization.Digest(d.RemoteNodes)
		proofFile.Size = d.RemoteNodes[0].Node.Size
	}
	var recognized *nativeRecognition
	var failure *recognitionFailure
	identify, cancel := context.WithTimeout(ctx, 45*time.Second)
	if in.Identity == nil {
		recognized, failure = api.organizer.recognition.recognize(identify, in.Name, "")
	} else {
		selected := organizationMediaAssignment{Source: in.Path, Identity: proofFile.Identity, Modified: strconv.FormatInt(proofFile.Modified, 10), Size: proofFile.Size, Kind: in.Identity.Kind, TMDBID: in.Identity.TMDBID, Season: in.Identity.Season, Episode: in.Identity.Episode, EndEpisode: in.Identity.EndEpisode}
		selections, f := organizationAssignments(identify, []organizationMediaAssignment{selected}, []organization.File{proofFile})
		failure = f
		if f == nil {
			recognized, failure = api.organizer.recognition.recognizeAssigned(identify, in.Name, selections[in.Path])
		}
	}
	cancel()
	if failure != nil {
		return zero, &subtitleRecognitionError{failure}
	}
	if recognized == nil {
		return zero, organization.ErrNotFound
	}
	d.Title = text(recognized.data["title"])
	d.Year = text(recognized.data["year"])
	d.MediaType = text(recognized.data["type"])
	d.TMDBID = strconv.FormatInt(recognized.detail.ID, 10)
	d.MediaInfo = recognized.data
	d.MediaInfo["tmdb_id"] = d.TMDBID
	if imdb, ok := recognized.detail.Attributes["imdb_id"].(string); ok && imdb != "" {
		d.MediaInfo["imdb_id"] = imdb
	}
	d.Plugin, e = api.queue.Snapshot(ctx, path.Join(scope.Root.Path, in.Path), d.MediaType, false)
	if e != nil {
		return zero, e
	}
	d.Created = time.Now().UTC().Format(time.RFC3339Nano)
	return api.organizer.jobs.SaveSubtitleReview(ctx, d, func() error { return api.requestCurrent(ctx, d) })
}

type subtitleRecognitionError struct{ failure *recognitionFailure }

func (e *subtitleRecognitionError) Error() string { return e.failure.message }
func (api *subtitleAPI) queueRequest(ctx context.Context, actor string, in subtitleRequestInput) (organization.SubtitleRequest, error) {
	p, e := api.organizer.jobs.SubtitleRequest(ctx, in.ReviewID)
	if e != nil {
		return p, e
	}
	if p.Definition.Actor != actor || p.Fingerprint != in.Fingerprint || !in.Confirm || !in.ConfirmPath || !in.ConfirmExternalWrite || !in.ConfirmOldExecutorsStopped || p.HTTP && !in.HTTP || p.Driver != "" && !in.ConfirmRemoteRead {
		return p, organization.ErrState
	}
	if in.RootID != "" && in.RootID != p.RootID || in.Driver != "" && in.Driver != p.Driver || in.Name != "" && in.Name != p.Name || in.Identity != nil || in.Review || in.Path != "" && in.Path != p.Path && in.Path != p.Definition.InputPath && in.Path != path.Join(p.Definition.Scope.Root.Path, p.Path) {
		return p, organization.ErrState
	}
	// A lost HTTP response is resolved only by this exact original definition.
	// Returning an already-queued request does not reread a replaced source.
	if p.State == "queued" {
		return p, nil
	}
	closeGuards, e := api.requestGuards(ctx, false)
	if e != nil {
		return p, e
	}
	defer closeGuards()
	c, closeRemote, e := api.requestContext(ctx, p.Definition)
	if e != nil {
		return p, e
	}
	defer closeRemote()
	if e = organization.VerifySubtitleRequestSource(ctx, p.Definition, c); e != nil {
		return p, e
	}
	return api.organizer.jobs.QueueSubtitleRequest(ctx, p.ID, actor, p.Fingerprint, func() error { return api.requestCurrent(ctx, p.Definition) })
}
func (api *subtitleAPI) serveRequests(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorizeRequest(w, r)
	if !ok {
		return
	}
	base := subtitleRequestsBase
	var out any
	var e error
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	switch {
	case r.URL.Path == base && r.Method == "GET":
		out, e = api.requestStatus(ctx, actor)
	case r.URL.Path == base+"/review" && r.Method == "POST":
		var in subtitleRequestInput
		if !decodeSubtitleJSON(w, r, &in) {
			return
		}
		out, e = api.reviewRequest(ctx, actor, in)
	case r.URL.Path == base+"/requests" && r.Method == "POST":
		var in subtitleRequestInput
		if !decodeSubtitleJSON(w, r, &in) {
			return
		}
		out, e = api.queueRequest(ctx, actor, in)
	case r.URL.Path == base+"/requests" && r.Method == "GET":
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			page, e = strconv.Atoi(raw)
		}
		if e == nil {
			out, e = api.organizer.jobs.SubtitleRequests(ctx, page)
		}
	case strings.HasPrefix(r.URL.Path, base+"/requests/"):
		tail := strings.TrimPrefix(r.URL.Path, base+"/requests/")
		id, action, has := strings.Cut(tail, "/")
		if !has && r.Method == "GET" {
			out, e = api.organizer.jobs.SubtitleRequest(ctx, id)
		} else if has && action == "cancel" && r.Method == "POST" {
			var in struct {
				Confirm bool   `json:"confirm"`
				Reason  string `json:"reason"`
			}
			if !decodeSubtitleJSON(w, r, &in) {
				return
			}
			if !in.Confirm {
				writeAPIError(w, 400, 400, "先确认仅停止原尚未发送的请求")
				return
			}
			closeGuards, err := api.requestGuards(ctx, false)
			if err != nil {
				subtitleRequestError(w, err)
				return
			}
			out, e = api.organizer.jobs.CancelSubtitleRequest(ctx, id, actor, in.Reason)
			closeGuards()
		} else {
			writeAPIError(w, 405, 405, "不支持的原生字幕操作")
			return
		}
	default:
		writeAPIError(w, 405, 405, "不支持的原生字幕请求")
		return
	}
	var recognition *subtitleRecognitionError
	if errors.As(e, &recognition) {
		writeAPIError(w, recognition.failure.status, recognition.failure.status, recognition.failure.message)
		return
	}
	if e != nil {
		subtitleRequestError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": out})
}

// The old /do dispatcher remains concrete and bounded. Only this migrated
// command is intercepted; the original native scrape handler checks all others.
func (api *subtitleAPI) legacyAction(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			writeAPIError(w, 405, 405, "仅支持 POST 原操作")
			return
		}
		raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if e != nil {
			writeAPIError(w, 400, 400, "原操作报文过大")
			return
		}
		var envelope struct {
			Cmd  string          `json:"cmd"`
			Data json.RawMessage `json:"data"`
		}
		if subtitleJSON(raw, &envelope) != nil {
			writeAPIError(w, 400, 400, "无效原操作报文")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if envelope.Cmd != "download_subtitle" {
			next(w, r)
			return
		}
		actor, ok := api.authorizeRequest(w, r)
		if !ok {
			return
		}
		var in subtitleRequestInput
		if subtitleJSON(envelope.Data, &in) != nil {
			writeAPIError(w, 400, 400, "无效原字幕操作")
			return
		}
		api.serveLegacyRequest(w, r, actor, in)
	}
}
func (api *subtitleAPI) serveLegacyRequest(w http.ResponseWriter, r *http.Request, actor string, in subtitleRequestInput) {
	ctx, cancel := context.WithTimeout(r.Context(), organization.ExecutionTimeout)
	defer cancel()
	var out organization.SubtitleRequest
	var e error
	if in.Review {
		out, e = api.reviewRequest(ctx, actor, in)
	} else {
		out, e = api.queueRequest(ctx, actor, in)
	}
	var recognized *subtitleRecognitionError
	if errors.As(e, &recognized) {
		writeAPIError(w, recognized.failure.status, recognized.failure.status, recognized.failure.message)
		return
	}
	if e != nil {
		subtitleRequestError(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": out, "msg": "仅保存这份字幕请求，不代表字幕已下载；按原编号查询"})
}
func (api *subtitleAPI) legacySubtitle(w http.ResponseWriter, r *http.Request) {
	actor, ok := api.authorizeRequest(w, r)
	if !ok {
		return
	}
	if r.Method != "POST" || r.URL.Path != "/api/v1/media/subtitle/download" {
		writeAPIError(w, 405, 405, "不支持的原字幕入口")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if r.ParseForm() != nil {
		writeAPIError(w, 400, 400, "无效字幕表单")
		return
	}
	in := subtitleRequestInput{}
	texts := map[string]*string{"rootId": &in.RootID, "driver": &in.Driver, "path": &in.Path, "name": &in.Name, "fingerprint": &in.Fingerprint, "reviewId": &in.ReviewID}
	flags := map[string]*bool{"review": &in.Review, "confirmRead": &in.ConfirmRead, "confirmRemoteRead": &in.ConfirmRemoteRead, "confirm": &in.Confirm, "confirmPath": &in.ConfirmPath, "confirmExternalWrite": &in.ConfirmExternalWrite, "confirmOldExecutorsStopped": &in.ConfirmOldExecutorsStopped, "http": &in.HTTP}
	for key, values := range r.PostForm {
		if len(values) != 1 {
			writeAPIError(w, 400, 400, "重复字幕表单字段")
			return
		}
		if target, ok := texts[key]; ok {
			*target = values[0]
		} else if target, ok := flags[key]; ok {
			if values[0] != "true" && values[0] != "false" {
				writeAPIError(w, 400, 400, "无效字幕确认")
				return
			}
			*target = values[0] == "true"
		} else {
			writeAPIError(w, 400, 400, "未知字幕表单字段")
			return
		}
	}
	api.serveLegacyRequest(w, r, actor, in)
}
