package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/filterconfig"
	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/0xforee/nas-tools/backend/internal/wordconfig"
	"github.com/dlclark/regexp2"
)

type rssRunAPI struct {
	preview     rssPreviewAPI
	download    rssItemDownloadAPI
	filters     *filterconfig.Store
	recognition mediaNameAPI
	pureGo      bool
	mu          sync.Mutex
	running     map[int64]bool
}

type rssRunResult struct {
	Total      int `json:"total"`
	Downloaded int `json:"downloaded"`
	Skipped    int `json:"skipped"`
	Uncertain  int `json:"uncertain"`
}

func (api *rssRunAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	auth := api.download.service.auth
	if auth == nil {
		writeAPIError(w, 503, 503, "native RSS execution is unavailable")
		return
	}
	claims, err := auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return
	}
	if !auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission is required")
		return
	}
	if !api.pureGo {
		writeAPIError(w, 501, 501, "native RSS execution requires disabled legacy backend")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil {
		writeAPIError(w, 400, 400, "invalid RSS execution request")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("id")), 10, 64)
	if err != nil || id <= 0 {
		writeAPIError(w, 400, 400, "invalid RSS task id")
		return
	}
	api.mu.Lock()
	if api.running == nil {
		api.running = map[int64]bool{}
	}
	if api.running[id] {
		api.mu.Unlock()
		writeAPIError(w, 409, 409, "RSS task is already running")
		return
	}
	api.running[id] = true
	api.mu.Unlock()
	defer func() { api.mu.Lock(); delete(api.running, id); api.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, failure := api.run(ctx, id)
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"code": failure.status, "success": false, "message": failure.message, "data": result})
		return
	}
	code, message := 0, "RSS task execution finished"
	if result.Uncertain > 0 {
		code, message = 1, "some submissions require downloader verification and will not be retried automatically"
	}
	writeJSON(w, 200, map[string]any{"code": code, "success": code == 0, "message": message, "data": result})
}

func (api *rssRunAPI) run(ctx context.Context, id int64) (rssRunResult, *recognitionFailure) {
	result := rssRunResult{}
	fail := func(status int, message string) (rssRunResult, *recognitionFailure) {
		return result, &recognitionFailure{status, message}
	}
	if api.preview.tasks == nil || api.preview.parsers == nil || api.preview.config == nil || api.filters == nil || api.download.downloaders == nil || api.download.system == nil || api.download.sites == nil {
		return fail(503, "native RSS execution configuration is unavailable")
	}
	task, err := api.preview.tasks.Get(ctx, id)
	if errors.Is(err, rsstaskconfig.ErrNotFound) {
		return fail(404, "RSS task is unavailable")
	}
	if err != nil {
		return fail(503, "RSS task configuration is unavailable")
	}
	if task.Uses != "D" {
		return fail(501, "RSS subscription/search task execution is not migrated")
	}
	var note map[string]any
	if task.Note != "" && (len(task.Note) > 64<<10 || json.Unmarshal([]byte(task.Note), &note) != nil) {
		return fail(422, "invalid RSS task settings")
	}
	mode := task.Recognization
	if mode == "" {
		mode = text(note["recognization"])
	}
	if mode == "" {
		mode = "Y"
	}
	if mode != "Y" && mode != "N" {
		return fail(422, "invalid RSS recognition mode")
	}
	if task.SavePath == "" {
		task.SavePath = text(note["save_path"])
	}
	if mode == "Y" && task.MediaInfos != "" {
		var infos []map[string]any
		if len(task.MediaInfos) > 1<<20 || json.Unmarshal([]byte(task.MediaInfos), &infos) != nil || infos == nil {
			return fail(422, "invalid RSS media identities")
		}
	}
	group := int64(0)
	if task.Filter != "" {
		group, err = strconv.ParseInt(task.Filter, 10, 64)
		if err != nil || group < -1 {
			return fail(422, "invalid RSS filter group")
		}
	}
	groups, err := api.filters.List(ctx)
	if err != nil {
		return fail(503, "RSS filters are unavailable")
	}
	if _, err := filterconfig.MatchGroups(groups, group, filterconfig.TorrentMetadata{RequireKnown: true}); err != nil {
		return fail(422, "invalid RSS filter configuration")
	}
	conditions := []*regexp2.Regexp{nil, nil}
	for i, raw := range []string{task.Include, task.Exclude} {
		if raw == "" {
			continue
		}
		if len(raw) > 16<<10 {
			return fail(422, "RSS conditions exceeded limit")
		}
		conditions[i], err = regexcompat.Compile(raw, regexp2.IgnoreCase)
		if err != nil {
			return fail(422, "invalid RSS condition")
		}
	}
	downloaderID, options, err := rssDownloadSettings(ctx, task, api.download.downloaders, api.download.system)
	if err != nil {
		return fail(503, "RSS download settings are unavailable")
	}
	downloader, _, handled, err := api.download.service.configuredDownloader(ctx, downloaderID)
	if err != nil || !handled || downloader.ID == 0 || downloader.Enabled == 0 {
		return fail(503, "RSS downloader is unavailable or disabled")
	}
	if !rssOptionsSupported(downloader.Type, options) {
		return fail(501, "RSS downloader does not support these settings")
	}
	configuration, err := api.preview.config.Snapshot()
	if err != nil {
		return fail(503, "RSS configuration is unavailable")
	}
	sites, err := api.download.sites.List(ctx)
	if err != nil {
		return fail(503, "RSS sites are unavailable")
	}
	addresses, parsers := rssStringList(task.Address), rssValueList(task.Parser)
	if len(addresses) == 0 || len(addresses) > 100 || len(addresses) != len(parsers) {
		return fail(422, "invalid RSS sources")
	}
	articles := []rssPreviewArticle{}
	for i, address := range addresses {
		parserID, err := strconv.ParseInt(text(parsers[i]), 10, 64)
		if err != nil || parserID <= 0 {
			return fail(422, "invalid RSS parser selection")
		}
		parser, err := api.preview.parsers.Get(ctx, parserID)
		if err != nil {
			return fail(422, "RSS parser is unavailable")
		}
		var format rssFormat
		if json.Unmarshal([]byte(parser.Format), &format) != nil || format.List == "" || len(format.Item) == 0 {
			return fail(422, "invalid RSS parser format")
		}
		body, err := api.preview.fetch(ctx, address, parser.Params, task.Note, configuration)
		if err != nil {
			return fail(502, "RSS feed could not be retrieved")
		}
		items, err := parseRSSPreview(body, parser.Type, format, i+1)
		if err != nil {
			return fail(502, "RSS feed could not be parsed")
		}
		articles = append(articles, items...)
		if len(articles) > 1000 {
			return fail(422, "RSS resource limit exceeded")
		}
	}
	result.Total = len(articles)
	for _, article := range articles {
		if ctx.Err() != nil {
			return fail(504, "RSS execution was cancelled or timed out")
		}
		if article.Title == "" || article.Enclosure == "" {
			result.Skipped++
			continue
		}
		if len(article.Title) > 512 || len(article.Enclosure) > 4096 || len(article.Link) > 4096 || !rssDownloadURLAllowed(article.Enclosure) || (article.Type != "" && article.Type != "movie" && article.Type != "tv") {
			return fail(422, "invalid RSS resource")
		}
		processed, err := api.preview.tasks.IsProcessed(ctx, "D", article.Title, article.Year, article.Enclosure)
		if err != nil {
			return fail(503, "RSS history is unavailable")
		}
		if processed {
			result.Skipped++
			continue
		}
		pending, err := api.preview.tasks.HasPendingDownload(ctx, article.Enclosure)
		if err != nil {
			return fail(503, "RSS submission state is unavailable")
		}
		if pending {
			result.Uncertain++
			continue
		}
		title := article.Title
		if article.Year != "" {
			title += " " + article.Year
		}
		meta, rev, err := api.offlineMetadata(ctx, title, article.Description)
		if err != nil {
			return fail(422, "RSS name parsing failed")
		}
		if article.Type != "" {
			meta.Episodes.TV = article.Type == "tv"
		}
		var recognized *nativeRecognition
		if mode == "Y" {
			var failure *recognitionFailure
			recognized, failure = api.recognition.recognizeKind(ctx, title, article.Description, article.Type)
			if failure != nil {
				return result, failure
			}
			if recognized == nil {
				result.Skipped++
				continue
			}
			meta, rev = recognized.meta, text(recognized.data["rev_string"])
			coverage, err := api.recognition.service.nativeMediaExistence(ctx, meta, recognized.detail, recognized.kind, text(recognized.data["category"]))
			if err != nil {
				return fail(503, "RSS media inventory could not be verified")
			}
			if coverage.Complete {
				result.Skipped++
				continue
			}
		}
		matches := true
		for i, condition := range conditions {
			if condition == nil {
				continue
			}
			found, err := condition.MatchString(rev)
			if err != nil {
				return fail(422, "RSS condition matching failed")
			}
			matches = matches && (found != (i == 1))
		}
		if !matches {
			result.Skipped++
			continue
		}
		var size int64
		if article.rawSize != "" {
			size, err = builtinindexer.SizeBytes(article.rawSize)
			if err != nil {
				return fail(422, "invalid RSS resource size")
			}
		}
		matched, err := filterconfig.MatchGroups(groups, group, filterconfig.TorrentMetadata{Title: rev, Subtitle: article.Description, SizeBytes: float64(size), Movie: !meta.Episodes.TV, Episodes: meta.Episodes.Count, RequireKnown: true})
		if err != nil {
			return fail(422, "RSS filter matching failed")
		}
		if !matched.Matched {
			result.Skipped++
			continue
		}
		magnet := article.Enclosure
		var torrent []byte
		if !validMagnet(magnet) {
			contents, err := api.download.fetchTorrent(ctx, task.Note, rssDownloadArticle{Title: article.Title, Year: article.Year, Enclosure: article.Enclosure, Link: article.Link}, sites, configuration)
			if err != nil {
				return fail(502, "RSS torrent could not be retrieved")
			}
			if validMagnet(string(contents)) {
				magnet = string(contents)
			} else {
				magnet = ""
				torrent = contents
			}
		}
		reserved, err := api.preview.tasks.ReserveDownload(ctx, id, article.Enclosure)
		if err != nil {
			return fail(503, "RSS resource could not be reserved")
		}
		if !reserved {
			pending, err := api.preview.tasks.HasPendingDownload(ctx, article.Enclosure)
			if err != nil {
				return fail(503, "RSS submission state is unavailable")
			}
			if pending {
				result.Uncertain++
			} else {
				result.Skipped++
			}
			continue
		}
		_, handled, err = api.download.service.nativeAddDownloadWithOptions(ctx, magnet, torrent, strconv.FormatInt(downloader.ID, 10), options)
		if err != nil || !handled {
			if downloadDefinitelyNotSubmitted(handled, err) {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				releaseErr := api.preview.tasks.ReleaseUnsubmittedDownload(cleanup, id, article.Enclosure)
				cancel()
				if releaseErr != nil {
					return fail(503, "RSS reservation could not be released after a rejected submission")
				}
				return fail(422, "RSS downloader rejected configuration or authentication before submission")
			}
			result.Uncertain++
			continue
		}
		var tv *rsstaskconfig.TVIdentity
		if recognized != nil && recognized.kind == "tv" {
			season := 1
			if meta.Episodes.Season != nil {
				season = *meta.Episodes.Season
			}
			tv = &rsstaskconfig.TVIdentity{ID: strconv.FormatInt(recognized.detail.ID, 10), Name: text(recognized.data["title"]), Season: season}
		}
		if err := api.preview.tasks.CompleteReservedDownload(ctx, id, article.Title, article.Year, article.Enclosure, downloader.Name, tv); err != nil {
			result.Uncertain++
			return fail(502, "RSS submission succeeded but persistence requires verification")
		}
		result.Downloaded++
	}
	return result, nil
}

func (api *rssRunAPI) offlineMetadata(ctx context.Context, title, subtitle string) (mediameta.Metadata, string, error) {
	service := api.recognition.service
	if service.words == nil || service.systemConfig == nil {
		return mediameta.Metadata{}, "", errors.New("native recognition is unavailable")
	}
	_, words, err := service.words.List(ctx)
	if err != nil {
		return mediameta.Metadata{}, "", err
	}
	processed, err := wordconfig.ProcessWords(ctx, title, words)
	if err != nil {
		return mediameta.Metadata{}, "", err
	}
	processedSubtitle, err := wordconfig.ProcessWords(ctx, subtitle, words)
	if err != nil {
		return mediameta.Metadata{}, "", err
	}
	options, err := mediameta.ReadLabelOptions(ctx, service.systemConfig)
	if err != nil {
		return mediameta.Metadata{}, "", err
	}
	meta, err := mediameta.ParseWithOptions(ctx, processed.Title, processedSubtitle.Title, options)
	return meta, processed.Title, err
}
