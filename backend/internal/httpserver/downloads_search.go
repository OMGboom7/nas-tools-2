package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/searchcache"
)

func (service downloadService) addNativeSearchResource(response http.ResponseWriter, request *http.Request, input addResourceRequest) {
	if service.resources == nil || service.auth == nil || service.configStore == nil || service.sites == nil {
		writeAPIError(response, 503, 503, "native search resources are unavailable")
		return
	}
	claims, err := service.auth.service.VerifyToken(request.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(response, 401, 401, "authorization token is invalid or expired")
		return
	}
	user, err := service.auth.service.FindUser(request.Context(), claims.Username)
	if err != nil {
		writeAPIError(response, 401, 401, "user is unavailable")
		return
	}
	allowed := false
	for _, permission := range user.Permissions {
		allowed = allowed || permission == "下载管理"
	}
	if !allowed {
		writeAPIError(response, 403, 403, "download permission is required")
		return
	}
	owner := strconv.FormatInt(user.ID, 10) + ":" + user.Name
	resource, done, err := service.resources.Claim(owner, input.ResourceID)
	if err != nil {
		status := 404
		if errors.Is(err, searchcache.ErrBusy) {
			status = 409
		}
		writeAPIError(response, status, status, "search resource is unavailable, expired, or already downloading")
		return
	}
	if done {
		writeJSON(response, 200, map[string]any{"code": 0, "success": true, "message": "该资源已提交下载"})
		return
	}
	succeeded := false
	defer func() { service.resources.Finish(owner, input.ResourceID, succeeded) }()
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	downloader, options, err := service.searchDownloadSettings(ctx, resource.Indexer, input)
	if errors.Is(err, errSearchDownloadSelection) {
		writeAPIError(response, 400, 400, "invalid download setting or directory selection")
		return
	}
	if errors.Is(err, errDownloadOptionsUnsupported) {
		writeAPIError(response, 501, 501, "selected downloader does not support these download settings")
		return
	}
	if err != nil {
		writeAPIError(response, 503, 503, "selected download configuration is unavailable or disabled")
		return
	}
	if downloader.Type != "qbittorrent" && downloader.Type != "transmission" && downloader.Type != "aria2" && downloader.Type != "pan115" {
		writeAPIError(response, 501, 501, "selected downloader type is not migrated")
		return
	}
	magnet, torrent, err := service.prepareNativeSearchTorrent(ctx, resource)
	if err != nil {
		writeAPIError(response, 502, 502, "search torrent could not be retrieved")
		return
	}
	_, handled, err := service.nativeAddDownloadWithOptions(ctx, magnet, torrent, strconv.FormatInt(downloader.ID, 10), options)
	if !handled {
		writeAPIError(response, 501, 501, "selected downloader type is not migrated")
		return
	}
	if errors.Is(err, errDownloadOptionsUnsupported) {
		writeAPIError(response, 501, 501, "selected downloader does not support these download settings")
		return
	}
	if err != nil {
		writeAPIError(response, 502, 502, "download submission failed; check downloader tasks before retrying")
		return
	}
	succeeded = true
	writeJSON(response, 200, map[string]any{"code": 0, "success": true, "message": "已提交下载器"})
}

// Shared by interactive and subscription downloads; tracker credentials and
// private locators never enter planning responses.
func (service downloadService) prepareNativeSearchTorrent(ctx context.Context, resource externalindexer.Resource) (string, []byte, error) {
	if validMagnet(resource.DownloadURL) {
		return resource.DownloadURL, nil, nil
	}
	sites, err := service.sites.List(ctx)
	if err != nil {
		return "", nil, err
	}
	configuration, err := service.configStore.Snapshot()
	if err != nil {
		return "", nil, err
	}
	var contents []byte
	switch resource.DownloadResolver {
	case "mteam":
		contents, err = service.fetchMTeamSearchTorrent(ctx, resource.DownloadURL, sites, configuration)
	case "":
		contents, err = (rssItemDownloadAPI{service: service}).fetchTorrent(ctx, "", rssDownloadArticle{Title: resource.Title, Enclosure: resource.DownloadURL, Link: resource.PageURL}, sites, configuration)
	default:
		err = errors.New("unsupported native download resolver")
	}
	if err != nil {
		return "", nil, err
	}
	if validMagnet(string(contents)) {
		return string(contents), nil, nil
	}
	return "", contents, nil
}

func (service downloadService) prepareSubscriptionTorrent(ctx context.Context, candidate subscriptionCandidate) (string, []byte, error) {
	if candidate.site == nil || validMagnet(candidate.resource.DownloadURL) {
		return service.prepareNativeSearchTorrent(ctx, candidate.resource)
	}
	if !subscriptionSiteOriginAllowed(*candidate.site, candidate.resource.DownloadURL) {
		return "", nil, errSiteLinkNotAllowed
	}
	contents, err := service.fetchSiteTorrent(ctx, *candidate.site, candidate.resource.DownloadURL)
	if err != nil {
		return "", nil, err
	}
	if validMagnet(string(contents)) {
		return string(contents), nil, nil
	}
	return "", contents, nil
}
