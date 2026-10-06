package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type tmdbRecommendation struct {
	ID           int64   `json:"id"`
	MediaType    string  `json:"media_type"`
	Title        string  `json:"title"`
	Name         string  `json:"name"`
	ReleaseDate  string  `json:"release_date"`
	FirstAirDate string  `json:"first_air_date"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	Overview     string  `json:"overview"`
	VoteAverage  float64 `json:"vote_average"`
}

func (service discoveryService) nativeRecommendations(ctx context.Context, endpoint string, page int) ([]discoveryMedia, error) {
	if service.config == nil || service.databasePath == "" {
		return nil, errors.New("native discovery configuration is unavailable")
	}
	snapshot, err := service.config.Snapshot()
	if err != nil {
		return nil, err
	}
	app := objectValue(snapshot["app"])
	media := objectValue(snapshot["media"])
	key := strings.TrimSpace(text(app["rmt_tmdbkey"]))
	if key == "" {
		return nil, errors.New("TMDB API key is unavailable")
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text(app["tmdb_domain"])), "https://"), "http://"), "/")
	if host == "" {
		host = "api.themoviedb.org"
	}
	if strings.ContainsAny(host, "/?#@\\") || strings.Contains(host, "..") {
		return nil, errors.New("invalid TMDB API domain")
	}
	apiURL := url.URL{Scheme: "https", Host: host, Path: "/3" + endpoint}
	if apiURL.Hostname() == "" {
		return nil, errors.New("invalid TMDB API domain")
	}
	language := strings.TrimSpace(text(media["tmdb_language"]))
	if language == "" {
		language = "zh"
	}
	if len(language) > 32 || strings.ContainsAny(language, "\r\n") {
		return nil, errors.New("invalid TMDB language")
	}
	query := url.Values{"api_key": {key}, "page": {strconv.Itoa(page)}, "language": {language}}
	if truthy(media["tmdb_include_adult"]) {
		query.Set("include_adult", "true")
	} else {
		query.Set("include_adult", "false")
	}
	apiURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	client := *service.client
	client.Timeout = 15 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	upstream, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TMDB status %d", upstream.StatusCode)
	}
	contents, err := io.ReadAll(io.LimitReader(upstream.Body, 2<<20+1))
	if err != nil || len(contents) > 2<<20 {
		return nil, errors.New("TMDB response is unavailable or too large")
	}
	var payload struct {
		Results []tmdbRecommendation `json:"results"`
	}
	if err := json.Unmarshal(contents, &payload); err != nil || payload.Results == nil {
		return nil, errors.New("invalid TMDB response")
	}
	databaseURL := (&url.URL{Scheme: "file", Path: service.databasePath, RawQuery: "mode=ro"}).String()
	database, err := sql.Open("sqlite", databaseURL)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return nil, err
	}
	imageBase := strings.TrimRight(strings.TrimSpace(text(app["tmdb_image_url"])), "/")
	if imageBase == "" {
		imageBase = "https://image.tmdb.org"
	}
	imageURL, err := url.Parse(imageBase)
	if err != nil || imageURL.Host == "" || imageURL.User != nil || imageURL.RawQuery != "" || imageURL.Fragment != "" || imageURL.Scheme != "https" && imageURL.Scheme != "http" {
		return nil, errors.New("invalid TMDB image domain")
	}
	items := make([]discoveryMedia, 0, len(payload.Results))
	for _, raw := range payload.Results {
		if raw.ID < 1 || raw.MediaType == "person" {
			continue
		}
		kind := "MOV"
		if endpoint == "/tv/popular" || endpoint == "/tv/on_the_air" || raw.MediaType == "tv" {
			kind = "TV"
		}
		title, date, label := raw.Title, raw.ReleaseDate, "电影"
		if kind == "TV" {
			title, date, label = raw.Name, raw.FirstAirDate, "电视剧"
		}
		if title == "" {
			continue
		}
		year := ""
		if len(date) >= 4 {
			year = date[:4]
		}
		id := strconv.FormatInt(raw.ID, 10)
		subscribed, err := discoverySubscribed(ctx, database, kind, id, title, year)
		if err != nil {
			return nil, err
		}
		image, backdrop := tmdbImageURL(imageBase, raw.PosterPath), tmdbImageURL(imageBase, raw.BackdropPath)
		if service.images != nil {
			image, backdrop = service.images.rewrite(image), service.images.rewrite(backdrop)
		}
		vote := raw.VoteAverage
		if math.IsNaN(vote) || math.IsInf(vote, 0) || vote < 0 {
			vote = 0
		}
		items = append(items, discoveryMedia{ID: id, Title: title, Year: year, Type: kind, MediaType: label,
			Vote: fmt.Sprintf("%.1f", vote), Image: image, Backdrop: backdrop, Overview: raw.Overview, Subscribed: subscribed})
	}
	return items, nil
}

func tmdbImageURL(base, path string) string {
	if path == "" || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return ""
	}
	return base + "/t/p/w500" + path
}

func discoverySubscribed(ctx context.Context, database *sql.DB, kind, id, title, year string) (bool, error) {
	table := "RSS_MOVIES"
	if kind == "TV" {
		table = "RSS_TVS"
	}
	var exists int
	if err := database.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, nil
	}
	query := "SELECT EXISTS(SELECT 1 FROM " + table + " WHERE TMDBID=? OR (NAME=? AND (?='' OR YEAR=?) AND (COALESCE(TMDBID,'')='' OR TMDBID=?)))"
	if err := database.QueryRowContext(ctx, query, id, title, year, year, id).Scan(&exists); err != nil {
		return false, err
	}
	return exists != 0, nil
}
