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
	inventoryNames map[string]string
	Attributes     map[string]any `json:"-"`
	EnglishTitle   *string        `json:"-"`
	ID             int64          `json:"id"`
	Title          string         `json:"title"`
	Name           string         `json:"name"`
	ReleaseDate    string         `json:"release_date"`
	FirstAirDate   string         `json:"first_air_date"`
	SeasonCount    int            `json:"number_of_seasons"`
	ExternalIDs    *struct {
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
	extra := map[string]string{}
	if kind == "tv" {
		extra["append_to_response"] = "external_ids"
	}
	contents, err := requestTMDBDetail(ctx, snapshot, transport, "/"+kind+"/"+id, overrideLanguage, extra, 2<<20)
	if err != nil {
		return detail, err
	}
	return decodeTMDBDetails(contents, id)
}

func decodeTMDBDetails(contents []byte, id string) (tmdbMediaDetails, error) {
	var detail tmdbMediaDetails
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

func requestTMDBDetail(ctx context.Context, snapshot map[string]any, transport http.RoundTripper, path, overrideLanguage string, extra map[string]string, limit int64) ([]byte, error) {
	app, media := objectValue(snapshot["app"]), objectValue(snapshot["media"])
	key := strings.TrimSpace(text(app["rmt_tmdbkey"]))
	if key == "" {
		return nil, errors.New("TMDB API key is unavailable")
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text(app["tmdb_domain"])), "https://"), "http://"), "/")
	if host == "" {
		host = "api.themoviedb.org"
	}
	if strings.ContainsAny(host, "/?#@\\") || strings.Contains(host, "..") {
		return nil, errors.New("invalid TMDB domain")
	}
	language := strings.TrimSpace(text(media["tmdb_language"]))
	if overrideLanguage != "" {
		language = overrideLanguage
	}
	if language == "" {
		language = "zh"
	}
	if len(language) > 32 || strings.ContainsAny(language, "\r\n") {
		return nil, errors.New("invalid TMDB language")
	}
	endpoint := url.URL{Scheme: "https", Host: host, Path: "/3" + path}
	if endpoint.Hostname() == "" {
		return nil, errors.New("invalid TMDB domain")
	}
	query := url.Values{"api_key": {key}, "language": {language}}
	for k, v := range extra {
		query.Set(k, v)
	}
	endpoint.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("invalid TMDB detail address")
	}
	probe.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	upstream, err := client.Do(probe)
	if err == nil && upstream.StatusCode == http.StatusTooManyRequests {
		if delay, ok := tmdbRetryDelay(upstream.Header, time.Now()); ok {
			upstream.Body.Close()
			deadline, _ := ctx.Deadline()
			if delay >= time.Until(deadline) {
				return nil, errors.New("TMDB rate limit exceeds request deadline")
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			upstream, err = client.Do(probe.Clone(ctx))
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("TMDB detail request failed")
	}
	defer upstream.Body.Close()
	if upstream.StatusCode != http.StatusOK {
		return nil, errors.New("TMDB returned non-success status")
	}
	contents, err := io.ReadAll(io.LimitReader(upstream.Body, limit+1))
	if err != nil || int64(len(contents)) > limit {
		return nil, errors.New("TMDB details are unavailable or too large")
	}
	return contents, nil
}

func tmdbRetryDelay(header http.Header, now time.Time) (time.Duration, bool) {
	if value := header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
		date, err := http.ParseTime(value)
		return max(0, date.Sub(now)), err == nil
	}
	seconds, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil || seconds <= 0 {
		return 0, false
	}
	return max(0, time.Unix(seconds, 0).Sub(now)), true
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
