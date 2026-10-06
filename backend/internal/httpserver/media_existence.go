package httpserver

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/0xforee/nas-tools/backend/internal/mediameta"
	"github.com/0xforee/nas-tools/backend/internal/mediaserver"
)

var errLocalMediaUnavailable = errors.New("local media directories are not configured")

func (service subscriptionService) nativeMediaExistence(ctx context.Context, meta mediameta.Metadata, detail tmdbMediaDetails, kind string, category ...string) (mediaserver.Coverage, error) {
	if err := ctx.Err(); err != nil {
		return mediaserver.Coverage{}, err
	}
	if service.configStore == nil || service.client == nil || detail.ID <= 0 || kind != "movie" && kind != "tv" {
		return mediaserver.Coverage{}, mediaserver.ErrConfiguration
	}
	snapshot, err := service.configStore.Snapshot()
	if err != nil {
		return mediaserver.Coverage{}, err
	}
	serverKind := strings.ToLower(strings.TrimSpace(text(objectValue(snapshot["media"])["media_server"])))
	config := objectValue(snapshot[serverKind])
	credential := text(config["api_key"])
	if serverKind == "plex" {
		credential = text(config["token"])
	}
	identity := mediaserver.Identity{Title: detail.Title, Year: meta.Year, TMDBID: strconv.FormatInt(detail.ID, 10), TV: kind == "tv"}
	date := detail.ReleaseDate
	if identity.TV {
		identity.Title, date = detail.Name, detail.FirstAirDate
	}
	if len(date) >= 4 {
		identity.Year = date[:4]
	}
	var inventory mediaserver.Inventory
	if serverKind == "" {
		inventory, err = service.nativeLocalMediaInventory(ctx, objectValue(snapshot["media"]), meta, detail, identity, category)
	} else {
		var client *mediaserver.Client
		client, err = mediaserver.New(serverKind, text(config["host"]), credential, service.client.Transport)
		if err == nil {
			inventory, err = client.Inventory(ctx, text(config["username"]), identity)
		}
		if err != nil {
			// A cancelled request must not start another lookup. An empty but
			// valid server inventory is authoritative and does not fall back.
			if contextErr := ctx.Err(); contextErr != nil {
				return mediaserver.Coverage{}, contextErr
			}
			serverErr := err
			inventory, err = service.nativeLocalMediaInventory(ctx, objectValue(snapshot["media"]), meta, detail, identity, category)
			if errors.Is(err, errLocalMediaUnavailable) {
				return mediaserver.Coverage{}, serverErr
			}
			if err != nil {
				return mediaserver.Coverage{}, errors.Join(serverErr, err)
			}
		}
	}
	if err != nil {
		return mediaserver.Coverage{}, err
	}
	selection := mediaserver.CoverageSelection{TV: identity.TV, SeasonTotals: map[int]int{}}
	if identity.TV {
		for _, season := range detail.Seasons {
			if season.Episodes != nil {
				if _, duplicate := selection.SeasonTotals[season.Number]; duplicate {
					return mediaserver.Coverage{}, mediaserver.ErrResponse
				}
				selection.SeasonTotals[season.Number] = *season.Episodes
			}
		}
		if meta.Episodes.Season != nil {
			begin, end := *meta.Episodes.Season, *meta.Episodes.Season
			if meta.Episodes.EndSeason != nil {
				end = *meta.Episodes.EndSeason
			}
			if begin < 0 || end < begin || end-begin > 1000 {
				return mediaserver.Coverage{}, mediaserver.ErrConfiguration
			}
			for season := begin; season <= end; season++ {
				selection.Seasons = append(selection.Seasons, season)
			}
		}
		if meta.Episodes.Episode != nil {
			begin, end := *meta.Episodes.Episode, *meta.Episodes.Episode
			if meta.Episodes.EndEpisode != nil {
				end = *meta.Episodes.EndEpisode
			}
			if begin < 0 || end < begin || end > 10000 {
				return mediaserver.Coverage{}, mediaserver.ErrConfiguration
			}
			for episode := begin; episode <= end; episode++ {
				selection.Episodes = append(selection.Episodes, episode)
			}
		}
	}
	return inventory.Coverage(selection)
}
