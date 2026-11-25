package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type nativeSubscriptionMetadata struct {
	ID       string
	Title    string
	Year     string
	Season   string
	Image    string
	Overview string
	Note     string
	Total    int
	Lack     int
}

func numericTMDBID(value string) bool {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return err == nil && id > 0
}

func (service subscriptionService) fetchNativeSubscriptionMetadata(ctx context.Context, input subscriptionUpsertRequest) (*nativeSubscriptionMetadata, error) {
	if service.configStore == nil || !numericTMDBID(input.MediaID) {
		return nil, errors.New("TMDB configuration or media ID is unavailable")
	}
	snapshot, err := service.configStore.Snapshot()
	if err != nil {
		return nil, err
	}
	kind := "movie"
	if input.Type == "TV" {
		kind = "tv"
	}
	contents, err := requestTMDBDetail(ctx, snapshot, service.client.Transport, "/"+kind+"/"+input.MediaID, "", nil, 2<<20)
	if err != nil {
		return nil, err
	}
	return subscriptionMetadataFromJSON(input, contents, objectValue(snapshot["app"]))
}

func subscriptionMetadataFromJSON(input subscriptionUpsertRequest, contents []byte, app map[string]any) (*nativeSubscriptionMetadata, error) {
	var detail struct {
		ID           int64   `json:"id"`
		Title        string  `json:"title"`
		Name         string  `json:"name"`
		ReleaseDate  string  `json:"release_date"`
		FirstAirDate string  `json:"first_air_date"`
		PosterPath   string  `json:"poster_path"`
		BackdropPath string  `json:"backdrop_path"`
		Overview     string  `json:"overview"`
		VoteAverage  float64 `json:"vote_average"`
		Seasons      []struct {
			Number       int `json:"season_number"`
			EpisodeCount int `json:"episode_count"`
		} `json:"seasons"`
	}
	if err := json.Unmarshal(contents, &detail); err != nil || detail.ID <= 0 || strconv.FormatInt(detail.ID, 10) != input.MediaID {
		return nil, errors.New("invalid TMDB media details")
	}
	metadata := &nativeSubscriptionMetadata{ID: input.MediaID, Title: strings.TrimSpace(detail.Title), Overview: detail.Overview}
	date := detail.ReleaseDate
	if input.Type == "TV" {
		metadata.Title = strings.TrimSpace(detail.Name)
		date = detail.FirstAirDate
	}
	if metadata.Title == "" {
		return nil, errors.New("TMDB title is unavailable")
	}
	metadata.Year = input.Year
	if len(date) >= 4 {
		metadata.Year = date[:4]
	}
	imageBase := strings.TrimRight(strings.TrimSpace(text(app["tmdb_image_url"])), "/")
	if imageBase == "" {
		imageBase = "https://image.tmdb.org"
	}
	imageURL, err := url.Parse(imageBase)
	if err != nil || imageURL.Host == "" || imageURL.User != nil || imageURL.RawQuery != "" || imageURL.Fragment != "" || imageURL.Scheme != "https" && imageURL.Scheme != "http" {
		return nil, errors.New("invalid TMDB image domain")
	}
	poster := tmdbImageURL(imageBase, detail.PosterPath)
	metadata.Image = tmdbImageURL(imageBase, detail.BackdropPath)
	if metadata.Image == "" {
		metadata.Image = poster
	}
	note, err := json.Marshal(map[string]any{"poster": poster, "release_date": date, "vote": detail.VoteAverage})
	if err != nil {
		return nil, err
	}
	metadata.Note = string(note)
	if input.Type == "TV" {
		season := -1
		if input.Season != "" {
			season, _ = strconv.Atoi(input.Season) // Already normalized by validateSubscription.
		} else {
			for _, candidate := range detail.Seasons {
				if candidate.Number > season {
					season = candidate.Number
				}
			}
		}
		for _, candidate := range detail.Seasons {
			if candidate.Number == season {
				metadata.Total = candidate.EpisodeCount
				break
			}
		}
		if metadata.Total <= 0 {
			return nil, errors.New("TMDB season has no episodes")
		}
		metadata.Season = fmt.Sprintf("S%02d", season)
		if input.TotalEpisodes != nil && *input.TotalEpisodes > 0 {
			metadata.Total = *input.TotalEpisodes
		}
		metadata.Lack = metadata.Total
		if input.CurrentEpisode != nil && *input.CurrentEpisode > 0 {
			metadata.Lack = metadata.Total - *input.CurrentEpisode - 1
			if metadata.Lack < 0 {
				metadata.Lack = 0
			}
		}
	}
	return metadata, nil
}
