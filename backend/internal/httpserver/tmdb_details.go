package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

type tmdbMediaDetails struct {
	Attributes   map[string]any `json:"-"`
	EnglishTitle *string        `json:"-"`
	ID           int64          `json:"id"`
	Title        string         `json:"title"`
	Name         string         `json:"name"`
	ReleaseDate  string         `json:"release_date"`
	FirstAirDate string         `json:"first_air_date"`
	SeasonCount  int            `json:"number_of_seasons"`
	ExternalIDs  *struct {
		ID     *int64  `json:"id"`
		IMDbID *string `json:"imdb_id"`
	} `json:"external_ids"`
	Seasons []struct {
		Number   int  `json:"season_number"`
		Episodes *int `json:"episode_count"`
	} `json:"seasons"`
}

func fetchNativeTMDBDetails(ctx context.Context, store *config.Store, transport http.RoundTripper, kind, id string) (tmdbMediaDetails, error) {
	return fetchNativeTMDBDetailsLanguage(ctx, store, transport, kind, id, "")
}

func fetchNativeTMDBDetailsLanguage(ctx context.Context, store *config.Store, transport http.RoundTripper, kind, id, overrideLanguage string) (tmdbMediaDetails, error) {
	var detail tmdbMediaDetails
	if store == nil || !numericTMDBID(id) || kind != "movie" && kind != "tv" {
		return detail, errors.New("invalid TMDB configuration or selector")
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		return detail, err
	}
	app, media := objectValue(snapshot["app"]), objectValue(snapshot["media"])
	key := strings.TrimSpace(text(app["rmt_tmdbkey"]))
	if key == "" {
		return detail, errors.New("TMDB API key is unavailable")
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text(app["tmdb_domain"])), "https://"), "http://"), "/")
	if host == "" {
		host = "api.themoviedb.org"
	}
	if strings.ContainsAny(host, "/?#@\\") || strings.Contains(host, "..") {
		return detail, errors.New("invalid TMDB domain")
	}
	language := strings.TrimSpace(text(media["tmdb_language"]))
	if overrideLanguage != "" {
		language = overrideLanguage
	}
	if language == "" {
		language = "zh"
	}
	if len(language) > 32 || strings.ContainsAny(language, "\r\n") {
		return detail, errors.New("invalid TMDB language")
	}
	endpoint := url.URL{Scheme: "https", Host: host, Path: "/3/" + kind + "/" + id}
	if endpoint.Hostname() == "" {
		return detail, errors.New("invalid TMDB domain")
	}
	query := url.Values{"api_key": {key}, "language": {language}}
	if kind == "tv" {
		query.Set("append_to_response", "external_ids")
	}
	endpoint.RawQuery = query.Encode()
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return detail, err
	}
	probe.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	upstream, err := client.Do(probe)
	if err != nil {
		return detail, err
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return detail, errors.New("TMDB returned non-success status")
	}
	contents, err := io.ReadAll(io.LimitReader(upstream.Body, (2<<20)+1))
	if err != nil || len(contents) > 2<<20 {
		return detail, errors.New("TMDB details are unavailable or too large")
	}
	if err := json.Unmarshal(contents, &detail); err != nil || strconv.FormatInt(detail.ID, 10) != id {
		return detail, errors.New("invalid TMDB details")
	}
	if err := json.Unmarshal(contents, &detail.Attributes); err != nil {
		return detail, errors.New("invalid TMDB attributes")
	}
	if detail.ExternalIDs != nil {
		if detail.ExternalIDs.ID != nil && strconv.FormatInt(*detail.ExternalIDs.ID, 10) != id {
			return detail, errors.New("invalid TMDB external identity")
		}
		// Keep null (verified no IMDb ID) distinct from an omitted field.
		if value, available := objectValue(detail.Attributes["external_ids"])["imdb_id"]; available {
			detail.Attributes["imdb_id"] = value
		}
	}
	if value, available := detail.Attributes["imdb_id"]; available && value != nil {
		imdbID, valid := value.(string)
		if !valid || imdbID != "" && !validNamingIMDbID(imdbID) {
			return detail, errors.New("invalid TMDB IMDb identity")
		}
	}
	return detail, nil
}

func validNamingIMDbID(id string) bool {
	if !strings.HasPrefix(id, "tt") || len(id) < 3 || len(id) > 22 {
		return false
	}
	for _, char := range id[2:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
