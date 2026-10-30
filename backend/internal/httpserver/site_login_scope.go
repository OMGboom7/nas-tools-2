package httpserver

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/sitelogin"
)

type siteLoginOptions struct {
	SiteIDs  []string `json:"siteIds"`
	UseSaved bool     `json:"useSaved"`
	Remember bool     `json:"remember"`
	OCR      bool     `json:"ocr"`
}
type siteLoginPlan struct {
	ID, Actor, Authority, Created, Expires string
	RememberSnapshot                       string
	Sources                                []sitelogin.Source
	Options                                siteLoginOptions
}
type siteLoginTarget struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	Path   string `json:"path"`
	Scheme string `json:"scheme"`
}
type siteLoginReview struct {
	ID           string                  `json:"id"`
	ScopeToken   string                  `json:"scopeToken"`
	Targets      []siteLoginTarget       `json:"targets"`
	OCRReceivers []sitelogin.OCRReceiver `json:"ocrReceivers"`
	Expires      string                  `json:"expires"`
}

func (api *siteLoginAPI) authority(actor string) (string, map[string]any, error) {
	if api.config == nil || api.auth == nil {
		return "", nil, sitelogin.ErrScope
	}
	v, err := api.config.Snapshot()
	if err != nil {
		return "", nil, err
	}
	app := objectValue(v["app"])
	if actor == "" || text(app["login_user"]) != actor || !api.auth.service.IsAdministrator(actor) {
		return "", nil, sitelogin.ErrScope
	}
	authority := sitelogin.Hash([]any{actor, app["login_password"], objectValue(v["security"])["api_key"]})
	if authority == "" || sitelogin.Hash(v) == "" {
		return "", nil, sitelogin.ErrScope
	}
	return authority, v, nil
}
func (api *siteLoginAPI) planKey(actor string) ([]byte, error) {
	authority, cfg, err := api.authority(actor)
	if err != nil {
		return nil, err
	}
	// The key stays in the original application configuration, not in the URL,
	// public plan or login journal. Review tokens contain no login credentials.
	key := sitelogin.Hash([]any{"site-login-review-v1", authority, objectValue(cfg["app"])["login_password"], objectValue(cfg["security"])["api_key"]})
	return hex.DecodeString(key)
}
func (api *siteLoginAPI) seal(actor string, p siteLoginPlan) (string, error) {
	key, err := api.planKey(actor)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > 128<<10 {
		return "", sitelogin.ErrScope
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, raw, []byte(actor))), nil
}
func (api *siteLoginAPI) unseal(actor, token string) (siteLoginPlan, error) {
	var p siteLoginPlan
	if len(token) > 200000 {
		return p, sitelogin.ErrScope
	}
	encrypted, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return p, sitelogin.ErrScope
	}
	key, err := api.planKey(actor)
	if err != nil {
		return p, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return p, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(encrypted) < aead.NonceSize() {
		return p, sitelogin.ErrScope
	}
	nonce := encrypted[:aead.NonceSize()]
	raw, err := aead.Open(nil, nonce, encrypted[aead.NonceSize():], []byte(actor))
	if err != nil || json.Unmarshal(raw, &p) != nil || p.Actor != actor || !sitelogin.Hex(p.ID, 64) || len(p.Sources) == 0 || len(p.Sources) > sitelogin.MaxSites {
		return p, sitelogin.ErrScope
	}
	expires, err := time.Parse(time.RFC3339Nano, p.Expires)
	if err != nil || !time.Now().Before(expires) {
		return p, sitelogin.ErrScope
	}
	for _, source := range p.Sources {
		if !source.Valid() || source.Authority != p.Authority {
			return p, sitelogin.ErrScope
		}
	}
	return p, nil
}
func (api *siteLoginAPI) review(ctx context.Context, actor string, in siteLoginOptions) (siteLoginReview, error) {
	result := siteLoginReview{Targets: []siteLoginTarget{}, OCRReceivers: []sitelogin.OCRReceiver{}}
	authority, cfg, err := api.authority(actor)
	if err != nil {
		return result, err
	}
	catalog, err := indexercatalog.Load(api.catalogPath)
	if err != nil {
		return result, err
	}
	sites, err := api.sites.List(ctx)
	if err != nil {
		return result, err
	}
	if len(in.SiteIDs) == 0 || len(in.SiteIDs) > sitelogin.MaxSites {
		return result, sitelogin.ErrScope
	}
	wanted := map[int64]bool{}
	ids := []int64{}
	for _, raw := range in.SiteIDs {
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id <= 0 || wanted[id] {
			return result, sitelogin.ErrScope
		}
		wanted[id] = true
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	in.SiteIDs = []string{}
	for _, id := range ids {
		in.SiteIDs = append(in.SiteIDs, strconv.FormatInt(id, 10))
	}
	rawSaved, err := api.store.Saved(ctx)
	if err != nil {
		return result, err
	}
	if in.UseSaved {
		credentials, e := sitelogin.ParseCredentials(rawSaved)
		if e != nil || credentials.Username == "" || credentials.Password == "" {
			return result, sitelogin.ErrScope
		}
	}
	now := time.Now().UTC()
	p := siteLoginPlan{ID: sitelogin.Random(), Actor: actor, Authority: authority, Created: now.Format(time.RFC3339Nano), Expires: now.Add(10 * time.Minute).Format(time.RFC3339Nano), Options: in}
	if in.Remember {
		p.RememberSnapshot = sitelogin.Hash(rawSaved)
	}
	if p.ID == "" {
		return result, sitelogin.ErrScope
	}
	byID := map[int64]siteconfig.Site{}
	for _, site := range sites {
		byID[site.ID] = site
	}
	for _, id := range ids {
		site, exists := byID[id]
		if !exists {
			return result, sitelogin.ErrScope
		}
		address, agent, err := siteLoginAddress(site, cfg, catalog)
		if err != nil {
			return result, err
		}
		u, _ := url.Parse(address)
		source := sitelogin.Source{SiteID: site.ID, Site: sitelogin.Hash(site), Application: sitelogin.Hash(cfg), Catalog: sitelogin.Hash(catalog), Authority: authority, LoginURL: address, UserAgent: agent, UseSaved: in.UseSaved, OCR: in.OCR}
		if in.UseSaved {
			source.Saved = sitelogin.Hash(rawSaved)
		}
		if !source.Valid() {
			return result, sitelogin.ErrScope
		}
		p.Sources = append(p.Sources, source)
		result.Targets = append(result.Targets, siteLoginTarget{ID: strconv.FormatInt(site.ID, 10), Name: site.Name, Host: u.Host, Path: u.EscapedPath(), Scheme: u.Scheme})
	}
	if in.OCR {
		ocr, err := sitelogin.OCRConfig(cfg)
		if err != nil {
			return result, err
		}
		result.OCRReceivers = ocr.Receivers()
		if len(result.OCRReceivers) == 0 {
			return result, sitelogin.ErrScope
		}
	}
	result.ScopeToken, err = api.seal(actor, p)
	result.ID, result.Expires = p.ID, p.Expires
	return result, err
}
func siteLoginAddress(site siteconfig.Site, cfg map[string]any, catalog indexercatalog.Catalog) (string, string, error) {
	raw := site.SignURL
	if raw == "" {
		raw = site.RSSURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
		return "", "", sitelogin.ErrScope
	}
	base := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/"}
	loginPath := "login.php"
	seen := false
	for domain, rules := range catalog.Conf {
		if !strings.Contains(domain, "://") {
			domain = "https://" + domain
		}
		if indexercatalog.Domain(domain) != indexercatalog.Domain(base.String()) {
			continue
		}
		if seen {
			return "", "", sitelogin.ErrScope
		}
		seen = true
		if raw, ok := rules["LOGIN"]; ok {
			if json.Unmarshal(raw, &loginPath) != nil || loginPath == "" || len(loginPath) > 4096 {
				return "", "", sitelogin.ErrScope
			}
		}
	}
	target, err := base.Parse(loginPath)
	if err != nil || browserruntime.Origin(target.String()) != browserruntime.Origin(base.String()) || target.Fragment != "" || target.User != nil {
		return "", "", sitelogin.ErrScope
	}
	var note map[string]any
	if site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
		return "", "", sitelogin.ErrScope
	}
	if _, err = siteconfig.CookieNote(site.Note, ""); err != nil {
		return "", "", sitelogin.ErrScope
	}
	if _, err = parseSiteRequestPolicy(note); err != nil {
		return "", "", err
	}
	agent := text(note["ua"])
	if agent == "" {
		agent = text(objectValue(cfg["app"])["user_agent"])
	}
	if agent == "" {
		agent = "auto"
	}
	if !sitelogin.Text(agent, 512) {
		return "", "", sitelogin.ErrScope
	}
	return target.String(), agent, nil
}

// Store transactions independently verify the original site and saved
// credentials using their own SQLite snapshot. This base guard never reads
// that same Store handle from inside an open transaction.
func (api *siteLoginAPI) baseGuard(source sitelogin.Source, actor, token string) sitelogin.Guard {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		claims, err := api.auth.service.VerifyToken(token)
		if err != nil || claims.Username != actor {
			return sitelogin.ErrScope
		}
		if _, err = api.auth.service.FindUser(ctx, actor); err != nil {
			return sitelogin.ErrScope
		}
		authority, cfg, err := api.authority(actor)
		if err != nil || authority != source.Authority || sitelogin.Hash(cfg) != source.Application {
			return sitelogin.ErrScope
		}
		catalog, err := indexercatalog.Load(api.catalogPath)
		if err != nil || sitelogin.Hash(catalog) != source.Catalog {
			return sitelogin.ErrScope
		}
		return nil
	}
}
func (api *siteLoginAPI) current(ctx context.Context, r sitelogin.Record, actor, token string) error {
	if err := api.baseGuard(r.Source, actor, token)(ctx); err != nil {
		return err
	}
	site, err := api.sites.Get(ctx, r.Source.SiteID)
	if err != nil || !sitelogin.SourceMatches(r.Source, site) {
		return sitelogin.ErrScope
	}
	if r.Source.UseSaved {
		raw, err := api.store.Saved(ctx)
		if err != nil || sitelogin.Hash(raw) != r.Source.Saved {
			return sitelogin.ErrScope
		}
	}
	current, err := api.store.Get(ctx, r.ID)
	if err != nil || !sitelogin.Active(current.State) || sitelogin.Hash(current.Source) != sitelogin.Hash(r.Source) {
		return sitelogin.ErrState
	}
	return nil
}

var errSiteLoginUnavailable = errors.New("native site login unavailable")
