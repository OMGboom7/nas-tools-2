package httpserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
)

// Old clients post an indexer ID. The site page also uses the catalog domain.
// Only the matching catalog entry can choose where the stored credentials go.
func (s *nativeExternalResourceSearch) siteResourcePage(fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
		if err != nil {
			writeAPIError(w, 400, 400, "站点资源请求过大")
			return
		}
		probe := r.Clone(r.Context())
		probe.Body = io.NopCloser(bytes.NewReader(body))
		if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
			err = probe.ParseMultipartForm(128 << 10)
			if probe.MultipartForm != nil {
				defer probe.MultipartForm.RemoveAll()
			}
		} else {
			err = probe.ParseForm()
		}
		if err != nil {
			writeAPIError(w, 400, 400, "站点资源表单无效")
			return
		}
		id := strings.TrimSpace(probe.PostForm.Get("id"))
		if id == "" {
			id = strings.TrimSpace(probe.PostForm.Get("site"))
		}
		if id == "" {
			writeAPIError(w, 400, 400, "请选择站点")
			return
		}
		external := strings.HasSuffix(id, "-jackett") || strings.HasSuffix(id, "-prowlarr")
		var def *indexercatalog.Definition
		if !external && s.catalogPath != "" {
			catalog, err := indexercatalog.Load(s.catalogPath)
			if err != nil {
				writeAPIError(w, 503, 503, "站点目录不可用")
				return
			}
			domain := indexercatalog.Domain(id)
			for i := range catalog.Indexers {
				item := &catalog.Indexers[i]
				host := indexercatalog.Domain(item.Domain)
				if (item.Parser != "MTeamSpider" && item.Parser != "TNodeSpider") || (item.ID != id && host != domain && !strings.EqualFold(host, id)) {
					continue
				}
				if def != nil {
					writeAPIError(w, 409, 409, "站点标识匹配多条目录记录")
					return
				}
				def = item
			}
		}
		if def == nil && !external {
			r.Body = io.NopCloser(bytes.NewReader(body))
			fallback.ServeHTTP(w, r)
			return
		}
		claims, err := s.auth.service.VerifyToken(r.Header.Get("Authorization"))
		if err != nil {
			writeAPIError(w, 401, 401, "请重新登录")
			return
		}
		user, err := s.auth.service.FindUser(r.Context(), claims.Username)
		if err != nil {
			writeAPIError(w, 401, 401, "当前账户不可用")
			return
		}
		page := 0
		if raw := probe.PostForm.Get("page"); raw != "" {
			page, err = strconv.Atoi(raw)
			if err != nil || page < 0 || page > 10000 {
				writeAPIError(w, 400, 400, "页码无效")
				return
			}
		}
		var keyword *string
		if probe.PostForm.Has("keyword") {
			value := probe.PostForm.Get("keyword")
			keyword = &value
		}
		if keyword != nil && len(*keyword) > 1024 {
			writeAPIError(w, 400, 400, "关键词过长")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		var rows []any
		if external {
			if !slices.Contains(user.Permissions, "资源搜索") {
				writeAPIError(w, 403, 403, "需要资源搜索权限")
				return
			}
			rows, err = s.externalSitePage(ctx, id, probe.PostForm.Get("keyword"), page, strconv.FormatInt(user.ID, 10)+":"+user.Name)
		} else {
			rows, err = s.readSiteResourcePage(ctx, *def, page, keyword)
		}
		if err != nil || (!external && len(rows) == 0) {
			writeJSON(w, 200, map[string]any{"code": 1, "success": false, "message": "获取站点资源出现错误，无法连接到站点！", "data": map[string]any{}})
			return
		}
		writeJSON(w, 200, map[string]any{"code": 0, "success": true, "message": "", "data": map[string]any{"data": rows}})
	})
}

func (s *nativeExternalResourceSearch) readSiteResourcePage(ctx context.Context, def indexercatalog.Definition, page int, keyword *string) ([]any, error) {
	sites, err := s.sites.List(ctx)
	if err != nil {
		return nil, err
	}
	site, found, err := configuredIndexerSite(def, sites)
	if err != nil || !found {
		return nil, builtinindexer.ErrConfig
	}
	cfg, err := s.config.Snapshot()
	if err != nil {
		return nil, err
	}
	app := objectValue(cfg["app"])
	options, ua, err := indexerSiteSettings(site.Note, app)
	if err != nil {
		return nil, err
	}
	q := builtinindexer.ResourcePageRequest{ID: def.ID, Name: site.Name, Domain: def.Domain, UA: ua, Page: page, Keyword: keyword, Limit: 100}
	if def.Parser == "MTeamSpider" {
		q.APIKey = site.APIKey
		if q.APIKey == "" {
			return nil, builtinindexer.ErrConfig
		}
	} else {
		q.Cookie = site.Cookie
		if q.Cookie == "" {
			return nil, builtinindexer.ErrConfig
		}
		if raw := text(objectValue(cfg["pt"])["site_search_result_num"]); raw != "" && raw != "0" {
			q.Limit, err = strconv.Atoi(raw)
			if err != nil || q.Limit < 1 {
				return nil, builtinindexer.ErrConfig
			}
		}
	}
	if truthy(options["proxy"]) || options["proxy"] == "Y" {
		proxy := text(objectValue(app["proxies"])["https"])
		q.Proxy = &proxy
	}
	if err := s.siteLimits.Wait(ctx, s.sites, site.ID); err != nil {
		return nil, err
	}
	started := time.Now()
	rows, err := builtinindexer.ResourcePage(ctx, def.Parser, q, s.transport)
	if recordErr := s.sites.RecordBuiltinRequest(ctx, site.Name, started, err == nil); recordErr != nil {
		slog.Warn("could not record site resource request statistics")
	}
	return rows, err
}

// External enclosure and details URLs can both carry the provider's API key.
// The existing downloads endpoint accepts the owner-bound resourceId.
func (s *nativeExternalResourceSearch) externalSitePage(ctx context.Context, id, keyword string, page int, owner string) ([]any, error) {
	items, err := s.fetchSelectedResources(ctx, keyword, []string{id}, &page)
	if err != nil {
		return nil, err
	}
	ids, err := s.resources.Put(owner, items)
	if err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(items))
	for i, item := range items {
		rows = append(rows, map[string]any{
			"resourceId": ids[i], "indexer": item.Indexer, "indexer_id": item.IndexerID,
			"title": item.Title, "size": item.Size, "seeders": item.Seeders, "peers": item.Peers,
			"description": item.Description, "freeleech": item.Freeleech, "imdbid": item.IMDbID,
			"downloadvolumefactor": item.DownloadFactor, "uploadvolumefactor": item.UploadFactor,
		})
	}
	return rows, nil
}
