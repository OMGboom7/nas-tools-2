package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
)

type tmdbSubscriptionSearchResult struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title"`
	Name          string `json:"name"`
	OriginalName  string `json:"original_name"`
	ReleaseDate   string `json:"release_date"`
	FirstAirDate  string `json:"first_air_date"`
}

func (service subscriptionService) resolveNativeSubscriptionMediaID(ctx context.Context, input subscriptionUpsertRequest) (string, error) {
	return service.resolveNativeMediaID(ctx, input, false)
}

func (service subscriptionService) resolveNativeMediaID(ctx context.Context, input subscriptionUpsertRequest, prepared bool) (string, error) {
	if service.configStore == nil {
		return "", errors.New("TMDB configuration is unavailable")
	}
	snapshot, err := service.configStore.Snapshot()
	if err != nil {
		return "", err
	}
	app, media := objectValue(snapshot["app"]), objectValue(snapshot["media"])
	key := strings.TrimSpace(text(app["rmt_tmdbkey"]))
	if key == "" {
		return "", errors.New("TMDB API key is unavailable")
	}
	host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(text(app["tmdb_domain"])), "https://"), "http://"), "/")
	if host == "" {
		host = "api.themoviedb.org"
	}
	if strings.ContainsAny(host, "/?#@\\") || strings.Contains(host, "..") {
		return "", errors.New("invalid TMDB domain")
	}
	language := strings.TrimSpace(text(media["tmdb_language"]))
	if language == "" {
		language = "zh"
	}
	if len(language) > 32 || strings.ContainsAny(language, "\r\n") {
		return "", errors.New("invalid TMDB language")
	}
	kind := "movie"
	if input.Type == "TV" {
		kind = "tv"
	}
	name := strings.TrimSpace(input.Name)
	if !prepared {
		if service.words != nil {
			processed, err := service.words.Process(ctx, name)
			if err != nil {
				return "", err
			}
			name = strings.TrimSpace(processed.Title)
		}
		if input.Year != "" {
			name = strings.TrimSpace(strings.TrimSuffix(name, " ("+input.Year+")"))
		}
		labelOptions := mediameta.LabelOptions{}
		if service.systemConfig != nil {
			labelOptions, err = mediameta.ReadLabelOptions(ctx, service.systemConfig)
			if err != nil {
				return "", err
			}
		}
		metadata, err := mediameta.ParseWithOptions(ctx, name, "", labelOptions)
		if err != nil {
			return "", err
		}
		name = metadata.Title
		if input.Year == "" {
			input.Year = metadata.Year
		}
	}
	if name == "" {
		return "", nil
	}
	years := []string{input.Year}
	if input.Type == "MOV" && input.Year != "" {
		year, _ := strconv.Atoi(input.Year) // validateSubscription already checked the year.
		years = append(years, strconv.Itoa(year+1), strconv.Itoa(year-1))
	}
	for _, year := range years {
		endpoint := url.URL{Scheme: "https", Host: host, Path: "/3/search/" + kind}
		if endpoint.Hostname() == "" {
			return "", errors.New("invalid TMDB domain")
		}
		query := url.Values{"api_key": {key}, "language": {language}, "query": {name}, "page": {"1"}}
		if year != "" {
			if input.Type == "MOV" {
				query.Set("year", year)
			} else {
				query.Set("first_air_date_year", year)
			}
		}
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return "", err
		}
		request.Header.Set("Accept", "application/json")
		client := *service.client
		client.Timeout = 15 * time.Second
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		upstream, err := client.Do(request)
		if err != nil {
			return "", err
		}
		if upstream.StatusCode != http.StatusOK {
			_ = upstream.Body.Close()
			return "", fmt.Errorf("TMDB search status %d", upstream.StatusCode)
		}
		contents, err := io.ReadAll(io.LimitReader(upstream.Body, 2<<20+1))
		_ = upstream.Body.Close()
		if err != nil || len(contents) > 2<<20 {
			return "", errors.New("TMDB search response is unavailable or too large")
		}
		var payload struct {
			Results []tmdbSubscriptionSearchResult `json:"results"`
		}
		if err := json.Unmarshal(contents, &payload); err != nil || payload.Results == nil {
			return "", errors.New("invalid TMDB search response")
		}
		aliasCandidates := make([]int64, 0, 6)
		for _, candidate := range payload.Results {
			if candidate.ID <= 0 {
				continue
			}
			title, original, date := candidate.Title, candidate.OriginalTitle, candidate.ReleaseDate
			if input.Type == "TV" {
				title, original, date = candidate.Name, candidate.OriginalName, candidate.FirstAirDate
			}
			if year != "" && (len(date) < 4 || date[:4] != year) {
				continue
			}
			if subscriptionTitlesMatch(name, title) || subscriptionTitlesMatch(name, original) {
				return strconv.FormatInt(candidate.ID, 10), nil
			}
			if len(aliasCandidates) < 6 {
				aliasCandidates = append(aliasCandidates, candidate.ID)
			}
		}
		for _, candidateID := range aliasCandidates {
			aliases, err := service.fetchNativeTMDBAliases(ctx, host, key, language, kind, candidateID)
			if err != nil {
				return "", err
			}
			for _, alias := range aliases {
				if subscriptionTitlesMatch(name, alias) {
					return strconv.FormatInt(candidateID, 10), nil
				}
			}
		}
	}
	return "", nil
}

func (service subscriptionService) fetchNativeTMDBAliases(ctx context.Context, host, key, language, kind string, id int64) ([]string, error) {
	endpoint := url.URL{Scheme: "https", Host: host, Path: "/3/" + kind + "/" + strconv.FormatInt(id, 10)}
	endpoint.RawQuery = url.Values{"api_key": {key}, "language": {language}, "append_to_response": {"alternative_titles,translations"}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
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
		return nil, fmt.Errorf("TMDB alternative titles status %d", upstream.StatusCode)
	}
	contents, err := io.ReadAll(io.LimitReader(upstream.Body, 2<<20+1))
	if err != nil || len(contents) > 2<<20 {
		return nil, errors.New("TMDB alternative titles response is unavailable or too large")
	}
	var detail struct {
		ID                int64 `json:"id"`
		AlternativeTitles struct {
			Titles []struct {
				Title string `json:"title"`
			} `json:"titles"`
			Results []struct {
				Title string `json:"title"`
			} `json:"results"`
		} `json:"alternative_titles"`
		Translations struct {
			Translations []struct {
				Data struct {
					Title string `json:"title"`
					Name  string `json:"name"`
				} `json:"data"`
			} `json:"translations"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(contents, &detail); err != nil || detail.ID != id {
		return nil, errors.New("invalid TMDB alternative titles response")
	}
	aliases := make([]string, 0, len(detail.AlternativeTitles.Titles)+len(detail.AlternativeTitles.Results)+len(detail.Translations.Translations))
	for _, title := range detail.AlternativeTitles.Titles {
		aliases = append(aliases, title.Title)
	}
	for _, title := range detail.AlternativeTitles.Results {
		aliases = append(aliases, title.Title)
	}
	for _, translation := range detail.Translations.Translations {
		aliases = append(aliases, translation.Data.Title, translation.Data.Name)
	}
	return aliases, nil
}

func subscriptionTitlesMatch(want, candidate string) bool {
	want, candidate = normalizeSubscriptionTitle(want), normalizeSubscriptionTitle(candidate)
	if want == "" || candidate == "" {
		return false
	}
	if want == candidate {
		return true
	}
	for _, char := range want {
		if char > unicode.MaxASCII || !unicode.IsLetter(char) && !unicode.IsDigit(char) {
			return false
		}
	}
	return strings.Contains(candidate, want)
}

func normalizeSubscriptionTitle(value string) string {
	var normalized strings.Builder
	for _, char := range strings.ToUpper(value) {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}
