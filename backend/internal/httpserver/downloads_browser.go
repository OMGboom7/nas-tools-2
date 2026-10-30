package httpserver

import (
	"context"
	"encoding/json"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

func (s downloadService) prepareBrowserSearchTorrent(ctx context.Context, r externalindexer.Resource) (string, []byte, error) {
	session := r.Browser
	if session == nil || !session.Valid() || s.configStore == nil {
		return "", nil, browserruntime.ErrScope
	}
	cfg, err := s.configStore.Snapshot()
	if err != nil || browserruntime.Hash(cfg) != session.Application {
		return "", nil, browserruntime.ErrScope
	}
	var original siteconfig.Site
	if session.SiteID > 0 {
		if s.sites == nil {
			return "", nil, browserruntime.ErrScope
		}
		original, err = s.sites.Get(ctx, session.SiteID)
		if err != nil {
			return "", nil, browserruntime.ErrScope
		}
	}
	if browserruntime.Hash(original) != session.Site {
		return "", nil, browserruntime.ErrScope
	}
	guard := (siteBrowser{sites: s.sites, config: s.configStore}).guard(original, cfg, nil)
	if err = guard(ctx); err != nil {
		return "", nil, err
	}
	if validMagnet(r.DownloadURL) {
		return r.DownloadURL, nil, nil
	}
	if r.DownloadResolver != "" || browserruntime.Origin(r.DownloadURL) != session.Origin {
		return "", nil, browserruntime.ErrScope
	}
	site := original
	if site.ID == 0 {
		site.RSSURL = session.Origin + "/"
	}
	var note map[string]any
	if site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
		return "", nil, browserruntime.ErrScope
	}
	if note == nil {
		note = map[string]any{}
	}
	note["ua"] = session.UserAgent
	encoded, err := json.Marshal(note)
	if err != nil {
		return "", nil, browserruntime.ErrScope
	}
	site.Cookie, site.Note = session.Cookie, string(encoded)
	contents, err := s.fetchSiteTorrentBound(ctx, site, r.DownloadURL, guard)
	if err != nil {
		return "", nil, err
	}
	if err = guard(ctx); err != nil {
		return "", nil, err
	}
	if validMagnet(string(contents)) {
		return string(contents), nil, nil
	}
	return "", contents, nil
}
