package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/builtinindexer"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func catalogPageLimit(cfg map[string]any) (int, error) {
	raw := text(objectValue(cfg["pt"])["site_search_result_num"])
	if raw == "" || raw == "0" {
		return 100, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 2000 {
		return 0, builtinindexer.ErrConfig
	}
	return limit, nil
}

func catalogEmptySelector(options map[string]any) (string, error) {
	value, exists := options["go_indexer_empty_selector"]
	if !exists {
		return "", nil
	}
	selector, ok := value.(string)
	if !ok {
		return "", builtinindexer.ErrConfig
	}
	return selector, nil
}

func shouldRenderCatalog(plan builtinindexer.Plan, options map[string]any) bool {
	declared := string(plan.Definition.Render)
	return plan.Definition.Parser == "RenderSpider" || declared == "true" || declared == `"Y"` || plan.Form != nil || options["chrome"] == "Y" || truthy(options["chrome"])
}

func (b siteBrowser) renderCatalog(ctx context.Context, site siteconfig.Site, configuration map[string]any, plan builtinindexer.Plan, empty, path string, catalog indexercatalog.Catalog) (browserruntime.Result, *browserruntime.Session, error) {
	if site.Cookie != "" {
		origin := site.SignURL
		if origin == "" {
			origin = site.RSSURL
		}
		if browserruntime.Origin(origin) != browserruntime.Origin(plan.URL.String()) {
			return browserruntime.Result{}, nil, browserruntime.ErrScope
		}
	}
	var rules struct{ List struct{ Selector string } }
	if json.Unmarshal(plan.Definition.Torrents, &rules) != nil || rules.List.Selector == "" {
		return browserruntime.Result{}, nil, builtinindexer.ErrConfig
	}
	fingerprint := browserruntime.Hash(catalog)
	guard := func(context.Context) error {
		current, err := indexercatalog.Load(path)
		if err != nil || browserruntime.Hash(current) != fingerprint {
			return browserruntime.ErrScope
		}
		return nil
	}
	return b.render(ctx, site, configuration, plan.URL.String(), plan.Form, rules.List.Selector, empty, guard)
}

// RenderCatalogCommand is local to the scheduler. Only the installed catalog
// supplies navigation and form definitions. Session updates go to this caller's
// in-memory indexer, as in the original client.
func RenderCatalogCommand(ctx context.Context, path, catalogPath string, in io.Reader, out io.Writer) error {
	var q struct {
		ID, Domain, Keyword, Kind string
		SiteID                    int64
		Page                      int
		Terms                     []string
	}
	input := &io.LimitedReader{R: in, N: 16 << 10}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || input.N == 0 || q.ID == "" || q.SiteID < 0 {
		return builtinindexer.ErrConfig
	}
	catalog, err := indexercatalog.Load(catalogPath)
	if err != nil {
		return err
	}
	var selected *indexercatalog.Definition
	for i := range catalog.Indexers {
		d := &catalog.Indexers[i]
		if d.ID == q.ID && d.Domain == q.Domain {
			if selected != nil {
				return builtinindexer.ErrConfig
			}
			selected = d
		}
	}
	if selected == nil {
		return builtinindexer.ErrConfig
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	store, err := siteconfig.Open((config.Application{Path: path}).UserDatabasePath())
	if err != nil {
		return err
	}
	defer store.Close()
	app := config.NewStore(path)
	cfg, err := app.Snapshot()
	if err != nil {
		return err
	}
	var site siteconfig.Site
	if q.SiteID > 0 {
		site, err = store.Get(ctx, q.SiteID)
		if err != nil {
			return err
		}
		location := site.SignURL
		if location == "" {
			location = site.RSSURL
		}
		if browserruntime.Origin(location) != browserruntime.Origin(selected.Domain) {
			return browserruntime.ErrScope
		}
	} else if !selected.Public || !truthy(objectValue(cfg["laboratory"])["show_more_sites"]) {
		return browserruntime.ErrScope
	}
	options, ua, err := indexerSiteSettings(site.Note, objectValue(cfg["app"]))
	if err != nil {
		return err
	}
	limit, err := catalogPageLimit(cfg)
	if err != nil {
		return err
	}
	request := builtinindexer.CatalogPageRequest{Definition: *selected, Keyword: q.Keyword, Kind: q.Kind, Page: q.Page, Terms: q.Terms, Limit: limit}
	plan, err := builtinindexer.CatalogPlan(request)
	if err != nil {
		return err
	}
	limits := &siteBatchLimiter{}
	limitedRows := func() error { return json.NewEncoder(out).Encode(map[string]any{"rows": []any{}, "limited": true}) }
	if !shouldRenderCatalog(plan, options) {
		// A catalog with several media-specific paths can mix ordinary GET
		// searches with browser forms. Fetch only the path selected by Go.
		request.Cookie, request.UA = site.Cookie, ua
		if truthy(options["proxy"]) || options["proxy"] == "Y" {
			value := text(objectValue(objectValue(cfg["app"])["proxies"])[plan.URL.Scheme])
			request.Proxy = &value
		}
		if site.ID > 0 {
			if err := limits.Wait(ctx, store, site.ID); errors.Is(err, errSiteRequestBusy) {
				return limitedRows()
			} else if err != nil {
				return err
			}
		}
		rows, err := builtinindexer.CatalogPage(ctx, request, nil)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{"rows": rows})
	}
	empty, err := catalogEmptySelector(options)
	if err != nil {
		return err
	}
	binary := os.Getenv("NASTOOL_BROWSER_BINARY")
	if binary == "" {
		binary = "auto"
	}
	browser := siteBrowser{browserruntime.New(binary), store, app, limits, http.DefaultTransport}
	rendered, session, err := browser.renderCatalog(ctx, site, cfg, plan, empty, catalogPath, catalog)
	if errors.Is(limits.firstErr, errSiteRequestBusy) {
		return limitedRows()
	}
	if err != nil {
		return err
	}
	html := string(rendered.HTML)
	request.HTML = &html
	rows, err := builtinindexer.CatalogPage(ctx, request, nil)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"rows": rows, "cookie": session.Cookie, "ua": session.UserAgent})
}
