package httpserver

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/batchdownload"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/regexcompat"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"github.com/0xforee/nas-tools/backend/internal/systemconfig"
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
	target                   batchdownload.Season
	current                  int
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
	if kind == "TV" && r.fuzzy == nil && !truthy(row["over_edition"]) {
		r.target.Number = 1
		if r.season != "" {
			r.target.Number, err = strconv.Atoi(strings.TrimPrefix(r.season, "S"))
			if err != nil || r.target.Number < 0 || r.target.Number > 1000 {
				return nil, errFuzzyScope
			}
		}
		for key, dst := range map[string]*int{"total": &r.target.Total, "current_ep": &r.current} {
			if value := text(row[key]); value != "" {
				*dst, err = strconv.Atoi(value)
				if err != nil || *dst < 0 || *dst > 10000 {
					return nil, errFuzzyScope
				}
			}
		}
		if len(src.Episodes) > 1 {
			return nil, errFuzzyScope
		}
		if len(src.Episodes) == 1 {
			r.target.Episodes = []int{}
			raw := text(src.Episodes[0]["EPISODES"])
			if raw != "" {
				for _, v := range strings.Split(raw, ",") {
					n, err := strconv.Atoi(strings.TrimSpace(v))
					if err != nil || n < 1 || n > 10000 {
						return nil, errFuzzyScope
					}
					r.target.Episodes = append(r.target.Episodes, n)
				}
				slices.Sort(r.target.Episodes)
				if len(slices.Compact(r.target.Episodes)) != len(r.target.Episodes) {
					return nil, errFuzzyScope
				}
			}
		}
	}
	for _, key := range []string{"id", "season", "filter_restype", "filter_pix", "filter_team", "filter_rule", "filter_include", "filter_exclude", "save_path", "download_setting"} {
		r.fields[key] = row[key]
	}
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
			busy, err := ordinaryRSSBusy(ctx, tx, kind, rule.id, rule.token)
			if err != nil {
				return p, err
			}
			if busy {
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
	Op        string
	Index     int
	Identity  *classicRSSIdentity
	Accepted  bool
	Inventory *inventoryQuery
}
type classicRSSScan struct {
	path      string
	app       map[string]any
	out       *json.Encoder
	in        *bufio.Scanner
	index     int
	inventory subscriptionService
	accepted  []classicRSSChoice
}
type classicRSSChoice struct {
	index     int
	site      siteconfig.Site
	rule      *classicRSSRule
	enclosure string
	show      string
	gap       *batchdownload.Season
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

// Reconcile this subscription's saved progress with the library before the site
// spends its final download allowance. Missing inventory never authorizes a download.
func (s *classicRSSScan) eligible(ctx context.Context, r *classicRSSRule, id classicRSSIdentity, q *inventoryQuery) (*batchdownload.Season, bool, error) {
	if r.fuzzy != nil || r.fields["over_edition"] == true {
		return nil, true, nil
	}
	if q == nil || !numericTMDBID(q.ID) || q.ID != id.TMDBID || strings.TrimSpace(q.Title) == "" ||
		id.Title != "" && q.Title != id.Title || id.Year != "" && q.Year != id.Year ||
		q.LocalOnly || len(q.Totals) != 0 || len(q.TVDetail) != 0 ||
		!(r.kind == "MOV" && q.Kind == "电影" || r.kind == "TV" && (q.Kind == "电视剧" || q.Kind == "动漫")) {
		return nil, false, errors.New("invalid RSS library selection")
	}
	if r.target.Total > 0 {
		q.Totals = map[int]int{r.target.Number: r.target.Total}
	}
	limit, cancel := context.WithTimeout(ctx, 80*time.Second)
	defer cancel()
	inv, err := s.inventory.checkLibrarySelection(limit, *q)
	if err != nil || inv.Complete {
		return nil, false, err
	}
	if r.kind == "MOV" {
		return nil, true, nil
	}
	for _, lib := range inv.Missing {
		if lib.Season != r.target.Number {
			continue
		}
		target := r.target
		target.Total = lib.Total
		if target.Total < 1 || target.Total > 10000 || target.Episodes != nil && len(target.Episodes) == 0 {
			break
		}
		if len(target.Episodes) > 0 && target.Episodes[len(target.Episodes)-1] > target.Total {
			break
		}
		if target.Episodes == nil && r.current > 0 {
			if r.current > target.Total {
				break
			}
			for n := r.current; n <= target.Total; n++ {
				target.Episodes = append(target.Episodes, n)
			}
		}
		result := batchdownload.IntersectSeasons([]batchdownload.Season{target}, []batchdownload.Season{{Number: lib.Season, Total: lib.Total, Episodes: lib.Episodes}})
		if len(result) != 0 {
			return &result[0], true, nil
		}
		break
	}
	return nil, false, nil
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
				gap, eligible, err := s.eligible(ctx, rule, *reply.Identity, decision.Inventory)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil {
					slog.Warn("unable to verify RSS library inventory", "kind", rule.kind, "subscription", rule.id)
				}
				if !eligible {
					break
				}
				ready, err := s.exchange(map[string]any{"op": "ready", "siteID": site.ID, "scope": gap}, s.index)
				if err != nil {
					return err
				}
				if ready.Accepted {
					s.accepted = append(s.accepted, classicRSSChoice{s.index, site, rule, article.Enclosure, reply.Identity.TMDBID, gap})
				}
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

func mergeRSSGaps(previous, next batchdownload.Season) batchdownload.Season {
	if previous.Total == 0 {
		return next
	}
	if previous.Total == next.Total && slices.Equal(previous.Episodes, next.Episodes) {
		return previous
	}
	total := max(previous.Total, next.Total)
	units := make([]bool, total+1)
	for _, g := range []batchdownload.Season{previous, next} {
		if len(g.Episodes) == 0 {
			for n := 1; n <= g.Total; n++ {
				units[n] = true
			}
		} else {
			for _, n := range g.Episodes {
				units[n] = true
			}
		}
	}
	var merged []int
	for n := 1; n <= total; n++ {
		if units[n] {
			merged = append(merged, n)
		}
	}
	if len(merged) == total {
		merged = nil
	}
	return batchdownload.Season{Number: previous.Number, Total: total, Episodes: merged}
}

// All receipts for the same subscription version share one starting point.
// Selection still honors the observed gaps for each individual resource.
func (s *classicRSSScan) batchMissing(indices []int) (map[string][]batchdownload.Season, map[string]batchdownload.Season) {
	shows := map[string][]batchdownload.Season{}
	subscriptions := map[string]batchdownload.Season{}
	for _, c := range s.accepted {
		if c.gap == nil {
			continue
		}
		if _, ok := slices.BinarySearch(indices, c.index); !ok {
			continue
		}
		subscriptions[c.rule.token] = mergeRSSGaps(subscriptions[c.rule.token], *c.gap)
		seasons := shows[c.show]
		i := slices.IndexFunc(seasons, func(v batchdownload.Season) bool { return v.Number == c.gap.Number })
		if i < 0 {
			shows[c.show] = append(seasons, *c.gap)
		} else {
			seasons[i] = mergeRSSGaps(seasons[i], *c.gap)
		}
	}
	return shows, subscriptions
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
	if _, err := recoverOrdinaryRSS(ctx, db, service.databasePath); err != nil {
		return err
	}
	p, err := loadClassicRSSPlan(ctx, db)
	if err != nil {
		return err
	}
	cfg, err := config.NewStore(path).Snapshot()
	if err != nil {
		return err
	}
	system, err := systemconfig.Open(service.databasePath)
	if err != nil {
		return err
	}
	defer system.Close()
	service.systemConfig, service.configStore, service.client = system, config.NewStore(path), http.DefaultClient
	s := classicRSSScan{inventory: service, path: path, app: objectValue(cfg["app"]), in: bufio.NewScanner(in), out: json.NewEncoder(out)}
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
	missing, receipts := s.batchMissing(indices)
	if _, err = s.exchange(map[string]any{"op": "submit", "indices": indices, "missing": missing, "receipts": receipts}, 0); err != nil {
		return err
	}
	finished, err := recoverOrdinaryRSS(ctx, db, service.databasePath)
	if err != nil {
		return err
	}
	return s.out.Encode(map[string]any{"op": "done", "finished": finished})
}
