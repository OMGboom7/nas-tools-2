package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/organization"
)

type organizationAPI struct {
	service     subscriptionService
	auth        *nativeAuthentication
	recognition mediaNameAPI
	jobs        *organization.Store
	pureGo      bool
}
type organizationRoot struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	Label string `json:"label"`
	Type  string `json:"type"`
}
type organizationRoots struct {
	Sources        []organizationRoot `json:"sources"`
	Targets        []organizationRoot `json:"targets"`
	ExecutionModes []string           `json:"executionModes"`
}
type organizationPlanInput struct {
	SourceID string `json:"sourceId"`
	TargetID string `json:"targetId"`
	Path     string `json:"path"`
	Mode     string `json:"mode"`
}
type organizationPlanItem struct {
	Source        string `json:"source"`
	Target        string `json:"target,omitempty"`
	Kind          string `json:"kind"`
	Size          int64  `json:"size"`
	Modified      string `json:"modified"`
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	TMDBID        string `json:"tmdbId,omitempty"`
	Identity      string `json:"identity"`
	Title         string `json:"title,omitempty"`
	Year          string `json:"year,omitempty"`
	MediaType     string `json:"mediaType,omitempty"`
	Category      string `json:"category,omitempty"`
	SeasonEpisode string `json:"seasonEpisode,omitempty"`
}
type organizationPlan struct {
	Items       []organizationPlanItem `json:"items"`
	Mode        string                 `json:"mode"`
	PreviewOnly bool                   `json:"previewOnly"`
	Skipped     int                    `json:"skipped"`
	Fingerprint string                 `json:"fingerprint"`
	definition  organization.Definition
}

func (api organizationAPI) authorize(w http.ResponseWriter, r *http.Request) bool {
	if api.auth == nil || api.service.configStore == nil || api.service.downloaders == nil {
		writeAPIError(w, 503, 503, "native organization configuration is unavailable")
		return false
	}
	claims, err := api.auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return false
	}
	if !api.auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission is required")
		return false
	}
	return true
}

func (api organizationAPI) serveRoots(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	roots, err := api.roots(ctx)
	if err != nil {
		writeAPIError(w, 503, 503, "configured organization roots are unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": roots})
}

func (api organizationAPI) roots(ctx context.Context) (organizationRoots, error) {
	result := organizationRoots{Sources: []organizationRoot{}, Targets: []organizationRoot{}, ExecutionModes: []string{}}
	if api.pureGo && api.jobs != nil && organization.CopySupported {
		result.ExecutionModes = []string{"copy"}
	}
	seen := map[string]bool{}
	add := func(role, path, label, kind string) error {
		if path == "" {
			return nil
		}
		if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) || strings.ContainsAny(path, "\x00\r\n") {
			return organization.ErrPath
		}
		path = filepath.Clean(path)
		id := subscriptionResourceKey(role + ":" + path + ":" + kind)
		if seen[id] {
			return nil
		}
		seen[id] = true
		if len(seen) > 128 {
			return organization.ErrLimit
		}
		root := organizationRoot{id, path, label, kind}
		if role == "source" {
			result.Sources = append(result.Sources, root)
		} else {
			result.Targets = append(result.Targets, root)
		}
		return nil
	}
	items, err := api.service.downloaders.List(ctx)
	if err != nil {
		return result, err
	}
	for _, item := range items {
		if item.Enabled == 0 || item.DownloadDir == "" {
			continue
		}
		var dirs []struct {
			SavePath      string `json:"save_path"`
			ContainerPath string `json:"container_path"`
		}
		if json.Unmarshal([]byte(item.DownloadDir), &dirs) != nil {
			return result, organization.ErrPath
		}
		for _, dir := range dirs {
			path := dir.SavePath
			if dir.ContainerPath != "" {
				path = dir.ContainerPath
			}
			if err := add("source", path, "下载器："+item.Name, ""); err != nil {
				return result, err
			}
		}
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return result, err
	}
	media := objectValue(snapshot["media"])
	for _, spec := range []struct{ key, label, kind string }{{"movie_path", "电影媒体库", "MOV"}, {"tv_path", "电视剧媒体库", "TV"}, {"anime_path", "动漫媒体库", "TV"}} {
		value := media[spec.key]
		if value == nil {
			continue
		}
		paths := []any{value}
		if values, ok := value.([]any); ok {
			paths = values
		}
		for _, value := range paths {
			path, ok := value.(string)
			if !ok {
				return result, organization.ErrPath
			}
			if err := add("target", path, spec.label, spec.kind); err != nil {
				return result, err
			}
		}
	}
	if api.service.databasePath == "" {
		return result, nil
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.service.databasePath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String())
	if err != nil {
		return result, err
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='CONFIG_SYNC_PATHS' AND type='table')`).Scan(&exists); err != nil {
		return result, err
	}
	if !exists {
		return result, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT ID,COALESCE(SOURCE,''),COALESCE(DEST,'') FROM CONFIG_SYNC_PATHS WHERE ENABLED=1 ORDER BY ID LIMIT 129`)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 128 {
			return result, organization.ErrLimit
		}
		var id int64
		var source, dest string
		if err := rows.Scan(&id, &source, &dest); err != nil {
			return result, err
		}
		label := "同步目录：" + strconv.FormatInt(id, 10)
		if err := add("source", source, label, ""); err != nil {
			return result, err
		}
		if err := add("target", dest, label, ""); err != nil {
			return result, err
		}
	}
	return result, rows.Err()
}

func (api organizationAPI) servePlan(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	var input organizationPlanInput
	if !decodeServiceRequest(w, r, &input, "invalid organization preview request") {
		return
	}
	if input.Path == "" {
		input.Path = "."
	}
	if input.Mode != "copy" && input.Mode != "move" && input.Mode != "link" && input.Mode != "softlink" {
		writeAPIError(w, 400, 400, "unsupported organization mode")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	plan, failure := api.plan(ctx, input)
	if failure != nil {
		writeAPIError(w, failure.status, failure.status, failure.message)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": plan})
}

func (api organizationAPI) plan(ctx context.Context, input organizationPlanInput) (organizationPlan, *recognitionFailure) {
	result := organizationPlan{Items: []organizationPlanItem{}, Mode: input.Mode, PreviewOnly: true}
	fail := func(status int, message string) (organizationPlan, *recognitionFailure) {
		return organizationPlan{}, &recognitionFailure{status, message}
	}
	if ctx.Err() != nil {
		return fail(504, "organization preview was cancelled or timed out")
	}
	roots, err := api.roots(ctx)
	if ctx.Err() != nil {
		return fail(504, "organization preview was cancelled or timed out")
	}
	if err != nil {
		return fail(503, "configured organization roots are unavailable")
	}
	var source, target organizationRoot
	for _, root := range roots.Sources {
		if root.ID == input.SourceID {
			source = root
		}
	}
	for _, root := range roots.Targets {
		if root.ID == input.TargetID {
			target = root
		}
	}
	if source.ID == "" || target.ID == "" {
		return fail(400, "select configured source and target roots")
	}
	sourcePath, e1 := filepath.EvalSymlinks(source.Path)
	targetPath, e2 := filepath.EvalSymlinks(target.Path)
	inside := func(a, b string) bool {
		relative, err := filepath.Rel(a, b)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
	}
	if e1 != nil || e2 != nil {
		return fail(422, "organization roots must be existing directories")
	}
	if inside(sourcePath, targetPath) || inside(targetPath, sourcePath) {
		return fail(422, "source and target roots must not overlap")
	}
	root, err := organization.OpenRoot(target.Path)
	if err != nil {
		return fail(422, "target root is unavailable")
	}
	root.Close()
	scan, err := organization.Scan(ctx, source.Path, input.Path)
	if err != nil {
		if ctx.Err() != nil {
			return fail(504, "organization preview was cancelled or timed out")
		}
		if errors.Is(err, organization.ErrLimit) {
			return fail(422, "organization scan exceeded limits; select a smaller folder")
		}
		return fail(422, "source path is invalid, unsafe or changed")
	}
	result.Skipped = scan.Skipped
	mediaCount := 0
	for _, file := range scan.Files {
		if file.Kind == "media" {
			mediaCount++
		}
	}
	if mediaCount > 100 {
		return fail(422, "preview is limited to 100 media files; select a smaller folder")
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return fail(503, "organization configuration is unavailable")
	}
	media := objectValue(snapshot["media"])
	sourceInfo, e1 := os.Stat(sourcePath)
	targetInfo, e2 := os.Stat(targetPath)
	if e1 != nil || e2 != nil || !sourceInfo.IsDir() || !targetInfo.IsDir() || os.SameFile(sourceInfo, targetInfo) {
		return fail(422, "organization roots changed")
	}
	result.definition = organization.Definition{SourceRoot: sourcePath, TargetRoot: targetPath, SourceIdentity: organization.Identity(sourceInfo), TargetIdentity: organization.Identity(targetInfo), ConfigDigest: organization.Digest(snapshot), SourceID: source.ID, TargetID: target.ID, Mode: input.Mode}
	if result.definition.ConfigDigest == "" {
		return fail(503, "organization configuration snapshot is unsupported")
	}
	minimumMB := int64(150)
	if value, exists := media["min_filesize"]; exists {
		minimumMB, err = strconv.ParseInt(text(value), 10, 64)
		if err != nil || minimumMB < 0 || minimumMB > 1000000 {
			return fail(422, "configured minimum media file size is invalid")
		}
	}
	selectionRoot, err := organization.OpenRoot(sourcePath)
	if err != nil {
		return fail(422, "source selection changed")
	}
	selectedInfo, err := selectionRoot.Lstat(input.Path)
	selectionRoot.Close()
	if err != nil {
		return fail(422, "source selection changed")
	}
	for _, file := range scan.Files {
		if ctx.Err() != nil {
			return fail(504, "organization preview was cancelled or timed out")
		}
		item := organizationPlanItem{Source: file.Path, Kind: file.Kind, Size: file.Size, Modified: strconv.FormatInt(file.Modified, 10), Identity: file.Identity, Status: "unmatched"}
		if file.Kind != "media" {
			result.Items = append(result.Items, item)
			continue
		}
		bluray := false
		if selectedInfo.IsDir() && file.Size < minimumMB*1024*1024 {
			item.Status, item.Reason = "blocked", "file is below the configured minimum media size"
			result.Items = append(result.Items, item)
			continue
		}
		for _, part := range strings.Split(filepath.ToSlash(file.Path), "/") {
			if strings.EqualFold(part, "BDMV") || strings.EqualFold(part, "CERTIFICATE") {
				bluray = true
			}
		}
		if bluray {
			item.Status, item.Reason = "blocked", "Blu-ray directory organization is not migrated"
			result.Items = append(result.Items, item)
			continue
		}
		recognition, failure := api.recognition.recognize(ctx, filepath.Base(file.Path), "")
		if ctx.Err() != nil {
			return fail(504, "organization preview was cancelled or timed out")
		}
		if failure != nil {
			item.Status, item.Reason = "blocked", failure.message
			result.Items = append(result.Items, item)
			continue
		}
		if recognition == nil {
			item.Status, item.Reason = "unrecognized", "media identity could not be verified"
			result.Items = append(result.Items, item)
			continue
		}
		item.TMDBID = text(recognition.data["tmdbid"])
		item.Title, item.Year, item.MediaType, item.SeasonEpisode = text(recognition.data["title"]), text(recognition.data["year"]), text(recognition.data["type"]), text(recognition.data["season_episode"])
		if categories := stringsOf(recognition.data["category"]); len(categories) > 0 {
			item.Category = categories[0]
		}
		kind := "MOV"
		if recognition.kind == "tv" {
			kind = "TV"
		}
		if target.Type != "" && target.Type != kind {
			item.Status, item.Reason = "blocked", "media type does not match selected target"
			result.Items = append(result.Items, item)
			continue
		}
		relative, err := api.targetName(ctx, media, recognition, filepath.Ext(file.Path))
		if ctx.Err() != nil {
			return fail(504, "organization preview was cancelled or timed out")
		}
		if err != nil {
			item.Status, item.Reason = "blocked", "naming template or season/episode metadata is unsupported"
			result.Items = append(result.Items, item)
			continue
		}
		item.Target = relative
		item.Status, err = organization.TargetStatus(ctx, target.Path, relative)
		if err != nil {
			if ctx.Err() != nil {
				return fail(504, "organization preview was cancelled or timed out")
			}
			return fail(422, "target path is unavailable or unsafe")
		}
		result.Items = append(result.Items, item)
	}
	markOrganizationConflicts(result.Items)
	// Sidecars attach only to exactly one named media stem in the same directory.
	// Ambiguous, unrelated and blocked sidecars are never guessed into a target.
	for i := range result.Items {
		item := &result.Items[i]
		if item.Kind == "media" {
			continue
		}
		var matches []organizationPlanItem
		for _, video := range result.Items {
			if video.Kind != "media" || filepath.Dir(video.Source) != filepath.Dir(item.Source) {
				continue
			}
			stem := strings.TrimSuffix(filepath.Base(video.Source), filepath.Ext(video.Source))
			name := filepath.Base(item.Source)
			if name == stem+filepath.Ext(item.Source) || strings.HasPrefix(name, stem+".") {
				matches = append(matches, video)
			}
		}
		if len(matches) != 1 || matches[0].Status != "available" {
			item.Reason = "companion has no unique available media target"
			continue
		}
		video := matches[0]
		stem := strings.TrimSuffix(filepath.Base(video.Source), filepath.Ext(video.Source))
		suffix := strings.TrimPrefix(filepath.Base(item.Source), stem)
		item.Target = strings.TrimSuffix(video.Target, filepath.Ext(video.Target)) + suffix
		item.TMDBID = video.TMDBID
		item.Title, item.Year, item.MediaType, item.Category, item.SeasonEpisode = video.Title, video.Year, video.MediaType, video.Category, video.SeasonEpisode
		item.Status, err = organization.TargetStatus(ctx, target.Path, item.Target)
		if err != nil {
			if ctx.Err() != nil {
				return fail(504, "organization preview was cancelled or timed out")
			}
			return fail(422, "companion target is unavailable or unsafe")
		}
	}
	markOrganizationConflicts(result.Items)
	if ctx.Err() != nil {
		return fail(504, "organization preview was cancelled or timed out")
	}
	for _, item := range result.Items {
		if item.Status == "available" {
			result.definition.Entries = append(result.definition.Entries, organization.Entry{Source: item.Source, Target: item.Target, Kind: item.Kind, Size: item.Size, Modified: item.Modified, Identity: item.Identity, TMDBID: item.TMDBID, Title: item.Title, Year: item.Year, MediaType: item.MediaType, Category: item.Category, SeasonEpisode: item.SeasonEpisode})
		}
	}
	// Publish primary media before companions; a failed media copy must not
	// leave subtitles/audio that appear to belong to an unpublished movie.
	sort.SliceStable(result.definition.Entries, func(i, j int) bool {
		return result.definition.Entries[i].Kind == "media" && result.definition.Entries[j].Kind != "media"
	})
	result.Fingerprint = organization.Digest(struct {
		Definition organization.Definition
		Items      []organizationPlanItem
	}{result.definition, result.Items})
	return result, nil
}

func markOrganizationConflicts(items []organizationPlanItem) {
	for i := range items {
		if items[i].Target == "" {
			continue
		}
		for j := i + 1; j < len(items); j++ {
			if items[i].Target == items[j].Target {
				items[i].Status, items[j].Status = "conflict", "conflict"
				items[i].Reason, items[j].Reason = "multiple sources share a target", "multiple sources share a target"
			}
		}
	}
}

func (api organizationAPI) targetName(ctx context.Context, media map[string]any, r *nativeRecognition, extension string) (string, error) {
	format := localNamingTemplate(media, r.kind == "tv")
	values := map[string]string{"title": localLibraryName(text(r.data["title"]), media), "name": localLibraryName(r.meta.Title, media), "year": text(r.data["year"]), "tmdbid": text(r.data["tmdbid"]), "original_title": localLibraryName(text(r.detail.Attributes["original_title"]), media), "part": "", "videoFormat": r.meta.Resolution, "videoCodec": r.meta.VideoCodec, "audioCodec": r.meta.AudioCodec, "resourceType": r.meta.Source, "resolution": r.meta.Resolution, "effect": r.meta.Effect, "season_episode": text(r.data["season_episode"]), "releaseGroup": localLibraryName(r.meta.Team, media), "customization": localLibraryName(r.meta.Customization, media)}
	year, _ := strconv.Atoi(values["year"])
	decade := year / 10 * 10
	values["decade_short"] = strconv.Itoa(decade) + "s"
	values["decade_long"] = strconv.Itoa(decade) + "-" + strconv.Itoa(decade+9)
	if imdb, ok := r.detail.Attributes["imdb_id"]; ok {
		values["imdbid"] = text(imdb)
	}
	if libraryTemplateUsesField(format, "en_title") {
		detail, err := fetchNativeTMDBDetailsLanguage(ctx, api.service.configStore, api.service.client.Transport, r.kind, text(r.data["tmdbid"]), "en")
		if err != nil {
			return "", err
		}
		title := detail.Title
		if r.kind == "tv" {
			title = detail.Name
		}
		if title == "" {
			return "", errLibraryFormat
		}
		values["en_title"] = localLibraryName(title, media)
	}
	if r.kind == "tv" {
		e := r.meta.Episodes
		if e.Season == nil || e.Episode == nil || e.EndSeason != nil {
			return "", errLibraryFormat
		}
		last := *e.Episode
		if e.EndEpisode != nil {
			last = *e.EndEpisode
		}
		verified := false
		for _, season := range r.detail.Seasons {
			if season.Number == *e.Season && season.Episodes != nil && *e.Episode > 0 && last >= *e.Episode && last <= *season.Episodes {
				verified = true
			}
		}
		if !verified {
			return "", errLibraryFormat
		}
		values["season"], values["episode"] = strconv.Itoa(*e.Season), strconv.Itoa(*e.Episode)
		if e.EndEpisode != nil {
			values["episode"] += "-" + strconv.Itoa(*e.EndEpisode)
		}
		values["original_title"] = localLibraryName(text(r.detail.Attributes["original_name"]), media)
	}
	relative, err := renderLocalDirectory(format, values)
	if err != nil {
		return "", err
	}
	categories := stringsOf(r.data["category"])
	if len(categories) > 0 {
		category := categories[0]
		if filepath.Base(category) != category || category == "." || category == ".." || strings.ContainsAny(category, "\\\x00") {
			return "", errLibraryFormat
		}
		relative = filepath.Join(category, relative)
	}
	return relative + strings.ToLower(extension), nil
}
