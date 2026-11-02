package httpserver

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
)

type siteSigninTarget struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Scheme  string `json:"scheme"`
	Host    string `json:"host"`
	Path    string `json:"path"`
	Adapter string `json:"adapter"`
	Browser bool   `json:"browser"`
}
type siteSigninReviewView struct {
	ID          string                 `json:"id"`
	Fingerprint string                 `json:"fingerprint"`
	Mode        string                 `json:"mode"`
	Expires     string                 `json:"expires"`
	Targets     []siteSigninTarget     `json:"targets"`
	Helpers     []sitesignin.Receiver  `json:"helpers"`
	Settings    sitesignin.Settings    `json:"settings"`
	Location    string                 `json:"location"`
	Remote      []siteSigninRemoteView `json:"remote"`
}

func signinAddress(site siteconfig.Site, cfg map[string]any) (sitesignin.Source, error) {
	address := site.SignURL
	if address == "" {
		address = browserruntime.Origin(site.RSSURL) + "/"
	}
	if browserruntime.Origin(address) == "" || len(address) > 16000 {
		return sitesignin.Source{}, sitesignin.ErrScope
	}
	var note map[string]json.RawMessage
	if site.Note != "" && site.Note != "null" {
		if json.Unmarshal([]byte(site.Note), &note) != nil {
			return sitesignin.Source{}, sitesignin.ErrScope
		}
	}
	ua := ""
	if len(note["ua"]) > 0 && string(note["ua"]) != "null" {
		if json.Unmarshal(note["ua"], &ua) != nil {
			return sitesignin.Source{}, sitesignin.ErrScope
		}
	}
	if ua == "" {
		ua = text(objectValue(cfg["app"])["user_agent"])
	}
	if ua == "" {
		ua = defaultSiteUserAgent
	}
	chrome := false
	var marker string
	_ = json.Unmarshal(note["chrome"], &marker)
	if marker == "Y" {
		chrome = true
	} else {
		_ = json.Unmarshal(note["chrome"], &chrome)
	}
	return sitesignin.Source{SiteID: site.ID, Name: site.Name, URL: address, UserAgent: ua, SiteDigest: sitelogin.Hash(site), Browser: chrome}, nil
}
func signinPublicTarget(source sitesignin.Source) siteSigninTarget {
	u, _ := url.Parse(source.URL)
	return siteSigninTarget{strconv.FormatInt(source.SiteID, 10), source.Name, u.Scheme, u.Host, u.EscapedPath(), sitesignin.Adapter(source.URL), source.Browser}
}
func (api *siteSigninAPI) pluginSnapshot(ctx context.Context) (string, string, sitesignin.Settings, bool, error) {
	raw, e := api.system.ReadMany(ctx, []string{"UserInstalledPlugins", "plugin." + sitesignin.Plugin})
	if e != nil {
		return "", "", sitesignin.Settings{}, false, e
	}
	on, e := indexerInstalled(raw["UserInstalledPlugins"], sitesignin.Plugin)
	if e != nil {
		return "", "", sitesignin.Settings{}, false, e
	}
	settings, e := sitesignin.ParseSettings(raw["plugin."+sitesignin.Plugin])
	return raw["plugin."+sitesignin.Plugin], raw["UserInstalledPlugins"], settings, on, e
}
func (api *siteSigninAPI) fingerprint(ctx context.Context, actor string) (string, error) {
	authority, _, e := api.login.authority(actor)
	if e != nil {
		return "", e
	}
	raw, installed, _, _, e := api.pluginSnapshot(ctx)
	if e != nil {
		return "", e
	}
	return sitelogin.Hash([]any{"native-signin-settings-v1", authority, raw, installed}), nil
}

// This guard only reads external stores. The sign-in store checks its own
// control and site rows within its transaction, avoiding a single-pool deadlock.
func (api *siteSigninAPI) base(scope sitesignin.Scope, token, pluginDigest string) func(context.Context) error {
	return func(ctx context.Context) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if token != "" {
			claims, e := api.login.auth.service.VerifyToken(token)
			if e != nil || claims.Username != scope.Actor {
				return sitesignin.ErrScope
			}
		}
		if _, e := api.login.auth.service.FindUser(ctx, scope.Actor); e != nil {
			return sitesignin.ErrScope
		}
		authority, cfg, e := api.login.authority(scope.Actor)
		if e != nil || authority != scope.Authority || sitelogin.Hash(cfg) != scope.ConfigDigest {
			return sitesignin.ErrScope
		}
		raw, _, settings, installed, e := api.pluginSnapshot(ctx)
		if e != nil || !installed || sitelogin.Hash(raw) != pluginDigest {
			return sitesignin.ErrScope
		}
		original := scope.Settings
		original.Once = false
		original.Clean = false
		settings.Once = false
		settings.Clean = false
		if sitelogin.Hash(original) != sitelogin.Hash(settings) {
			return sitesignin.ErrScope
		}
		if scope.Options.SavedLogin {
			raw, e := api.login.store.Saved(ctx)
			if e != nil || sitelogin.Hash(raw) != scope.SavedDigest {
				return sitesignin.ErrScope
			}
		}
		if scope.Options.Remote {
			_, digest, e := api.remoteSources(ctx)
			if e != nil || digest != scope.CommandDigest {
				return sitesignin.ErrScope
			}
		}
		return nil
	}
}
func (api *siteSigninAPI) current(ctx context.Context, b sitesignin.Batch, r sitesignin.Job, token string) error {
	if e := api.base(b.Scope, token, b.Scope.PluginDigest)(ctx); e != nil {
		return e
	}
	site, e := api.login.sites.Get(ctx, r.Source.SiteID)
	if e != nil || sitelogin.Hash(site) != r.Source.SiteDigest {
		return sitesignin.ErrScope
	}
	cfg, e := api.login.config.Snapshot()
	if e != nil {
		return e
	}
	derived, e := signinAddress(site, cfg)
	if e != nil || sitelogin.Hash(derived) != sitelogin.Hash(r.Source) {
		return sitesignin.ErrScope
	}
	actual, e := api.store.Get(ctx, r.ID)
	if e != nil || !sitesignin.Active(actual.State) || sitelogin.Hash(actual.Source) != sitelogin.Hash(r.Source) {
		return sitesignin.ErrState
	}
	if b.Trigger != "manual" {
		control, e := api.store.Control(ctx)
		if e != nil || !control.Enabled || sitelogin.Hash(control.Scope) != sitelogin.Hash(b.Scope) {
			return sitesignin.ErrScope
		}
	}
	return nil
}
func (api *siteSigninAPI) review(ctx context.Context, actor, mode string, ids []string, options sitesignin.Options) (siteSigninReviewView, error) {
	out := siteSigninReviewView{Targets: []siteSigninTarget{}, Helpers: []sitesignin.Receiver{}, Remote: []siteSigninRemoteView{}}
	if mode != "manual" && mode != "activate" {
		return out, sitesignin.ErrScope
	}
	authority, cfg, e := api.login.authority(actor)
	if e != nil {
		return out, e
	}
	raw, _, settings, installed, e := api.pluginSnapshot(ctx)
	if e != nil || !installed {
		return out, sitesignin.ErrScope
	}
	if mode == "activate" {
		ids = append(append([]string{}, settings.Sites...), settings.Special...)
		if !settings.Enabled && !settings.Once && !options.Remote {
			return out, sitesignin.ErrScope
		}
	}
	wanted := map[int64]bool{}
	for _, str := range ids {
		id, e := strconv.ParseInt(str, 10, 64)
		if e != nil || id <= 0 {
			return out, sitesignin.ErrScope
		}
		if wanted[id] && mode == "manual" {
			return out, sitesignin.ErrScope
		}
		wanted[id] = true
	}
	if len(wanted) == 0 || len(wanted) > sitesignin.MaxSites {
		return out, sitesignin.ErrScope
	}
	all, e := api.login.sites.List(ctx)
	if e != nil {
		return out, e
	}
	byID := map[int64]siteconfig.Site{}
	for _, site := range all {
		byID[site.ID] = site
	}
	ordered := []int64{}
	for id := range wanted {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	location, e := rssWorkerLocation()
	if e != nil {
		return out, e
	}
	stamp := time.Now().UTC()
	scope := sitesignin.Scope{ID: sitelogin.Random(), Actor: actor, Authority: authority, ConfigDigest: sitelogin.Hash(cfg), PluginDigest: sitelogin.Hash(raw), Created: stamp.Format(time.RFC3339Nano), Location: location.String(), Options: options, Notify: settings.Notify, Settings: settings}
	if mode == "activate" {
		after, e := sitesignin.MergeSettings(raw, map[string]json.RawMessage{"onlyonce": json.RawMessage("false"), "clean": json.RawMessage("false")})
		if e != nil {
			return out, e
		}
		scope.PluginDigest = sitelogin.Hash(after)
		if _, _, e = signinNext(settings, location, stamp); e != nil {
			return out, e
		}
	}
	for _, id := range ordered {
		site, found := byID[id]
		if !found {
			return out, sitesignin.ErrScope
		}
		source, e := signinAddress(site, cfg)
		if e != nil {
			return out, e
		}
		scope.Sources = append(scope.Sources, source)
		out.Targets = append(out.Targets, signinPublicTarget(source))
	}
	if options.SavedLogin {
		saved, e := api.login.store.Saved(ctx)
		if e != nil {
			return out, e
		}
		credentials, e := sitelogin.ParseCredentials(saved)
		if e != nil || credentials.Username == "" || credentials.Password == "" {
			return out, sitesignin.ErrScope
		}
		scope.SavedDigest = sitelogin.Hash(saved)
	}
	if options.Remote {
		remote, digest, e := api.remoteSources(ctx)
		if e != nil || len(remote) == 0 {
			return out, sitesignin.ErrScope
		}
		scope.CommandDigest = digest
		for _, source := range remote {
			out.Remote = append(out.Remote, source.public())
		}
	}
	out.Helpers, e = sitesignin.Helpers(cfg, options)
	if e != nil {
		return out, e
	}
	previous, e := api.store.Control(ctx)
	if e != nil {
		return out, e
	}
	plan := sitesignin.Review{ID: scope.ID, Mode: mode, Expires: stamp.Add(10 * time.Minute).Format(time.RFC3339Nano), BeforePluginDigest: sitelogin.Hash(raw), PreviousControlDigest: sitelogin.Hash(previous), Scope: scope}
	if e = api.store.SaveReview(ctx, plan, api.base(scope, "", plan.BeforePluginDigest)); e != nil {
		return out, e
	}
	out.ID = plan.ID
	out.Fingerprint = sitelogin.Hash(plan)
	out.Expires = plan.Expires
	out.Mode = mode
	out.Location = scope.Location
	out.Settings = settings
	return out, nil
}
func (api *siteSigninAPI) checkedReview(ctx context.Context, actor, id, fingerprint string) (sitesignin.Review, error) {
	p, e := api.store.Review(ctx, id)
	if e != nil || p.Scope.Actor != actor || sitelogin.Hash(p) != fingerprint {
		return p, sitesignin.ErrScope
	}
	end, e := time.Parse(time.RFC3339Nano, p.Expires)
	if e != nil || !time.Now().Before(end) {
		return p, sitesignin.ErrScope
	}
	if e = api.base(p.Scope, "", p.BeforePluginDigest)(ctx); e != nil {
		return p, e
	}
	if e = api.store.ReviewCurrent(ctx, p.Scope); e != nil {
		return p, e
	}
	for _, source := range p.Scope.Sources {
		site, e := api.login.sites.Get(ctx, source.SiteID)
		if e != nil || sitelogin.Hash(site) != source.SiteDigest {
			return p, sitesignin.ErrScope
		}
	}
	return p, nil
}
func signinScopeEqualIDs(a, b []string) bool {
	aa := append([]string{}, a...)
	bb := append([]string{}, b...)
	sort.Strings(aa)
	sort.Strings(bb)
	return strings.Join(aa, ",") == strings.Join(bb, ",")
}
