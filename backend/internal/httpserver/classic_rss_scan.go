package httpserver

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/dlclark/regexp2"
)

type classicRSSIdentity struct {
	Kind, Title, Year, TMDBID, Season, Revised string
}

type classicRSSRule struct {
	kind                     string
	id                       int64
	token                    string
	scope                    []string
	name, year, tmdb, season string
	fuzzy                    *regexp2.Regexp
	fields                   map[string]any
}

func newClassicRSSRule(kind string, src classicSubscriptionSource) (*classicRSSRule, error) {
	if desc := strings.TrimSpace(text(src.Row["DESC"])); strings.HasPrefix(desc, "{") && !json.Valid([]byte(desc)) {
		return nil, errFuzzyScope
	}
	row := normalizeNativeSubscriptionRow(src.Row)
	id, err := strconv.ParseInt(text(row["id"]), 10, 64)
	if err != nil || id < 1 {
		return nil, errFuzzyScope
	}
	r := &classicRSSRule{kind: kind, id: id, token: src.Token,
		name: text(row["name"]), year: text(row["year"]), tmdb: text(row["tmdbid"]), season: text(row["season"]), fields: map[string]any{}}
	if truthy(row["fuzzy_match"]) {
		if r.name == "" {
			return nil, errFuzzyScope
		}
		pattern, err := regexcompat.Compile(r.name, regexp2.IgnoreCase)
		if err != nil {
			return nil, err
		}
		r.fuzzy = pattern
	}
	var episodes []int
	if len(src.Episodes) > 1 {
		return nil, errFuzzyScope
	}
	if len(src.Episodes) == 1 {
		episodes = []int{}
		for _, v := range strings.Split(text(src.Episodes[0]["EPISODES"]), ",") {
			if v == "" {
				continue
			}
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 {
				return nil, errFuzzyScope
			}
			episodes = append(episodes, n)
		}
	}
	for _, key := range []string{"id", "season", "total", "current_ep", "filter_restype", "filter_pix", "filter_team", "filter_rule", "filter_include", "filter_exclude", "save_path", "download_setting"} {
		r.fields[key] = row[key]
	}
	r.fields["episodes"] = episodes
	r.fields["progress"] = map[string]any{"kind": kind, "id": r.id, "token": r.token}
	rank := int64(0)
	if value := text(row["filter_order"]); value != "" {
		rank, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, errFuzzyScope
		}
	}
	r.fields["rank"] = rank
	r.fields["over_edition"] = truthy(row["over_edition"])
	r.fields["fuzzy_match"] = r.fuzzy != nil
	return r, nil
}

func (r *classicRSSRule) matches(i classicRSSIdentity) (bool, error) {
	if i.Kind != r.kind || r.kind == "TV" && r.season != "" && r.season != i.Season && !(r.fuzzy != nil && r.season == "S00") {
		return false, nil
	}
	if r.fuzzy != nil {
		if r.year != "" && r.year != i.Year {
			return false, nil
		}
		return fuzzySubscriptionTitle(r.fuzzy, r.name, i.Revised+" "+i.Title+" "+i.Year)
	}
	if r.tmdb != "" && !strings.HasPrefix(r.tmdb, "DB:") {
		return r.tmdb == i.TMDBID, nil
	}
	if r.name == "" || r.name != i.Title {
		return false, nil
	}
	if r.year == "" || r.year == i.Year {
		return true, nil
	}
	if r.kind == "MOV" {
		a, ae := strconv.Atoi(r.year)
		b, be := strconv.Atoi(i.Year)
		return ae == nil && be == nil && (a-1 == b || a+1 == b), nil
	}
	return false, nil
}

type classicRSSPlan struct {
	sites  []siteconfig.Site
	bySite map[int64][]*classicRSSRule
}

func loadClassicRSSPlan(ctx context.Context, db *sql.DB) (classicRSSPlan, error) {
	p := classicRSSPlan{bySite: map[int64][]*classicRSSRule{}}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	p.sites, err = siteconfig.ListIn(ctx, tx)
	if err != nil {
		return p, err
	}
	for _, kind := range []string{"MOV", "TV"} {
		rows, err := listClassicSubscriptions(ctx, tx, classicSubscriptionRequest{Kind: kind, State: "R"})
		if err != nil {
			return p, err
		}
		for _, src := range rows {
			scope, err := subscriptionRSSScope(src.Row)
			if err != nil {
				slog.Warn("invalid RSS subscription", "kind", kind, "id", src.Row["ID"])
				continue
			}
			sites, err := resolveSubscriptionRSSSites(p.sites, scope, true)
			if err != nil {
				slog.Warn("invalid RSS subscription", "kind", kind, "id", src.Row["ID"])
				continue
			}
			rule, err := newClassicRSSRule(kind, src)
			if err != nil {
				slog.Warn("invalid RSS subscription", "kind", kind, "id", src.Row["ID"])
				continue
			}
			rule.scope = scope
			for _, site := range sites {
				p.bySite[site.ID] = append(p.bySite[site.ID], rule)
			}
		}
	}
	return p, tx.Commit()
}

// Each response belongs to the exact article and phase that requested it.
type classicRSSReply struct {
	Op       string
	Index    int
	Identity *classicRSSIdentity
	Accepted bool
}
type classicRSSScan struct {
	path     string
	app      map[string]any
	out      *json.Encoder
	in       *bufio.Scanner
	index    int
	accepted []classicRSSChoice
}
type classicRSSChoice struct {
	index     int
	site      siteconfig.Site
	rule      *classicRSSRule
	enclosure string
}

func (s *classicRSSScan) exchange(event map[string]any, index int) (classicRSSReply, error) {
	var reply classicRSSReply
	event["index"] = index
	if err := s.out.Encode(event); err != nil {
		return reply, err
	}
	if !s.in.Scan() {
		if err := s.in.Err(); err != nil {
			return reply, err
		}
		return reply, io.ErrUnexpectedEOF
	}
	if err := decodeFuzzyDocument(s.in.Text(), &reply, 256<<10); err != nil {
		return reply, err
	}
	if reply.Op != event["op"] || reply.Index != index {
		return reply, errors.New("unexpected RSS scan response")
	}
	return reply, nil
}

func (s *classicRSSScan) scanSite(ctx context.Context, site siteconfig.Site, rules []*classicRSSRule) error {
	note, _, order, err := subscriptionRSSSiteNote(site)
	if err != nil {
		return s.out.Encode(map[string]any{"op": "failed", "site": site.Name})
	}
	proxy := note["proxy"] == "Y"
	items, err := fetchSiteFeed(ctx, site.RSSURL, proxy, s.app)
	if err != nil {
		op := "failed"
		if errors.Is(err, errExpiredSiteFeed) {
			op = "expired"
		}
		return s.out.Encode(map[string]any{"op": op, "site": site.Name})
	}
	items, err = filterRecordedArticles(ctx, s.path, items)
	if err != nil {
		return err
	}
	for _, article := range items {
		s.index++
		reply, err := s.exchange(map[string]any{"op": "identify", "site": site.Name, "order": order, "article": article}, s.index)
		if err != nil {
			return err
		}
		if reply.Identity == nil {
			continue
		}
		for _, rule := range rules {
			match, err := rule.matches(*reply.Identity)
			if err != nil {
				break
			}
			if !match {
				continue
			}
			parse := note["parse"] == "Y"
			details := map[string]any{"id": site.ID, "rule": note["rule"], "parse": parse}
			if parse {
				if article.Link == nil || !subscriptionSiteOriginAllowed(site, *article.Link) {
					break
				}
				details["ua"], details["cookie"], details["proxy"] = note["ua"], site.Cookie, proxy
			}
			decision, err := s.exchange(map[string]any{"op": "candidate", "source": rule.fields, "site": details}, s.index)
			if err != nil {
				return err
			}
			if decision.Accepted {
				s.accepted = append(s.accepted, classicRSSChoice{s.index, site, rule, article.Enclosure})
			}
			break
		}
	}
	return nil
}

func (s *classicRSSScan) validSelections(ctx context.Context, db *sql.DB) ([]int, error) {
	indices := []int{}
	if len(s.accepted) == 0 {
		return indices, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	sites, err := siteconfig.ListIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	current := map[int64]siteconfig.Site{}
	for _, site := range sites {
		current[site.ID] = site
	}
	allowed := map[*classicRSSRule]map[int64]bool{}
	for _, choice := range s.accepted {
		r := choice.rule
		if current[choice.site.ID] != choice.site {
			continue
		}
		siteIDs, ok := allowed[r]
		if !ok {
			src, err := captureClassicSubscription(ctx, tx, r.kind, r.id)
			if err != nil {
				return nil, err
			}
			pending, err := readPendingSubscriptionSubmission(ctx, tx, r.kind, r.id)
			if err != nil {
				return nil, err
			}
			siteIDs = map[int64]bool{}
			if !pending && r.token == src.Token {
				resolved, err := resolveSubscriptionRSSSites(sites, r.scope, true)
				if err != nil {
					return nil, err
				}
				for _, site := range resolved {
					siteIDs[site.ID] = true
				}
			}
			allowed[r] = siteIDs
		}
		if !siteIDs[choice.site.ID] {
			continue
		}
		processed, err := subscriptionRSSProcessed(ctx, tx, choice.enclosure)
		if err != nil {
			return nil, err
		}
		if !processed {
			indices = append(indices, choice.index)
		}
	}
	return indices, tx.Commit()
}

func holdRSSScan(ctx context.Context, path string) (func(), error) {
	return holdVolumeReceiver(ctx, path, "subscription-rss-scan", ".nastool-subscription-rss-scan.lock")
}

func ClassicRSSScanCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	release, err := backuprestore.HoldStartup(path)
	if err != nil {
		return err
	}
	defer release()
	service := subscriptionService{databasePath: (config.Application{Path: path}).UserDatabasePath()}
	unlock, err := holdRSSScan(ctx, service.databasePath)
	if err != nil {
		return err
	}
	defer unlock()
	db, err := service.openNativeSubscriptionWriteDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	p, err := loadClassicRSSPlan(ctx, db)
	if err != nil {
		return err
	}
	cfg, err := config.NewStore(path).Snapshot()
	if err != nil {
		return err
	}
	s := classicRSSScan{path: path, app: objectValue(cfg["app"]), in: bufio.NewScanner(in), out: json.NewEncoder(out)}
	s.in.Buffer(make([]byte, 4096), 256<<10)
	for _, site := range p.sites {
		if rules := p.bySite[site.ID]; len(rules) > 0 {
			if err := s.scanSite(ctx, site, rules); err != nil {
				return err
			}
		}
	}
	indices, err := s.validSelections(ctx, db)
	if err != nil {
		return err
	}
	if _, err = s.exchange(map[string]any{"op": "submit", "indices": indices}, 0); err != nil {
		return err
	}
	return s.out.Encode(map[string]string{"op": "done"})
}
