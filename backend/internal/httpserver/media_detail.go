package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediasync"
)

type mediaDetailAPI struct {
	service   subscriptionService
	cache     *mediaSyncAPI
	discovery discoveryService
	auth      *nativeAuthentication
	imageBase string
}
type mediaPerson struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	OriginalName string `json:"originalName"`
	Role         string `json:"role"`
	Job          string `json:"job"`
	Department   string `json:"department"`
	Image        string `json:"image"`
	Link         string `json:"link"`
}
type mediaFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
type mediaSeason struct {
	Number         int                     `json:"number"`
	ID             int64                   `json:"id"`
	Name           string                  `json:"name"`
	Overview       string                  `json:"overview"`
	AirDate        string                  `json:"airDate"`
	EpisodeCount   *int                    `json:"episodeCount"`
	Poster         string                  `json:"poster"`
	CachedPresence mediasync.CachePresence `json:"cachedPresence"`
}
type mediaSubscriptionRef struct {
	ID     string `json:"id"`
	Season string `json:"season"`
}
type mediaDisplayDetail struct {
	ID                 string                  `json:"id"`
	Kind               string                  `json:"kind"`
	SourceID           string                  `json:"sourceId"`
	Title              string                  `json:"title"`
	OriginalTitle      string                  `json:"originalTitle"`
	Year               string                  `json:"year"`
	Overview           string                  `json:"overview"`
	Tagline            string                  `json:"tagline"`
	Vote               float64                 `json:"vote"`
	Runtime            *int                    `json:"runtime"`
	Poster             string                  `json:"poster"`
	Backdrop           string                  `json:"backdrop"`
	Genres             []string                `json:"genres"`
	Link               string                  `json:"link"`
	DoubanID           string                  `json:"doubanId"`
	DoubanLink         string                  `json:"doubanLink"`
	Facts              []mediaFact             `json:"facts"`
	Cast               []mediaPerson           `json:"cast"`
	Crew               []mediaPerson           `json:"crew"`
	Seasons            []mediaSeason           `json:"seasons"`
	Subscribed         bool                    `json:"subscribed"`
	SubscriptionsKnown bool                    `json:"subscriptionsKnown"`
	Subscriptions      []mediaSubscriptionRef  `json:"subscriptions"`
	Cache              mediasync.CacheSummary  `json:"cache"`
	CachedPresence     mediasync.CachePresence `json:"cachedPresence"`
	Warnings           []string                `json:"warnings"`
}
type displayTMDBDetails struct {
	OriginalTitle  string  `json:"original_title"`
	OriginalName   string  `json:"original_name"`
	Overview       string  `json:"overview"`
	Tagline        string  `json:"tagline"`
	Status         string  `json:"status"`
	Vote           float64 `json:"vote_average"`
	Runtime        *int    `json:"runtime"`
	EpisodeRuntime []int   `json:"episode_run_time"`
	Poster         string  `json:"poster_path"`
	Backdrop       string  `json:"backdrop_path"`
	Language       string  `json:"original_language"`
	Budget         int64   `json:"budget"`
	Revenue        int64   `json:"revenue"`
	Genres         []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Countries []struct {
		Name string `json:"name"`
	} `json:"production_countries"`
	Companies []struct {
		Name string `json:"name"`
	} `json:"production_companies"`
	Networks []struct {
		Name string `json:"name"`
	} `json:"networks"`
	Seasons []struct {
		Number       *int   `json:"season_number"`
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Overview     string `json:"overview"`
		AirDate      string `json:"air_date"`
		EpisodeCount *int   `json:"episode_count"`
		Poster       string `json:"poster_path"`
	} `json:"seasons"`
}

func displayMediaKind(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "MOV", "MOVIE", "电影":
		return "movie"
	case "TV", "电视剧", "剧集", "ANIME", "动漫":
		return "tv"
	}
	return ""
}
func mediaDetailRedirect(w http.ResponseWriter, r *http.Request) {
	kind, id := displayMediaKind(r.URL.Query().Get("type")), strings.TrimSpace(r.URL.Query().Get("id"))
	raw := id
	if strings.HasPrefix(raw, "DB:") {
		raw = strings.TrimPrefix(raw, "DB:")
	} else if strings.HasPrefix(raw, "BG:") && kind == "tv" {
		raw = strings.TrimPrefix(raw, "BG:")
	}
	if kind == "" || !numericTMDBID(raw) || len(id) > 64 {
		writeAPIError(w, 400, 400, "invalid media detail link")
		return
	}
	number, _ := strconv.ParseInt(raw, 10, 64)
	canonical := strconv.FormatInt(number, 10)
	if strings.HasPrefix(id, "DB:") {
		id = "DB:" + canonical
	} else if strings.HasPrefix(id, "BG:") {
		id = "BG:" + canonical
	} else {
		id = canonical
	}
	// This public, relative redirect exposes no data or credentials. The React
	// page requires login before its authenticated metadata requests.
	http.Redirect(w, r, "/media/"+kind+"/"+url.PathEscape(id), http.StatusSeeOther)
}
func (api mediaDetailAPI) authorize(w http.ResponseWriter, r *http.Request) bool {
	if api.auth == nil || api.service.configStore == nil || api.service.client == nil {
		writeAPIError(w, 503, 503, "native media details unavailable")
		return false
	}
	if _, err := api.auth.service.VerifyToken(r.Header.Get("Authorization")); err != nil {
		writeAPIError(w, 401, 401, "invalid authorization")
		return false
	}
	return true
}
func (api mediaDetailAPI) resolve(ctx context.Context, kind, id string) (string, error) {
	if kind != "movie" && kind != "tv" || id == "" || len(id) > 64 {
		return "", errInvalidSubscriptionSelector
	}
	if numericTMDBID(id) {
		n, _ := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		return strconv.FormatInt(n, 10), nil
	}
	if !strings.HasPrefix(id, "DB:") && !strings.HasPrefix(id, "BG:") {
		return "", errInvalidSubscriptionSelector
	}
	if strings.HasPrefix(id, "BG:") && kind != "tv" {
		return "", errInvalidSubscriptionSelector
	}
	typeCode := "MOV"
	if kind == "tv" {
		typeCode = "TV"
	}
	resolved, err := api.service.resolveNativeSubscriptionInput(ctx, subscriptionUpsertRequest{Type: typeCode, MediaID: id})
	if err != nil {
		return "", err
	}
	if !numericTMDBID(resolved.MediaID) {
		return "", errors.New("media identity not found")
	}
	n, _ := strconv.ParseInt(resolved.MediaID, 10, 64)
	return strconv.FormatInt(n, 10), nil
}
func (api mediaDetailAPI) tmdbJSON(ctx context.Context, path string, output any) error {
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		return err
	}
	app, media := objectValue(snapshot["app"]), objectValue(snapshot["media"])
	key := strings.TrimSpace(text(app["rmt_tmdbkey"]))
	if key == "" {
		return errors.New("TMDB configuration unavailable")
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text(app["tmdb_domain"])), "https://"), "http://"), "/")
	if host == "" {
		host = "api.themoviedb.org"
	}
	if strings.ContainsAny(host, "/?#@\\") || strings.Contains(host, "..") {
		return errors.New("invalid TMDB host")
	}
	language := strings.TrimSpace(text(media["tmdb_language"]))
	if language == "" {
		language = "zh"
	}
	if len(language) > 32 || strings.ContainsAny(language, "\r\n") {
		return errors.New("invalid TMDB language")
	}
	endpoint := url.URL{Scheme: "https", Host: host, Path: "/3" + path}
	if endpoint.Hostname() == "" {
		return errors.New("invalid TMDB endpoint")
	}
	endpoint.RawQuery = url.Values{"api_key": {key}, "language": {language}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	client := http.Client{Transport: api.service.client.Transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("TMDB response unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return errors.New("TMDB response too large")
	}
	return json.Unmarshal(raw, output)
}
func (api mediaDetailAPI) image(path string) string {
	if path == "" {
		return ""
	}
	if api.imageBase == "" {
		api = api.withImages()
	}
	base := api.imageBase
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return ""
	}
	image := tmdbImageURL(base, path)
	if api.service.images != nil {
		image = api.service.images.rewrite(image)
	}
	return image
}
func (api mediaDetailAPI) withImages() mediaDetailAPI {
	api.imageBase = "https://image.tmdb.org"
	if snapshot, err := api.service.configStore.Snapshot(); err == nil {
		if candidate := strings.TrimRight(strings.TrimSpace(text(objectValue(snapshot["app"])["tmdb_image_url"])), "/"); candidate != "" {
			api.imageBase = candidate
		}
	}
	return api
}
func (api mediaDetailAPI) displayCache(ctx context.Context, queries []mediasync.CacheQuery) mediasync.CacheBatch {
	unknown := mediasync.CacheBatch{Cache: mediasync.CacheSummary{State: "unavailable", DisplayOnly: true, Libraries: []string{}}, Results: make([]mediasync.CachePresence, len(queries))}
	for i := range unknown.Results {
		unknown.Results[i].State = "unavailable"
	}
	if api.cache == nil {
		return unknown
	}
	result := unknown
	result.Results = append([]mediasync.CachePresence{}, unknown.Results...)
	for offset := 0; offset < len(queries); offset += 200 {
		end := offset + 200
		if end > len(queries) {
			end = len(queries)
		}
		batch, err := api.cache.cachedPresence(ctx, queries[offset:end])
		if err != nil {
			return unknown
		}
		if offset == 0 {
			result.Cache = batch.Cache
		} else if result.Cache.Generation != batch.Cache.Generation || result.Cache.State != batch.Cache.State || result.Cache.Server != batch.Cache.Server {
			return unknown
		}
		copy(result.Results[offset:end], batch.Results)
	}
	return result
}
func (api mediaDetailAPI) subscriptionRefs(ctx context.Context, kind, id, title, year string) ([]mediaSubscriptionRef, error) {
	result := []mediaSubscriptionRef{}
	if api.service.databasePath == "" {
		return result, errors.New("subscription database unavailable")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: api.service.databasePath, RawQuery: "mode=ro"}).String())
	if err != nil {
		return result, err
	}
	defer db.Close()
	table := "RSS_MOVIES"
	season := `''`
	if kind == "tv" {
		table = "RSS_TVS"
		season = `COALESCE(SEASON,'')`
	}
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists); err != nil {
		return result, err
	}
	if exists == 0 {
		return result, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT ID,`+season+` FROM `+table+` WHERE TMDBID=? OR (NAME=? AND (?='' OR YEAR=?) AND (COALESCE(TMDBID,'')='' OR TMDBID=?)) ORDER BY ID LIMIT 1001`, id, title, year, year, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref mediaSubscriptionRef
		if err := rows.Scan(&ref.ID, &ref.Season); err != nil {
			return result, err
		}
		result = append(result, ref)
		if len(result) > 1000 {
			return nil, errors.New("too many subscription references")
		}
	}
	return result, rows.Err()
}
func (api mediaDetailAPI) detail(ctx context.Context, kind, source string) (mediaDisplayDetail, error) {
	api = api.withImages()
	d := mediaDisplayDetail{Kind: kind, SourceID: source, Genres: []string{}, Facts: []mediaFact{}, Cast: []mediaPerson{}, Crew: []mediaPerson{}, Seasons: []mediaSeason{}, Subscriptions: []mediaSubscriptionRef{}, Warnings: []string{}}
	id, err := api.resolve(ctx, kind, source)
	if err != nil {
		return d, err
	}
	d.ID = id
	detail, err := fetchNativeTMDBDetails(ctx, api.service.configStore, api.service.client.Transport, kind, id)
	if err != nil {
		return d, err
	}
	raw, err := json.Marshal(detail.Attributes)
	if err != nil {
		return d, err
	}
	var attr displayTMDBDetails
	if err := json.Unmarshal(raw, &attr); err != nil {
		return d, err
	}
	d.Title, d.OriginalTitle, d.Year = detail.Title, attr.OriginalTitle, detail.ReleaseDate
	if kind == "tv" {
		d.Title, d.OriginalTitle, d.Year = detail.Name, attr.OriginalName, detail.FirstAirDate
	}
	date := d.Year
	if len(d.Year) >= 4 {
		d.Year = d.Year[:4]
	} else {
		d.Year = ""
	}
	if strings.TrimSpace(d.Title) == "" || attr.Vote < 0 || attr.Vote > 10 {
		return d, errors.New("invalid media details")
	}
	d.Overview, d.Tagline, d.Vote, d.Runtime = attr.Overview, attr.Tagline, attr.Vote, attr.Runtime
	if d.Runtime == nil && len(attr.EpisodeRuntime) > 0 {
		d.Runtime = &attr.EpisodeRuntime[0]
	}
	if d.Runtime != nil && (*d.Runtime < 0 || *d.Runtime > 100000) {
		return d, errors.New("invalid runtime")
	}
	d.Poster, d.Backdrop = api.image(attr.Poster), api.image(attr.Backdrop)
	d.Link = "https://www.themoviedb.org/" + kind + "/" + id
	if strings.HasPrefix(source, "DB:") {
		dbid := strings.TrimPrefix(source, "DB:")
		if numericTMDBID(dbid) {
			d.DoubanID = dbid
			d.DoubanLink = "https://movie.douban.com/subject/" + dbid + "/"
		}
	}
	for _, genre := range attr.Genres {
		if genre.Name != "" {
			d.Genres = append(d.Genres, genre.Name)
		}
	}
	addFact := func(label, value string) {
		if value != "" {
			d.Facts = append(d.Facts, mediaFact{label, value})
		}
	}
	addFact("原始标题", d.OriginalTitle)
	addFact("状态", attr.Status)
	addFact("上映日期", date)
	addFact("原始语言", attr.Language)
	if attr.Budget > 0 {
		addFact("成本", strconv.FormatInt(attr.Budget, 10))
	}
	if attr.Revenue > 0 {
		addFact("收入", strconv.FormatInt(attr.Revenue, 10))
	}
	for _, group := range []struct {
		label string
		items []struct {
			Name string `json:"name"`
		}
	}{{"出品国家", attr.Countries}, {"制作公司", attr.Companies}, {"电视网", attr.Networks}} {
		names := []string{}
		for _, item := range group.items {
			if item.Name != "" {
				names = append(names, item.Name)
			}
		}
		addFact(group.label, strings.Join(names, "、"))
	}
	if kind == "tv" {
		if len(attr.Seasons) > 1000 {
			return d, errors.New("too many seasons")
		}
		seen := map[int]bool{}
		for _, season := range attr.Seasons {
			if season.Number == nil || *season.Number < 0 || *season.Number > 10000 || seen[*season.Number] || season.EpisodeCount != nil && (*season.EpisodeCount < 0 || *season.EpisodeCount > 100000) {
				return d, errors.New("invalid season identity")
			}
			seen[*season.Number] = true
			d.Seasons = append(d.Seasons, mediaSeason{Number: *season.Number, ID: season.ID, Name: season.Name, Overview: season.Overview, AirDate: season.AirDate, EpisodeCount: season.EpisodeCount, Poster: api.image(season.Poster)})
		}
		sort.Slice(d.Seasons, func(i, j int) bool { return d.Seasons[i].Number > d.Seasons[j].Number })
	}
	if err := api.credits(ctx, kind, id, &d); err != nil {
		if ctx.Err() != nil {
			return d, ctx.Err()
		}
		d.Warnings = append(d.Warnings, "演职人员暂时不可用；其余详情已保留")
	}
	refs, err := api.subscriptionRefs(ctx, kind, id, d.Title, d.Year)
	if err != nil {
		d.Warnings = append(d.Warnings, "当前订阅记录无法核实")
	} else {
		d.SubscriptionsKnown = true
		d.Subscriptions = refs
		d.Subscribed = len(refs) > 0
	}
	cacheKind := "movie"
	if kind == "tv" {
		cacheKind = "show"
	}
	queries := []mediasync.CacheQuery{{TMDBID: id, Title: d.Title, Year: d.Year, Type: cacheKind}}
	for i := range d.Seasons {
		queries = append(queries, mediasync.CacheQuery{TMDBID: id, Type: "show", Season: &d.Seasons[i].Number})
	}
	cached := api.displayCache(ctx, queries)
	d.Cache, d.CachedPresence = cached.Cache, cached.Results[0]
	for i := range d.Seasons {
		d.Seasons[i].CachedPresence = cached.Results[i+1]
	}
	return d, nil
}
func (api mediaDetailAPI) credits(ctx context.Context, kind, id string, d *mediaDisplayDetail) error {
	type person struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		OriginalName string `json:"original_name"`
		Role         string `json:"character"`
		Job          string `json:"job"`
		Department   string `json:"department"`
		Profile      string `json:"profile_path"`
	}
	var payload struct {
		ID   int64    `json:"id"`
		Cast []person `json:"cast"`
		Crew []person `json:"crew"`
	}
	if err := api.tmdbJSON(ctx, "/"+kind+"/"+id+"/credits", &payload); err != nil {
		return err
	}
	if strconv.FormatInt(payload.ID, 10) != id || payload.Cast == nil || payload.Crew == nil || len(payload.Cast) > 3000 || len(payload.Crew) > 3000 {
		return errors.New("invalid credits")
	}
	convert := func(items []person) ([]mediaPerson, error) {
		result := []mediaPerson{}
		for _, p := range items {
			if p.ID <= 0 || p.Name == "" {
				return nil, errors.New("invalid person identity")
			}
			result = append(result, mediaPerson{ID: p.ID, Name: p.Name, OriginalName: p.OriginalName, Role: p.Role, Job: p.Job, Department: p.Department, Image: api.image(p.Profile), Link: "https://www.themoviedb.org/person/" + strconv.FormatInt(p.ID, 10)})
		}
		return result, nil
	}
	cast, err := convert(payload.Cast)
	if err != nil {
		return err
	}
	crew, err := convert(payload.Crew)
	if err != nil {
		return err
	}
	d.Cast, d.Crew = cast, crew
	return nil
}
func (api mediaDetailAPI) serveDetail(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("id")
	legacy := r.Method == "POST"
	if legacy {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if err := r.ParseForm(); err != nil {
			writeAPIError(w, 400, 400, "invalid detail request")
			return
		}
		kind, id = displayMediaKind(r.Form.Get("type")), strings.TrimSpace(r.Form.Get("tmdbid"))
	}
	if kind != "movie" && kind != "tv" || id == "" || len(id) > 64 {
		writeAPIError(w, 400, 400, "invalid media selector")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	detail, err := api.detail(ctx, kind, id)
	if err != nil {
		status := 502
		if errors.Is(err, errInvalidSubscriptionSelector) {
			status = 400
		}
		writeAPIError(w, status, status, "native media identity or details unavailable")
		return
	}
	if legacy {
		api.writeLegacyDetail(w, detail)
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": detail})
}
func (api mediaDetailAPI) writeLegacyDetail(w http.ResponseWriter, d mediaDisplayDetail) {
	fav := "0"
	if d.Subscribed {
		fav = "1"
	} else if d.CachedPresence.State == "present" {
		fav = "2"
	}
	rssid := ""
	if len(d.Subscriptions) > 0 {
		rssid = d.Subscriptions[0].ID
	}
	seasons := []map[string]any{}
	for _, s := range d.Seasons {
		seasons = append(seasons, map[string]any{"season_number": s.Number, "episode_count": s.EpisodeCount, "id": s.ID, "name": s.Name, "overview": s.Overview, "air_date": s.AirDate, "poster_path": s.Poster, "state": s.CachedPresence.State == "present", "state_known": s.CachedPresence.State != "unavailable"})
	}
	runtime := ""
	if d.Runtime != nil {
		runtime = fmt.Sprintf("%d 分钟", *d.Runtime)
	}
	facts := []map[string]string{}
	for _, fact := range d.Facts {
		facts = append(facts, map[string]string{fact.Label: fact.Value})
	}
	crews := []map[string]string{}
	for _, person := range d.Crew {
		if len(crews) == 6 {
			break
		}
		crews = append(crews, map[string]string{person.Name: person.Job})
	}
	actors := []map[string]any{}
	for _, person := range d.Cast {
		actors = append(actors, map[string]any{"id": person.ID, "name": person.Name, "original_name": person.OriginalName, "role": person.Role, "image": person.Image, "profile": person.Link})
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"tmdbid": d.ID, "douban_id": d.DoubanID, "background": d.Backdrop, "image": d.Poster, "vote": d.Vote, "year": d.Year, "title": d.Title, "genres": strings.Join(d.Genres, ", "), "overview": d.Overview, "runtime": runtime, "fact": facts, "crews": crews, "actors": actors, "link": d.Link, "douban_link": d.DoubanLink, "fav": fav, "item_url": "", "rssid": rssid, "seasons": seasons, "cache": d.Cache, "cachedPresence": d.CachedPresence, "exists_known": d.CachedPresence.State != "unavailable", "subscriptions_known": d.SubscriptionsKnown, "warnings": d.Warnings}})
}
