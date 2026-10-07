package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/externalindexer"
	"github.com/0xforee/nas-tools/backend/internal/indexercatalog"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
)

type subscriptionRSSAPI struct {
	runner  *subscriptionSearchRunner
	mu      sync.Mutex
	running bool
}
type subscriptionRSSResult struct {
	Subscriptions int                      `json:"subscriptions"`
	Submitted     int                      `json:"submitted"`
	Completed     int                      `json:"completed"`
	Failed        int                      `json:"failed"`
	Failures      []subscriptionRSSFailure `json:"failures,omitempty"`
}

type subscriptionRSSFailure struct {
	Type   string `json:"type"`
	ID     int64  `json:"id"`
	Status int    `json:"status"`
}

func (api *subscriptionRSSAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if api.runner == nil || api.runner.planner.runner.download.service.auth == nil {
		writeAPIError(w, 503, 503, "native subscription RSS is unavailable")
		return
	}
	auth := api.runner.planner.runner.download.service.auth
	claims, err := auth.service.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeAPIError(w, 401, 401, "authorization token is invalid or expired")
		return
	}
	if !auth.service.IsAdministrator(claims.Username) {
		writeAPIError(w, 403, 403, "administrator permission is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if r.ParseForm() != nil {
		writeAPIError(w, 400, 400, "invalid subscription RSS request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	result, failure := api.execute(ctx)
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"code": failure.status, "success": false, "message": failure.message, "data": result})
		return
	}
	code := 0
	if result.Failed > 0 {
		code = 1
	}
	writeJSON(w, 200, map[string]any{"code": code, "success": code == 0, "data": result})
}

func (api *subscriptionRSSAPI) execute(ctx context.Context) (subscriptionRSSResult, *recognitionFailure) {
	result := subscriptionRSSResult{}
	fail := func(status int, message string) (subscriptionRSSResult, *recognitionFailure) {
		return result, &recognitionFailure{status, message}
	}
	if !api.runner.planner.runner.pureGo {
		return fail(501, "native subscription RSS requires disabled legacy backend")
	}
	api.mu.Lock()
	if api.running {
		api.mu.Unlock()
		return fail(409, "subscription RSS scan is already running")
	}
	api.running = true
	api.mu.Unlock()
	defer func() { api.mu.Lock(); api.running = false; api.mu.Unlock() }()
	jobs, err := api.runner.selectScheduledSubscriptions(ctx, []string{"R"}, true)
	if err != nil {
		return fail(503, "subscription RSS queue is unavailable")
	}
	result.Subscriptions = len(jobs)
	if len(jobs) == 0 {
		return result, nil
	}
	sites, err := api.runner.planner.runner.download.sites.List(ctx)
	if err != nil {
		return fail(503, "subscription RSS sites are unavailable")
	}
	if len(sites) > 100 {
		return fail(422, "subscription RSS site limit exceeded")
	}
	batch := subscriptionRSSBatch{api: api, sites: sites, feeds: map[int64]*subscriptionFeedCache{}}
	var executions sync.WaitGroup
	capacity := make(chan struct{}, 4)
	var resultMu sync.Mutex
	for _, job := range jobs {
		select {
		case <-ctx.Done():
			executions.Wait()
			return fail(504, "subscription RSS scan was cancelled or timed out")
		case capacity <- struct{}{}:
		}
		if ctx.Err() != nil {
			<-capacity
			executions.Wait()
			return fail(504, "subscription RSS scan was cancelled or timed out")
		}
		executions.Add(1)
		go func(job scheduledSubscription) {
			defer executions.Done()
			defer func() { <-capacity }()
			item, failure := api.runner.executeResources(ctx, job.kind, job.id, "R", batch.resources)
			resultMu.Lock()
			defer resultMu.Unlock()
			result.Submitted += item.Submitted
			if item.Completed {
				result.Completed++
			}
			if failure != nil {
				result.Failed++
				result.Failures = append(result.Failures, subscriptionRSSFailure{job.kind, job.id, failure.status})
			}
		}(job)
	}
	executions.Wait()
	if ctx.Err() != nil {
		return fail(504, "subscription RSS scan was cancelled or timed out")
	}
	return result, nil
}

type subscriptionFeedCache struct {
	once      sync.Once
	resources []externalindexer.Resource
	err       error
}
type subscriptionRSSBatch struct {
	api         *subscriptionRSSAPI
	sites       []siteconfig.Site
	mu          sync.Mutex
	feeds       map[int64]*subscriptionFeedCache
	catalogOnce sync.Once
	catalog     indexercatalog.Catalog
	catalogErr  error
}

func subscriptionRSSScope(raw map[string]any) ([]string, error) {
	encoded := json.RawMessage(text(raw["RSS_SITES"]))
	var old map[string]any
	if json.Unmarshal([]byte(text(raw["DESC"])), &old) == nil && old != nil {
		if value, exists := old["rss_sites"]; exists {
			var err error
			encoded, err = json.Marshal(value)
			if err != nil {
				return nil, err
			}
		}
	}
	return rssSubscriptionSiteList(encoded)
}

func selectedSubscriptionRSSSites(sites []siteconfig.Site, scope []string) ([]siteconfig.Site, error) {
	enabled := []siteconfig.Site{}
	for _, site := range sites {
		if strings.Contains(site.Include, "D") && strings.TrimSpace(site.RSSURL) != "" {
			enabled = append(enabled, site)
		}
	}
	if len(scope) == 0 {
		return enabled, nil
	}
	selected := []siteconfig.Site{}
	seen := map[int64]bool{}
	for _, value := range scope {
		matches := []siteconfig.Site{}
		for _, site := range enabled {
			if site.Name == value || strconv.FormatInt(site.ID, 10) == value {
				matches = append(matches, site)
			}
		}
		if len(matches) != 1 {
			return nil, errors.New("RSS site scope is unavailable or ambiguous")
		}
		if !seen[matches[0].ID] {
			selected = append(selected, matches[0])
			seen[matches[0].ID] = true
		}
	}
	return selected, nil
}

func subscriptionRSSSiteNote(site siteconfig.Site) (map[string]any, *int64, int, error) {
	note := map[string]any{}
	if len(site.Note) > 64<<10 || site.Note != "" && site.Note != "null" && json.Unmarshal([]byte(site.Note), &note) != nil {
		return nil, nil, 0, errors.New("invalid RSS site configuration")
	}
	if note == nil {
		note = map[string]any{}
	}
	for _, key := range []string{"parse", "proxy", "chrome"} {
		raw := strings.ToLower(strings.TrimSpace(text(note[key])))
		switch raw {
		case "", "n", "y", "true", "false", "1", "0":
		default:
			return nil, nil, 0, errors.New("invalid RSS site flag")
		}
		if raw == "y" || raw == "true" || raw == "1" {
			note[key] = "Y"
		} else {
			note[key] = "N"
		}
	}
	if text(note["chrome"]) == "Y" || truthy(note["chrome"]) {
		return nil, nil, 0, errSubscriptionRSSUnsupported
	}
	if _, err := parseSiteRequestPolicy(note); err != nil {
		return nil, nil, 0, err
	}
	var group *int64
	if raw := text(note["rule"]); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < -1 {
			return nil, nil, 0, errors.New("invalid RSS site filter")
		}
		group = &n
	}
	order := 0
	if site.Priority != "" {
		n, err := strconv.Atoi(site.Priority)
		if err != nil || n < -100000 || n > 100000 {
			return nil, nil, 0, errors.New("invalid RSS site priority")
		}
		order = 100 - n
	}
	return note, group, order, nil
}

func (batch *subscriptionRSSBatch) feed(ctx context.Context, site siteconfig.Site, note map[string]any) ([]externalindexer.Resource, error) {
	batch.mu.Lock()
	entry := batch.feeds[site.ID]
	if entry == nil {
		entry = &subscriptionFeedCache{}
		batch.feeds[site.ID] = entry
	}
	batch.mu.Unlock()
	entry.once.Do(func() {
		body, err := batch.api.fetchDocument(ctx, site, site.RSSURL, note, false)
		if err != nil {
			entry.err = err
			return
		}
		entry.resources, entry.err = parseSubscriptionRSS(body)
		for i := range entry.resources {
			entry.resources[i].Indexer = site.Name
			entry.resources[i].IndexerID = strconv.FormatInt(site.ID, 10)
		}
	})
	return entry.resources, entry.err
}

func (api *subscriptionRSSAPI) fetchDocument(ctx context.Context, site siteconfig.Site, raw string, note map[string]any, credentials bool) ([]byte, error) {
	if len(raw) > 4096 || !subscriptionSiteOriginAllowed(site, raw) {
		return nil, errors.New("RSS document is outside configured site")
	}
	service := api.runner.planner.runner.download.service
	transport := service.client.Transport
	if text(note["proxy"]) == "Y" || truthy(note["proxy"]) {
		configuration, err := service.configStore.Snapshot()
		if err != nil {
			return nil, err
		}
		var errProxy error
		transport, errProxy = siteProxyTransport(transport, objectValue(objectValue(configuration["app"])["proxies"]), strings.SplitN(raw, ":", 2)[0])
		if errProxy != nil {
			return nil, errProxy
		}
	}
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	ua := strings.TrimSpace(text(note["ua"]))
	if ua == "" {
		ua = defaultSiteUserAgent
	}
	request.Header.Set("User-Agent", ua)
	if credentials {
		request.Header.Set("Cookie", site.Cookie)
	}
	if err := service.siteLimits.Wait(ctx, service.sites, site.ID); err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, errors.New("RSS document request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return nil, errors.New("RSS document exceeded size limit")
	}
	return body, nil
}

func (batch *subscriptionRSSBatch) resources(ctx context.Context, raw map[string]any, kind, mediaID string) ([]identifiedSubscriptionResource, *recognitionFailure) {
	fail := func(err error) ([]identifiedSubscriptionResource, *recognitionFailure) {
		status := 502
		if errors.Is(err, errSubscriptionRSSUnsupported) {
			status = 501
		}
		return nil, &recognitionFailure{status, "subscription RSS feed or detail could not be verified"}
	}
	scope, err := subscriptionRSSScope(raw)
	if err != nil {
		return nil, &recognitionFailure{422, "invalid subscription RSS sites"}
	}
	sites, err := selectedSubscriptionRSSSites(batch.sites, scope)
	if err != nil {
		return nil, &recognitionFailure{422, "subscription RSS sites are unavailable or ambiguous"}
	}
	service := batch.api.runner.planner.runner.recognition.service
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	identified := []identifiedSubscriptionResource{}
	count := 0
	for _, site := range sites {
		note, group, order, err := subscriptionRSSSiteNote(site)
		if err != nil {
			return fail(err)
		}
		resources, err := batch.feed(ctx, site, note)
		if err != nil {
			return fail(err)
		}
		count += len(resources)
		if count > 2000 {
			return nil, &recognitionFailure{422, "subscription RSS resource limit exceeded"}
		}
		for _, resource := range resources {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			var processed bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=?) OR EXISTS(SELECT 1 FROM GO_RSS_DOWNLOAD_CLAIMS WHERE RESOURCE_KEY=?)`, resource.DownloadURL, subscriptionResourceKey(resource.DownloadURL)).Scan(&processed); err != nil {
				return fail(err)
			}
			if processed {
				continue
			}
			result, failure := batch.api.runner.planner.runner.recognition.recognizeKind(ctx, resource.Title, resource.Description, kind)
			if failure != nil {
				return nil, failure
			}
			if result == nil || strconv.FormatInt(result.detail.ID, 10) != mediaID {
				continue
			}
			if text(note["parse"]) == "Y" || truthy(note["parse"]) {
				batch.catalogOnce.Do(func() { batch.catalog, batch.catalogErr = indexercatalog.Load(service.catalogPath) })
				if batch.catalogErr != nil {
					return fail(batch.catalogErr)
				}
				rules, err := subscriptionDetailRules(batch.catalog, resource.PageURL)
				if err != nil {
					return fail(err)
				}
				body, err := batch.api.fetchDocument(ctx, site, resource.PageURL, note, true)
				if err != nil {
					return fail(err)
				}
				resource, err = parseSubscriptionDetail(ctx, body, resource.Title, rules, resource)
				if err != nil {
					return fail(err)
				}
			}
			copySite := site
			normalizedNote, err := json.Marshal(note)
			if err != nil {
				return fail(err)
			}
			copySite.Note = string(normalizedNote)
			identified = append(identified, identifiedSubscriptionResource{resource: resource, meta: result.meta, revised: text(result.data["rev_string"]), subtitle: result.subtitle, site: &copySite, siteOrder: order, siteGroup: group})
		}
	}
	return identified, nil
}
