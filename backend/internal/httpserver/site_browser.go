package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

// Browser sessions are private read credentials, bound to the original saved
// site and complete application settings. They never update CONFIG_SITE.
type siteBrowser struct {
	renderer  browserruntime.Renderer
	sites     *siteconfig.Store
	config    *config.Store
	limits    *siteRequestLimiter
	transport http.RoundTripper
}

func (b siteBrowser) guard(site siteconfig.Site, configuration map[string]any, extra func(context.Context) error) func(context.Context) error {
	siteHash, appHash := browserruntime.Hash(site), browserruntime.Hash(configuration)
	return func(ctx context.Context) error {
		if siteHash == "" || appHash == "" {
			return browserruntime.ErrScope
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if b.config == nil {
			return browserruntime.ErrScope
		}
		current, err := b.config.Snapshot()
		if err != nil || browserruntime.Hash(current) != appHash {
			return browserruntime.ErrScope
		}
		if site.ID > 0 {
			if b.sites == nil {
				return browserruntime.ErrScope
			}
			current, err := b.sites.Get(ctx, site.ID)
			if err != nil || browserruntime.Hash(current) != siteHash {
				return browserruntime.ErrScope
			}
		}
		if extra != nil {
			return extra(ctx)
		}
		return nil
	}
}
func (b siteBrowser) render(ctx context.Context, site siteconfig.Site, configuration map[string]any, address string, form *browserruntime.Form, ready, empty string, extra func(context.Context) error) (browserruntime.Result, *browserruntime.Session, error) {
	if b.renderer == nil || !b.renderer.Available() {
		return browserruntime.Result{}, nil, browserruntime.ErrUnavailable
	}
	var note map[string]any
	if site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
		return browserruntime.Result{}, nil, browserruntime.ErrScope
	}
	app := objectValue(configuration["app"])
	ua := text(note["ua"])
	if ua == "" {
		ua = text(app["user_agent"])
	}
	if ua == "" {
		ua = defaultSiteUserAgent
	}
	transport := b.transport
	if truthy(note["proxy"]) || note["proxy"] == "Y" {
		var err error
		u, _ := url.Parse(address)
		transport, err = siteProxyTransport(transport, objectValue(app["proxies"]), u.Scheme)
		if err != nil {
			return browserruntime.Result{}, nil, err
		}
	}
	guard := b.guard(site, configuration, extra)
	if err := guard(ctx); err != nil {
		return browserruntime.Result{}, nil, err
	}
	result, err := b.renderer.Render(ctx, browserruntime.Request{URL: address, Cookie: site.Cookie, UserAgent: ua, Form: form, ReadySelector: ready, EmptySelector: empty, ReadOnly: form == nil, Assets: true, Transport: transport, Guard: guard,
		Before: func(ctx context.Context, _ browserruntime.Exchange) (func(string, string) error, error) {
			if err := guard(ctx); err != nil {
				return nil, err
			}
			if site.ID > 0 {
				if err := b.limits.Wait(ctx, b.sites, site.ID); err != nil {
					return nil, err
				}
			}
			return func(string, string) error { return nil }, nil
		}})
	if err != nil {
		return browserruntime.Result{}, nil, err
	}
	if err = guard(ctx); err != nil {
		return browserruntime.Result{}, nil, err
	}
	session := &browserruntime.Session{SiteID: site.ID, Site: browserruntime.Hash(site), Application: browserruntime.Hash(configuration), Origin: browserruntime.Origin(address), Cookie: result.Cookie, UserAgent: result.UserAgent, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	if !session.Valid() {
		return browserruntime.Result{}, nil, browserruntime.ErrScope
	}
	return result, session, nil
}

// This wrapper lets the existing connection-test parser use the same renderer
// with a current-record guard, without introducing an arbitrary-URL endpoint.
type guardedSiteRenderer struct {
	browserruntime.Renderer
	guard func(context.Context) error
}

func (g guardedSiteRenderer) Render(ctx context.Context, r browserruntime.Request) (browserruntime.Result, error) {
	prior := r.Guard
	r.Guard = func(ctx context.Context) error {
		if err := g.guard(ctx); err != nil {
			return err
		}
		if prior != nil {
			return prior(ctx)
		}
		return nil
	}
	return g.Renderer.Render(ctx, r)
}
func (s siteService) connectionRenderer(site siteconfig.Site, configuration map[string]any) browserruntime.Renderer {
	if s.browser == nil {
		return nil
	}
	b := siteBrowser{renderer: s.browser, sites: s.store, config: s.configStore}
	return guardedSiteRenderer{Renderer: s.browser, guard: b.guard(site, configuration, nil)}
}
func (api *subscriptionRSSAPI) fetchRenderedDetail(ctx context.Context, site siteconfig.Site, address string) ([]byte, *browserruntime.Session, error) {
	if !subscriptionSiteOriginAllowed(site, address) {
		return nil, nil, browserruntime.ErrScope
	}
	s := api.runner.planner.runner.download.service
	cfg, err := s.configStore.Snapshot()
	if err != nil {
		return nil, nil, err
	}
	page, session, err := (siteBrowser{renderer: s.browser, sites: s.sites, config: s.configStore, limits: s.siteLimits, transport: s.client.Transport}).render(ctx, site, cfg, address, nil, "", "", nil)
	return page.HTML, session, err
}
