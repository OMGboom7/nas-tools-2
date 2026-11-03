package httpserver

import (
	"context"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/sitesignin"
	"github.com/0xforee/nas-tools/backend/internal/sitestats"
)

func (api *siteStatisticsAPI) remoteScope(ctx context.Context) (sitesignin.Control, []siteSigninRemoteSource, error) {
	c, e := api.store.Control(ctx)
	if e != nil || !c.Enabled || !c.Remote {
		return sitesignin.Control{}, nil, sitestats.ErrState
	}
	fp, authority, _, _, e := api.snapshot(ctx, c.Actor)
	if e != nil || fp != c.Fingerprint || authority != c.Authority {
		return sitesignin.Control{}, nil, sitestats.ErrState
	}
	sources, _, e := api.signin.remoteSources(ctx)
	if e != nil {
		return sitesignin.Control{}, nil, e
	}
	return sitesignin.Control{Enabled: true, Scope: sitesignin.Scope{ID: c.Epoch, Actor: c.Actor, Authority: authority, ConfigDigest: fp, PluginDigest: "native-site-statistics", Created: c.Created, Location: c.Location, Options: sitesignin.Options{Remote: true}}}, sources, nil
}
func (api *siteStatisticsAPI) remoteCurrent(ctx context.Context, old sitesignin.Control) error {
	c, _, e := api.remoteScope(ctx)
	if e != nil || c.Scope.ID != old.Scope.ID || c.Scope.ConfigDigest != old.Scope.ConfigDigest || c.Scope.Authority != old.Scope.Authority {
		return sitestats.ErrState
	}
	return nil
}
func (api *siteStatisticsAPI) remoteLaunch(ctx context.Context, s siteSigninRemoteSource, id, user string, at time.Time) error {
	scope, sources, e := api.remoteScope(ctx)
	if e != nil {
		return e
	}
	valid := false
	for _, current := range sources {
		if current.digest == s.digest {
			valid = true
		}
	}
	if !valid || !s.allowed(user, "/sta") {
		return sitestats.ErrState
	}
	created, e := time.Parse(time.RFC3339Nano, scope.Scope.Created)
	if e != nil || at.Before(created.Truncate(time.Second)) {
		return sitestats.ErrState
	}
	c, e := api.store.Control(ctx)
	if e != nil {
		return e
	}
	if c.Epoch != scope.Scope.ID || c.Fingerprint != scope.Scope.ConfigDigest || c.Authority != scope.Scope.Authority || !c.Enabled || !c.Remote {
		return sitestats.ErrState
	}
	origin := "remote:" + sitestats.Hash([]any{s.digest, id})
	_, e = api.launch(ctx, sitestats.Job{ID: sitestats.Hash([]any{c.Actor, origin}), Origin: origin, Actor: c.Actor, Epoch: c.Epoch, Fingerprint: c.Fingerprint, Authority: c.Authority, ReadMessages: c.ReadMessages, SiteIDs: []string{}, Summary: true})
	return e
}

// The receiver remains shared with /pts. An independent statistics grant may
// maintain it when no valid sign-in receiver owns the channel.
func (api *siteStatisticsAPI) tickRemote(ctx context.Context, now time.Time) {
	c, _, e := api.signin.remoteControl(ctx)
	if e != nil || c.Scope.PluginDigest != "native-site-statistics" {
		return
	}
	guard, e := api.signin.acquire(ctx)
	if e != nil {
		return
	}
	defer guard.Close()
	_ = api.signin.pollTelegram(ctx, c, now)
}
