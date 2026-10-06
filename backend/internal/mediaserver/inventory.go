package mediaserver

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

type Identity struct {
	Title, Year, TMDBID string
	TV                  bool
}

type Inventory struct {
	ItemIDs  []string
	Episodes map[int]map[int]bool
}

// Inventory queries the current server, rather than presenting a failed query
// or a stale local database entry as evidence that a download is needed.
func (client *Client) Inventory(ctx context.Context, username string, identity Identity) (Inventory, error) {
	result := Inventory{ItemIDs: []string{}, Episodes: map[int]map[int]bool{}}
	if strings.TrimSpace(identity.Title) == "" || len(identity.Title) > 4096 || len(identity.Year) > 4 || len(identity.TMDBID) > 32 {
		return result, ErrConfiguration
	}
	if client.kind == "plex" {
		return client.plexInventory(ctx, identity)
	}
	endpoint := *client.endpoint
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/System/Info")
	base := endpoint.Path
	userID := ""
	if client.kind == "jellyfin" {
		endpoint.Path = base + "/Users"
		var users []struct {
			ID     string `json:"Id"`
			Name   string
			Policy struct {
				Disabled bool `json:"IsDisabled"`
			}
		}
		if err := client.readJSON(ctx, endpoint.String(), &users); err != nil {
			return result, err
		}
		for _, user := range users {
			if !user.Policy.Disabled && (username == "" || user.Name == username) {
				userID = user.ID
				break
			}
		}
		if !validInventoryID(userID) {
			return result, ErrConfiguration
		}
		endpoint.Path = base + "/Users/" + userID + "/Items"
	} else {
		endpoint.Path = base + "/Items"
	}
	kind := "Movie"
	if identity.TV {
		kind = "Series"
	}
	query := url.Values{"IncludeItemTypes": {kind}, "SearchTerm": {identity.Title}, "Recursive": {"true"}, "Limit": {"100"}, "Fields": {"ProductionYear,ProviderIds"}, "IncludeSearchTypes": {"false"}}
	endpoint.RawQuery = query.Encode()
	var items struct {
		Items []struct {
			ID        string `json:"Id"`
			Name      string
			Year      int               `json:"ProductionYear"`
			Providers map[string]string `json:"ProviderIds"`
		}
		Total int `json:"TotalRecordCount"`
	}
	if err := client.readJSON(ctx, endpoint.String(), &items); err != nil {
		return result, err
	}
	if items.Items == nil || items.Total > 100 || len(items.Items) > 100 {
		return result, ErrResponse
	}
	for _, item := range items.Items {
		if item.Name != identity.Title || identity.Year != "" && strconv.Itoa(item.Year) != identity.Year {
			continue
		}
		providerID := ""
		for key, value := range item.Providers {
			if strings.EqualFold(key, "tmdb") {
				providerID = value
			}
		}
		if identity.TMDBID != "" && providerID != "" && providerID != identity.TMDBID {
			continue
		}
		if !validInventoryID(item.ID) {
			return result, ErrResponse
		}
		result.ItemIDs = append(result.ItemIDs, item.ID)
		if !identity.TV {
			continue
		}
		episodesURL := *client.endpoint
		episodesURL.Path = base + "/Shows/" + item.ID + "/Episodes"
		epQuery := url.Values{"IsMissing": {"false"}}
		if userID != "" {
			epQuery.Set("UserId", userID)
		}
		episodesURL.RawQuery = epQuery.Encode()
		var episodes struct {
			Items []struct {
				Season  *int `json:"ParentIndexNumber"`
				Number  *int `json:"IndexNumber"`
				End     *int `json:"IndexNumberEnd"`
				Virtual bool `json:"IsVirtualItem"`
			}
		}
		if err := client.readJSON(ctx, episodesURL.String(), &episodes); err != nil {
			return Inventory{}, err
		}
		if episodes.Items == nil || len(episodes.Items) > 10000 {
			return Inventory{}, ErrResponse
		}
		for _, episode := range episodes.Items {
			if episode.Virtual {
				continue
			}
			if episode.Season == nil || episode.Number == nil || *episode.Season < 0 || *episode.Number < 0 {
				return Inventory{}, ErrResponse
			}
			end := *episode.Number
			if episode.End != nil {
				end = *episode.End
			}
			if end < *episode.Number || end > 10000 {
				return Inventory{}, ErrResponse
			}
			if result.Episodes[*episode.Season] == nil {
				result.Episodes[*episode.Season] = map[int]bool{}
			}
			for number := *episode.Number; number <= end; number++ {
				result.Episodes[*episode.Season][number] = true
			}
		}
	}
	return result, nil
}

func validInventoryID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, letter := range value {
		if !(letter >= '0' && letter <= '9' || letter >= 'a' && letter <= 'z' || letter >= 'A' && letter <= 'Z' || letter == '-' || letter == '_') {
			return false
		}
	}
	return true
}
