package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/downloaderconfig"
	"github.com/0xforee/nas-tools/backend/internal/iyuu"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
	"github.com/0xforee/nas-tools/backend/internal/torrenttransfer"
)

type iyuuSelection struct {
	proof       string
	names       []string
	downloaders []downloaderconfig.Downloader
	sites       []siteconfig.Site
}

func (api *iyuuAPI) selection(ctx context.Context, s iyuuScope) (iyuuSelection, error) {
	out := iyuuSelection{names: []string{}}
	ds, err := api.transfer.downloaders.List(ctx)
	if err != nil {
		return out, err
	}
	for _, d := range ds {
		if !slices.Contains(s.cfg.Downloaders, strconv.FormatInt(d.ID, 10)) {
			continue
		}
		if d.Enabled == 1 && (d.Type == "transmission" || d.Type == "qbittorrent") {
			out.downloaders = append(out.downloaders, d)
			out.names = append(out.names, d.Name)
		}
	}
	ss, err := api.transfer.login.sites.List(ctx)
	if err != nil {
		return out, err
	}
	for _, v := range ss {
		if len(s.cfg.Sites) == 0 || slices.Contains(s.cfg.Sites, strconv.FormatInt(v.ID, 10)) {
			out.sites = append(out.sites, v)
		}
	}
	out.proof = sitelogin.Hash([]any{s.actor, s.authority, s.account(), s.raw[iyuuGeneration], s.cfg.Downloaders, s.cfg.Sites, s.cfg.Excluded, out.downloaders, out.sites})
	return out, nil
}

func iyuuHost(address string) string {
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return strings.ToLower(u.Host)
}

type iyuuMatch struct {
	SourceProof  string `json:"sourceProof"`
	SourceID     string `json:"sourceId"`
	ProviderID   string `json:"providerId"`
	Downloader   string `json:"downloader"`
	DownloaderID string `json:"downloaderId"`
	Source       string `json:"source"`
	Name         string `json:"name"`
	Site         string `json:"site"`
	SiteID       string `json:"siteId"`
	TorrentID    string `json:"torrentId"`
	Hash         string `json:"hash"`
}

func (api *iyuuAPI) matches(w http.ResponseWriter, r *http.Request) {
	s, ok := api.requestScope(w, r)
	if !ok {
		return
	}
	var in struct{ Fingerprint string }
	if !siteLoginJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	sel, err := api.selection(ctx, s)
	if err != nil || sel.proof != in.Fingerprint || len(sel.downloaders) == 0 || len(sel.downloaders) != len(s.cfg.Downloaders) {
		writeAPIError(w, 409, 409, "请核对选定的下载器并刷新页面")
		return
	}
	current := func() error {
		if err := api.current(ctx, s); err != nil {
			return err
		}
		now, err := api.selection(ctx, s)
		if err != nil || now.proof != sel.proof {
			return iyuu.ErrInput
		}
		return nil
	}
	matcher := iyuuMatcher{scope: s, sites: sel.sites, current: current}
	rows := []iyuuMatch{}
	scanned := 0
	for _, d := range sel.downloaders {
		c, err := api.transfer.organizer.clientFromDownloader(d)
		if err != nil {
			writeAPIError(w, 409, 409, "选定的下载器配置无效")
			return
		}
		part, n, err := matcher.scan(ctx, d, c)
		if err != nil {
			if errors.Is(err, iyuu.ErrInput) {
				writeAPIError(w, 409, 409, "配置已变化，请刷新后重新匹配")
			} else {
				writeAPIError(w, 502, 502, "匹配未完成，请核对下载器及 IYUU 服务状态")
			}
			return
		}
		rows = append(rows, part...)
		scanned += n
	}
	if current() != nil {
		writeAPIError(w, 409, 409, "配置已变化，请重新匹配")
		return
	}
	signinJSON(w, map[string]any{"scanned": scanned, "items": rows})
}

type iyuuMatcher struct {
	scope     iyuuScope
	sites     []siteconfig.Site
	current   func() error
	signature string
	targets   map[string]siteconfig.Site
	providers map[string]iyuu.Site
}

func (m *iyuuMatcher) prepare(ctx context.Context) error {
	if m.targets != nil {
		return nil
	}
	if err := m.current(); err != nil {
		return err
	}
	sites, err := m.scope.client.Sites(ctx)
	if err != nil {
		return err
	}
	local := map[string]siteconfig.Site{}
	for _, s := range m.sites {
		address := s.SignURL
		if address == "" {
			address = s.RSSURL
		}
		h := iyuuHost(address)
		if h == "" {
			continue
		}
		if _, ambiguous := local[h]; ambiguous {
			local[h] = siteconfig.Site{}
		} else {
			local[h] = s
		}
	}
	targets := map[string]siteconfig.Site{}
	m.providers = map[string]iyuu.Site{}
	for _, s := range sites {
		if v := local[iyuuHost(s.Domain)]; v.ID != 0 {
			targets[s.ID.String()] = v
			m.providers[s.ID.String()] = s
		}
	}
	if len(targets) > 0 {
		if err := m.current(); err != nil {
			return err
		}
		m.signature, err = m.scope.client.ReportSites(ctx, sites)
		if err != nil {
			return err
		}
	}
	m.targets = targets
	return nil
}

func (m *iyuuMatcher) scan(ctx context.Context, d downloaderconfig.Downloader, c completionOrganizationClient) ([]iyuuMatch, int, error) {
	if err := m.current(); err != nil {
		return nil, 0, err
	}
	ids, err := c.transfer.CompletedIDs(ctx)
	if err != nil {
		return nil, 0, err
	}
	type sourceInfo struct{ name, wire, proof string }
	names := map[string]sourceInfo{}
	hashes := []string{}
	cfg := m.scope.cfg
	for _, id := range ids {
		if cfg.Failed[id] || cfg.Permanent[id] {
			continue
		}
		task, err := c.transfer.TransferTask(ctx, id)
		if errors.Is(err, torrenttransfer.ErrTaskAbsent) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		if !task.Valid() || task.Kind != d.Type || task.WireID != id {
			return nil, 0, iyuu.ErrResponse
		}
		if !iyuuSourceEligible(task, cfg) {
			continue
		}
		if _, ok := names[task.V1Hash]; ok {
			continue
		}
		names[task.V1Hash] = sourceInfo{task.Name, id, iyuuSourceProof(task)}
		hashes = append(hashes, task.V1Hash)
	}
	if len(hashes) == 0 {
		return nil, 0, nil
	}
	if err := m.prepare(ctx); err != nil {
		return nil, 0, err
	}
	if len(m.targets) == 0 {
		return nil, 0, nil
	}
	slices.Sort(hashes)
	rows := []iyuuMatch{}
	present := map[string]bool{}
	for chunk := range slices.Chunk(hashes, 200) {
		if err := m.current(); err != nil {
			return nil, 0, err
		}
		found, err := m.scope.client.Match(ctx, chunk, m.signature, time.Now())
		if err != nil {
			return nil, 0, err
		}
		for _, source := range chunk {
			seen := map[[3]string]bool{}
			for _, v := range found[source] {
				site, ok := m.targets[v.SiteID.String()]
				if !ok || cfg.Success[v.Hash] || cfg.Failed[v.Hash] || cfg.Permanent[v.Hash] {
					continue
				}
				if _, exists := names[v.Hash]; exists {
					continue
				}
				key := [3]string{v.SiteID.String(), v.TorrentID.String(), v.Hash}
				if seen[key] {
					continue
				}
				seen[key] = true
				exists, checked := present[v.Hash]
				if !checked {
					exists, err = c.has(ctx, v.Hash)
					if err != nil {
						return nil, 0, err
					}
					present[v.Hash] = exists
				}
				if exists {
					continue
				}
				rows = append(rows, iyuuMatch{Downloader: d.Name, DownloaderID: strconv.FormatInt(d.ID, 10), Source: source, Name: names[source].name, SourceID: names[source].wire, SourceProof: names[source].proof, ProviderID: v.SiteID.String(), Site: site.Name, SiteID: strconv.FormatInt(site.ID, 10), TorrentID: v.TorrentID.String(), Hash: v.Hash})
			}
		}
	}
	return rows, len(hashes), nil
}

func iyuuSourceProof(t torrenttransfer.Task) string {
	return sitelogin.Hash([]any{t.Kind, t.WireID, t.V1Hash, t.V2Hash, t.AddedAt, t.Directory, t.RawQbTags, t.Labels, t.Files})
}
func iyuuSourceEligible(t torrenttransfer.Task, cfg iyuu.Settings) bool {
	if !t.Valid() || !t.Complete || t.Error || t.Checking || !iyuu.Hash(t.V1Hash) || cfg.Failed[t.V1Hash] || cfg.Permanent[t.V1Hash] {
		return false
	}
	return !slices.ContainsFunc(cfg.Excluded, func(label string) bool {
		if t.Kind == "qbittorrent" {
			return strings.Contains(t.RawQbTags, label)
		}
		return slices.Contains(t.Labels, label)
	})
}
