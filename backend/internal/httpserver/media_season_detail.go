package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
	"github.com/0xforee/nas-tools/backend/internal/mediasync"
)

type mediaEpisode struct {
	ID             int64                   `json:"id"`
	Number         int                     `json:"number"`
	Season         int                     `json:"season"`
	Name           string                  `json:"name"`
	Overview       string                  `json:"overview"`
	AirDate        string                  `json:"airDate"`
	ProductionCode string                  `json:"productionCode"`
	Runtime        *int                    `json:"runtime"`
	Vote           float64                 `json:"vote"`
	Image          string                  `json:"image"`
	CachedPresence mediasync.CachePresence `json:"cachedPresence"`
}
type mediaSeasonDetail struct {
	ID       string                 `json:"id"`
	Season   int                    `json:"season"`
	SeasonID int64                  `json:"seasonId"`
	Name     string                 `json:"name"`
	Overview string                 `json:"overview"`
	Poster   string                 `json:"poster"`
	AirDate  string                 `json:"airDate"`
	Page     int                    `json:"page"`
	Total    int                    `json:"total"`
	Episodes []mediaEpisode         `json:"episodes"`
	Cache    mediasync.CacheSummary `json:"cache"`
}

func (api mediaDetailAPI) season(ctx context.Context, id string, season, page int) (mediaSeasonDetail, error) {
	api = api.withImages()
	d := mediaSeasonDetail{ID: id, Season: season, Page: page, Episodes: []mediaEpisode{}}
	if !numericTMDBID(id) || season < 0 || season > 10000 || page < 1 || page > 100 {
		return d, errInvalidSubscriptionSelector
	}
	number, _ := strconv.ParseInt(id, 10, 64)
	id = strconv.FormatInt(number, 10)
	d.ID = id
	var payload struct {
		ID       int64  `json:"id"`
		Number   *int   `json:"season_number"`
		Name     string `json:"name"`
		Overview string `json:"overview"`
		Poster   string `json:"poster_path"`
		AirDate  string `json:"air_date"`
		Episodes []struct {
			ID             int64   `json:"id"`
			Number         *int    `json:"episode_number"`
			Season         *int    `json:"season_number"`
			ShowID         *int64  `json:"show_id"`
			Name           string  `json:"name"`
			Overview       string  `json:"overview"`
			AirDate        string  `json:"air_date"`
			ProductionCode string  `json:"production_code"`
			Runtime        *int    `json:"runtime"`
			Vote           float64 `json:"vote_average"`
			Image          string  `json:"still_path"`
		} `json:"episodes"`
	}
	if err := api.tmdbJSON(ctx, "/tv/"+id+"/season/"+strconv.Itoa(season), &payload); err != nil {
		return d, err
	}
	if payload.ID <= 0 || payload.Number == nil || *payload.Number != season || payload.Episodes == nil || len(payload.Episodes) > 10000 {
		return d, errors.New("invalid season identity")
	}
	seenIDs := map[int64]bool{}
	seenNumbers := map[int]bool{}
	for _, ep := range payload.Episodes {
		if ep.ID <= 0 || ep.Number == nil || *ep.Number < 0 || *ep.Number > 10000 || ep.Season == nil || *ep.Season != season || ep.ShowID != nil && *ep.ShowID != number || seenIDs[ep.ID] || seenNumbers[*ep.Number] || ep.Runtime != nil && (*ep.Runtime < 0 || *ep.Runtime > 100000) || ep.Vote < 0 || ep.Vote > 10 {
			return d, errors.New("invalid episode identity")
		}
		seenIDs[ep.ID] = true
		seenNumbers[*ep.Number] = true
	}
	sort.Slice(payload.Episodes, func(i, j int) bool { return *payload.Episodes[i].Number < *payload.Episodes[j].Number })
	d.SeasonID, d.Name, d.Overview, d.Poster, d.AirDate, d.Total = payload.ID, payload.Name, payload.Overview, api.image(payload.Poster), payload.AirDate, len(payload.Episodes)
	begin, end := (page-1)*100, page*100
	if end > d.Total {
		end = d.Total
	}
	queries := []mediasync.CacheQuery{}
	for i := begin; i < end; i++ {
		ep := payload.Episodes[i]
		d.Episodes = append(d.Episodes, mediaEpisode{ID: ep.ID, Number: *ep.Number, Season: season, Name: ep.Name, Overview: ep.Overview, AirDate: ep.AirDate, ProductionCode: ep.ProductionCode, Runtime: ep.Runtime, Vote: ep.Vote, Image: api.image(ep.Image)})
		queries = append(queries, mediasync.CacheQuery{TMDBID: id, Type: "show", Season: &d.Season, Episode: &d.Episodes[len(d.Episodes)-1].Number})
	}
	// Slice growth can move episode storage; rebuild pointer selectors after it
	// reaches its final size, preserving episode 0 rather than truthy defaults.
	for i := range queries {
		queries[i].Episode = &d.Episodes[i].Number
	}
	if len(queries) == 0 {
		queries = []mediasync.CacheQuery{{TMDBID: id, Type: "show", Season: &d.Season}}
	}
	cached := api.displayCache(ctx, queries)
	d.Cache = cached.Cache
	for i := range d.Episodes {
		d.Episodes[i].CachedPresence = cached.Results[i]
	}
	return d, nil
}
func (api mediaDetailAPI) serveSeason(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	season, err := strconv.Atoi(r.PathValue("season"))
	if err != nil || season < 0 || season > 10000 || !numericTMDBID(r.PathValue("id")) {
		writeAPIError(w, 400, 400, "invalid season selector")
		return
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 || page > 100 {
			writeAPIError(w, 400, 400, "invalid episode page")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	detail, err := api.season(ctx, r.PathValue("id"), season, page)
	if err != nil {
		writeAPIError(w, 502, 502, "season details unavailable; no episode state inferred")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": detail})
}
func (api mediaDetailAPI) serveRelated(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	kind, id, relation := r.PathValue("kind"), r.PathValue("id"), r.PathValue("relation")
	page := 1
	if r.Method == "POST" {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
		if r.ParseForm() != nil {
			writeAPIError(w, 400, 400, "invalid related media request")
			return
		}
		kind, id = displayMediaKind(r.Form.Get("type")), r.Form.Get("tmdbid")
		if r.URL.Path == "/api/v1/media/similar" {
			relation = "similar"
		} else {
			relation = "recommendations"
		}
		if raw := r.Form.Get("page"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				writeAPIError(w, 400, 400, "invalid related page")
				return
			}
			page = n
		}
	} else if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeAPIError(w, 400, 400, "invalid related page")
			return
		}
		page = n
	}
	if kind != "movie" && kind != "tv" || !numericTMDBID(id) || relation != "similar" && relation != "recommendations" || page < 1 || page > 100 {
		writeAPIError(w, 400, 400, "invalid related media selector")
		return
	}
	number, _ := strconv.ParseInt(id, 10, 64)
	id = strconv.FormatInt(number, 10)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	items, err := api.discovery.nativeRecommendations(ctx, "/"+kind+"/"+id+"/"+relation, page)
	if err != nil {
		writeAPIError(w, 502, 502, "related media unavailable")
		return
	}
	data := discoveryData{Category: relation, Page: page, Items: items}
	api.discovery.decorateCache(ctx, &data)
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": data})
}
func (api mediaDetailAPI) servePlayback(w http.ResponseWriter, r *http.Request) {
	if !api.authorize(w, r) {
		return
	}
	kind, id := r.PathValue("kind"), r.PathValue("id")
	if kind != "movie" && kind != "tv" || !numericTMDBID(id) {
		writeAPIError(w, 400, 400, "invalid playback selector")
		return
	}
	number, _ := strconv.ParseInt(id, 10, 64)
	id = strconv.FormatInt(number, 10)
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	detail, err := fetchNativeTMDBDetails(ctx, api.service.configStore, api.service.client.Transport, kind, id)
	if err != nil {
		writeAPIError(w, 502, 502, "media identity unavailable; no playback link inferred")
		return
	}
	if api.cache == nil {
		writeAPIError(w, 503, 503, "media server unavailable")
		return
	}
	client, serverKind, username, digest, err := api.cache.connection()
	if err != nil {
		writeAPIError(w, 503, 503, "media server unavailable")
		return
	}
	identity := mediaserver.Identity{Title: detail.Title, TMDBID: id, TV: kind == "tv"}
	date := detail.ReleaseDate
	if kind == "tv" {
		identity.Title = detail.Name
		date = detail.FirstAirDate
	}
	if len(date) >= 4 {
		identity.Year = date[:4]
	}
	inventory, err := client.Inventory(ctx, username, identity)
	if err != nil {
		writeAPIError(w, 502, 502, "live media lookup unavailable; cached presence is not playback evidence")
		return
	}
	if len(inventory.ItemIDs) == 0 {
		writeAPIError(w, 404, 404, "no current matching item in configured media server")
		return
	}
	snapshot, err := api.service.configStore.Snapshot()
	if err != nil {
		writeAPIError(w, 503, 503, "media configuration unavailable")
		return
	}
	link, err := client.PlaybackURL(ctx, inventory.ItemIDs[0], text(objectValue(snapshot[serverKind])["play_host"]))
	if err != nil {
		writeAPIError(w, 502, 502, "safe playback URL unavailable")
		return
	}
	_, _, _, currentDigest, err := api.cache.connection()
	if err != nil || currentDigest != digest {
		writeAPIError(w, 409, 409, "media configuration changed; reload playback lookup")
		return
	}
	writeJSON(w, 200, map[string]any{"code": 0, "success": true, "data": map[string]any{"id": id, "kind": kind, "link": link, "liveVerified": true}})
}
