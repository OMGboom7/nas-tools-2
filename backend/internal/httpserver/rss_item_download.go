package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/rsstaskconfig"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
)

type rssItemDownloadAPI struct {
	tasks       *rsstaskconfig.Store
	downloaders *downloaderconfig.Store
	system      *systemconfig.Store
	sites       *siteconfig.Store
	config      *config.Store
	service     downloadService
}

type rssDownloadArticle struct {
	Title     string `json:"title"`
	Enclosure string `json:"enclosure"`
	Link      string `json:"link"`
	Year      string `json:"year"`
}

func (api rssItemDownloadAPI) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	if api.tasks == nil || api.downloaders == nil || api.system == nil || api.sites == nil || api.config == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native RSS download is unavailable")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 256<<10)
	if err := request.ParseForm(); err != nil {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS download request")
		return
	}
	taskID, err := strconv.ParseInt(strings.TrimSpace(request.Form.Get("taskid")), 10, 64)
	if err != nil || taskID <= 0 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS task id")
		return
	}
	rawArticles := request.Form.Get("articles")
	if rawArticles == "" || rawArticles == "null" || rawArticles == "[]" {
		writeJSON(response, http.StatusOK, map[string]any{"code": 2})
		return
	}
	var articles []rssDownloadArticle
	if json.Unmarshal([]byte(rawArticles), &articles) != nil || len(articles) == 0 || len(articles) > 100 {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS articles")
		return
	}
	for _, article := range articles {
		if strings.TrimSpace(article.Title) == "" || len(article.Title) > 512 || len(article.Enclosure) > 4096 || len(article.Link) > 4096 || len(article.Year) > 20 || !rssDownloadURLAllowed(article.Enclosure) {
			writeAPIError(response, http.StatusBadRequest, 400, "invalid RSS article")
			return
		}
	}
	ctx, cancel := context.WithTimeout(request.Context(), 60*time.Second)
	defer cancel()
	task, err := api.tasks.Get(ctx, taskID)
	if errors.Is(err, rsstaskconfig.ErrNotFound) {
		writeJSON(response, http.StatusOK, map[string]any{"code": 1})
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS task is unavailable")
		return
	}
	downloaderID, options, err := rssDownloadSettings(ctx, task, api.downloaders, api.system)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS download settings are unavailable")
		return
	}
	downloader, _, handled, err := api.service.configuredDownloader(ctx, downloaderID)
	if err != nil || !handled || downloader.ID == 0 || downloader.Enabled == 0 {
		writeJSON(response, http.StatusOK, map[string]any{"code": 1})
		return
	}
	if !rssOptionsSupported(downloader.Type, options) {
		writeAPIError(response, http.StatusNotImplemented, 501, "RSS download settings are not supported by this downloader")
		return
	}
	sites, err := api.sites.List(ctx)
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS sites are unavailable")
		return
	}
	configuration, err := api.config.Snapshot()
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "RSS configuration is unavailable")
		return
	}
	for _, article := range articles {
		processed, err := api.tasks.IsProcessed(ctx, "D", article.Title, article.Year, article.Enclosure)
		if err != nil {
			writeAPIError(response, 503, 503, "RSS download history is unavailable")
			return
		}
		if processed {
			continue
		}
		pending, err := api.tasks.HasPendingDownload(ctx, article.Enclosure)
		if err != nil {
			writeAPIError(response, 503, 503, "RSS submission state is unavailable")
			return
		}
		if pending {
			writeAPIError(response, 409, 409, "RSS submission requires downloader verification before retrying")
			return
		}
		magnet := ""
		var torrent []byte
		if validMagnet(article.Enclosure) {
			magnet = article.Enclosure
		} else {
			contents, err := api.fetchTorrent(ctx, task.Note, article, sites, configuration)
			if err != nil {
				writeJSON(response, http.StatusOK, map[string]any{"code": 1})
				return
			}
			if validMagnet(string(contents)) {
				magnet = string(contents)
			} else {
				torrent = contents
			}
		}
		reserved, err := api.tasks.ReserveDownload(ctx, taskID, article.Enclosure)
		if err != nil {
			writeAPIError(response, 503, 503, "RSS resource could not be reserved")
			return
		}
		if !reserved {
			pending, err := api.tasks.HasPendingDownload(ctx, article.Enclosure)
			if err != nil || pending {
				writeAPIError(response, 409, 409, "RSS submission requires downloader verification before retrying")
				return
			}
			continue
		}
		_, handled, err := api.service.nativeAddDownloadWithOptions(ctx, magnet, torrent, strconv.FormatInt(downloader.ID, 10), options)
		if (err != nil || !handled) && downloadDefinitelyNotSubmitted(handled, err) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			releaseErr := api.tasks.ReleaseUnsubmittedDownload(cleanup, taskID, article.Enclosure)
			cancel()
			if releaseErr != nil {
				writeAPIError(response, 503, 503, "RSS reservation could not be released after a rejected submission")
				return
			}
		}
		if !handled {
			writeAPIError(response, http.StatusNotImplemented, 501, "downloader type is not migrated")
			return
		}
		if err != nil {
			writeJSON(response, http.StatusOK, map[string]any{"code": 1})
			return
		}
		if err := api.tasks.CompleteReservedManualDownload(ctx, taskID, article.Title, article.Year, article.Enclosure, downloader.Name); err != nil {
			writeAPIError(response, http.StatusBadGateway, 502, "RSS download succeeded but history could not be saved")
			return
		}
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0})
}

func rssDownloadURLAllowed(raw string) bool {
	if validMagnet(raw) {
		return true
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func rssOptionsSupported(kind string, options downloadAddOptions) bool {
	switch kind {
	case "qbittorrent":
		return true
	case "transmission":
		return options.Category == "" && options.UploadLimitKB == 0 && options.DownloadLimitKB == 0 && options.RatioLimit == 0 && options.SeedingTimeLimit == 0
	case "aria2":
		return options.Category == "" && len(options.Tags) == 0 && options.RatioLimit == 0 && options.SeedingTimeLimit == 0
	case "pan115":
		return options.empty()
	default:
		return false
	}
}

func (api rssItemDownloadAPI) fetchTorrent(ctx context.Context, taskNote string, article rssDownloadArticle, sites []siteconfig.Site, configuration map[string]any) ([]byte, error) {
	parsed, _ := url.Parse(article.Enclosure)
	var site *siteconfig.Site
	for index := range sites {
		for _, source := range []string{sites[index].SignURL, sites[index].RSSURL} {
			base, err := url.Parse(source)
			if err == nil && base.Hostname() != "" && strings.EqualFold(base.Host, parsed.Host) && base.Scheme == parsed.Scheme {
				site = &sites[index]
				break
			}
		}
		if site != nil {
			break
		}
	}
	var note map[string]any
	_ = json.Unmarshal([]byte(taskNote), &note)
	transport := api.service.client.Transport
	if note["proxy"] == true || note["proxy"] == "Y" || note["proxy"] == "1" {
		var err error
		transport, err = siteProxyTransport(transport, objectValue(objectValue(configuration["app"])["proxies"]), parsed.Scheme)
		if err != nil {
			return nil, err
		}
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	if site != nil {
		request.Header.Set("Cookie", site.Cookie)
		var siteNote map[string]any
		_ = json.Unmarshal([]byte(site.Note), &siteNote)
		ua := strings.TrimSpace(text(siteNote["ua"]))
		if ua == "" {
			ua = defaultSiteUserAgent
		}
		request.Header.Set("User-Agent", ua)
		if siteNote["referer"] == true || siteNote["referer"] == "Y" {
			if ref, err := url.Parse(article.Link); err == nil && ref.Host == parsed.Host && ref.Scheme == parsed.Scheme {
				request.Header.Set("Referer", ref.String())
			}
		}
	}
	result, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		return nil, errors.New("RSS torrent request failed")
	}
	contents, err := io.ReadAll(io.LimitReader(result.Body, (8<<20)+1))
	if err != nil || len(contents) == 0 || len(contents) > 8<<20 {
		return nil, errors.New("invalid RSS torrent response")
	}
	if validMagnet(string(contents)) {
		return contents, nil
	}
	if _, err := torrentmeta.Parse(contents); err != nil {
		return nil, err
	}
	return contents, nil
}
