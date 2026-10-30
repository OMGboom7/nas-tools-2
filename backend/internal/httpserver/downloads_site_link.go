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

	"github.com/0xforee/nas-tools/backend/internal/aria2"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/qbittorrent"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/torrentmeta"
	"github.com/0xforee/nas-tools/backend/internal/transmission"
)

var errSiteLinkNotAllowed = errors.New("torrent URL is not on the selected site")

func (service downloadService) addSiteLink(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") == "" {
		writeAPIError(response, http.StatusUnauthorized, 401, "missing authorization token")
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, 8<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		SiteID string `json:"siteId"`
		URL    string `json:"url"`
	}
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid torrent link request")
		return
	}
	siteID, err := strconv.ParseInt(strings.TrimSpace(input.SiteID), 10, 64)
	if err != nil || siteID < 1 || len(input.URL) > 4096 {
		writeAPIError(response, http.StatusBadRequest, 400, "valid site and torrent URL are required")
		return
	}
	if service.sites == nil {
		writeAPIError(response, http.StatusServiceUnavailable, 503, "native site configuration is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
	defer cancel()
	site, err := service.sites.Get(ctx, siteID)
	if errors.Is(err, siteconfig.ErrNotFound) {
		writeAPIError(response, http.StatusNotFound, 404, "site not found")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "site configuration is unavailable")
		return
	}
	contents, err := service.fetchSiteTorrent(ctx, site, input.URL)
	if errors.Is(err, errSiteLinkNotAllowed) {
		writeAPIError(response, http.StatusBadRequest, 400, "torrent URL does not match the selected site")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "torrent file could not be fetched")
		return
	}
	var id string
	var handled bool
	if validMagnet(string(contents)) {
		id, handled, err = service.submitManual(ctx, request, string(contents), nil, nil, downloadAddOptions{}, "", "站点链接")
	} else {
		id, handled, err = service.submitManual(ctx, request, "", contents, nil, downloadAddOptions{}, "", "站点链接")
	}
	if !handled {
		writeAPIError(response, http.StatusNotImplemented, 501, "downloader type is not migrated")
		return
	}
	if manualSubmissionError(response, err) {
		return
	}
	if errors.Is(err, qbittorrent.ErrConfiguration) || errors.Is(err, transmission.ErrConfiguration) || errors.Is(err, aria2.ErrConfiguration) {
		writeAPIError(response, http.StatusBadRequest, 400, "invalid downloader configuration")
		return
	}
	if err != nil {
		writeAPIError(response, http.StatusBadGateway, 502, "torrent task could not be added")
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"code": 0, "success": true, "data": map[string]any{"id": id}})
}

func (service downloadService) fetchSiteTorrent(ctx context.Context, site siteconfig.Site, rawURL string) ([]byte, error) {
	return service.fetchSiteTorrentBound(ctx, site, rawURL, nil)
}
func (service downloadService) fetchSiteTorrentBound(ctx context.Context, site siteconfig.Site, rawURL string, guard func(context.Context) error) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Fragment != "" || parsed.Hostname() == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errSiteLinkNotAllowed
	}
	allowed := false
	for _, source := range []string{site.SignURL, site.RSSURL} {
		base, err := url.Parse(source)
		if err == nil && base.Hostname() != "" && strings.EqualFold(base.Host, parsed.Host) && base.Scheme == parsed.Scheme {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errSiteLinkNotAllowed
	}
	note := map[string]any{}
	if site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
		return nil, errors.New("invalid site settings")
	}
	if guard == nil && (text(note["chrome"]) == "Y" || truthy(note["chrome"])) {
		if service.configStore == nil {
			return nil, errors.New("browser settings are unavailable")
		}
		cfg, e := service.configStore.Snapshot()
		if e != nil {
			return nil, e
		}
		original := site
		if service.sites != nil {
			original, e = service.sites.Get(ctx, site.ID)
			if e != nil {
				return nil, e
			}
			if !subscriptionSiteOriginAllowed(original, rawURL) {
				return nil, errSiteLinkNotAllowed
			}
		}
		home := parsed.Scheme + "://" + parsed.Host + "/"
		_, session, e := (siteBrowser{renderer: service.browser, sites: service.sites, config: service.configStore, limits: service.siteLimits, transport: service.client.Transport}).render(ctx, original, cfg, home, nil, "", "", nil)
		if e != nil {
			return nil, e
		}
		magnet, body, e := service.prepareBrowserSearchTorrent(ctx, externalindexer.Resource{DownloadURL: rawURL, Browser: session})
		if magnet != "" {
			body = []byte(magnet)
		}
		return body, e
	}
	transport := service.client.Transport
	if text(note["proxy"]) == "Y" {
		if service.configStore == nil {
			return nil, errors.New("proxy settings are unavailable")
		}
		configuration, err := service.configStore.Snapshot()
		if err != nil {
			return nil, err
		}
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
	userAgent := strings.TrimSpace(text(note["ua"]))
	if userAgent == "" {
		userAgent = defaultSiteUserAgent
	}
	request.Header.Set("User-Agent", userAgent)
	if site.Cookie != "" {
		request.Header.Set("Cookie", site.Cookie)
	}
	if guard != nil {
		if err := guard(ctx); err != nil {
			return nil, err
		}
	}
	if site.ID > 0 {
		if err := service.siteLimits.Wait(ctx, service.sites, site.ID); err != nil {
			return nil, err
		}
	}
	if guard != nil {
		if err := guard(ctx); err != nil {
			return nil, err
		}
	}
	result, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	if guard != nil {
		if err := guard(ctx); err != nil {
			return nil, err
		}
	}
	if result.StatusCode != http.StatusOK {
		return nil, errors.New("site returned a non-success status")
	}
	contents, err := io.ReadAll(io.LimitReader(result.Body, (8<<20)+1))
	if err != nil || len(contents) == 0 || len(contents) > 8<<20 {
		return nil, errors.New("invalid torrent response")
	}
	if validMagnet(string(contents)) {
		return contents, nil
	}
	if _, err := torrentmeta.Parse(contents); err != nil {
		return nil, errors.New("site did not return a torrent")
	}
	return contents, nil
}
