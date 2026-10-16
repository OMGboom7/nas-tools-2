package mediaserver

// Snapshots are display-only inventories. Download decisions must continue to
// use Inventory, which reads current server state instead of this cache.
import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type SyncLibrary struct {
	ID   string `json:"id" xml:"key,attr"`
	Name string `json:"name" xml:"title,attr"`
	Type string `json:"type" xml:"type,attr"`
}
type SyncEpisode struct {
	Season  int `json:"season_num"`
	Episode int `json:"episode_num"`
}
type SyncItem struct {
	ID            string        `json:"id"`
	Library       string        `json:"library"`
	Type          string        `json:"type"`
	Title         string        `json:"title"`
	OriginalTitle string        `json:"originalTitle,omitempty"`
	Year          string        `json:"year,omitempty"`
	TMDBID        string        `json:"tmdbid,omitempty"`
	IMDBID        string        `json:"imdbid,omitempty"`
	Path          string        `json:"path,omitempty"`
	Episodes      []SyncEpisode `json:"episodes"`
}
type syncJSONItem struct {
	ID                                              string `json:"Id"`
	Name, Type, CollectionType, OriginalTitle, Path string
	Year                                            int               `json:"ProductionYear"`
	Providers                                       map[string]string `json:"ProviderIds"`
	Season                                          *int              `json:"ParentIndexNumber"`
	Episode                                         *int              `json:"IndexNumber"`
	End                                             *int              `json:"IndexNumberEnd"`
	Virtual                                         bool              `json:"IsVirtualItem"`
}

func (client *Client) syncUser(ctx context.Context, username string) (url.URL, string, error) {
	base := *client.endpoint
	base.Path = strings.TrimSuffix(base.Path, "/System/Info")
	endpoint := base
	endpoint.Path += "/Users"
	var users []struct {
		ID     string `json:"Id"`
		Name   string
		Policy struct {
			Disabled bool `json:"IsDisabled"`
			Admin    bool `json:"IsAdministrator"`
		}
	}
	if err := client.readJSON(ctx, endpoint.String(), &users); err != nil {
		return base, "", err
	}
	// Never silently switch to another user when an explicitly named user is
	// missing. With no configured username, use an enabled administrator.
	for _, user := range users {
		if !user.Policy.Disabled && ((username != "" && user.Name == username) || (username == "" && user.Policy.Admin)) && validInventoryID(user.ID) {
			return base, user.ID, nil
		}
	}
	return base, "", ErrConfiguration
}

func (client *Client) SyncLibraries(ctx context.Context, username string) ([]SyncLibrary, error) {
	result := []SyncLibrary{}
	if client.kind == "plex" {
		contents, err := client.plexGET(ctx, "/library/sections", nil)
		if err != nil {
			return nil, err
		}
		var sections []SyncLibrary
		if strings.HasPrefix(strings.TrimSpace(string(contents)), "{") {
			var envelope struct {
				Container *struct {
					Size  *int `json:"size"`
					Items []struct {
						Key         string `json:"key"`
						Title, Type string
					} `json:"Directory"`
				} `json:"MediaContainer"`
			}
			if json.Unmarshal(contents, &envelope) != nil || envelope.Container == nil {
				return nil, ErrResponse
			}
			c := envelope.Container
			if c.Size == nil || *c.Size != len(c.Items) {
				return nil, ErrResponse
			}
			for _, item := range c.Items {
				sections = append(sections, SyncLibrary{ID: item.Key, Name: item.Title, Type: item.Type})
			}
		} else {
			var envelope struct {
				XMLName xml.Name
				Size    *int          `xml:"size,attr"`
				Items   []SyncLibrary `xml:"Directory"`
			}
			if xml.Unmarshal(contents, &envelope) != nil || envelope.XMLName.Local != "MediaContainer" || envelope.Size == nil || *envelope.Size != len(envelope.Items) {
				return nil, ErrResponse
			}
			sections = envelope.Items
		}
		for _, item := range sections {
			if item.Type == "movie" || item.Type == "show" {
				result = append(result, item)
			}
		}
	} else {
		base, user, err := client.syncUser(ctx, username)
		if err != nil {
			return nil, err
		}
		base.Path += "/Users/" + user + "/Views"
		var envelope struct {
			Items []syncJSONItem
			Total *int `json:"TotalRecordCount"`
		}
		if err := client.readJSON(ctx, base.String(), &envelope); err != nil {
			return nil, err
		}
		if envelope.Items == nil || envelope.Total == nil || *envelope.Total != len(envelope.Items) {
			return nil, ErrResponse
		}
		for _, item := range envelope.Items {
			kind := ""
			if item.CollectionType == "movies" {
				kind = "movie"
			} else if item.CollectionType == "tvshows" {
				kind = "show"
			}
			if kind != "" {
				result = append(result, SyncLibrary{ID: item.ID, Name: item.Name, Type: kind})
			}
		}
	}
	if len(result) > 100 {
		return nil, ErrResponse
	}
	seen := map[string]bool{}
	for _, library := range result {
		if !validInventoryID(library.ID) || library.Name == "" || seen[library.ID] {
			return nil, ErrResponse
		}
		seen[library.ID] = true
	}
	return result, nil
}

// Pagination requires a stable declared total and unique IDs, so an error,
// truncated page, ignored offset or changing count cannot erase a valid cache.
func (client *Client) syncJSONPages(ctx context.Context, endpoint url.URL, query url.Values) ([]syncJSONItem, error) {
	result := []syncJSONItem{}
	seen := map[string]bool{}
	expected := -1
	for offset := 0; ; {
		query.Set("StartIndex", strconv.Itoa(offset))
		query.Set("Limit", "100")
		query.Set("EnableTotalRecordCount", "true")
		endpoint.RawQuery = query.Encode()
		var page struct {
			Items []syncJSONItem
			Total *int `json:"TotalRecordCount"`
		}
		if err := client.readJSON(ctx, endpoint.String(), &page); err != nil {
			return nil, err
		}
		if page.Items == nil || page.Total == nil || *page.Total < 0 || *page.Total > 100000 || expected >= 0 && expected != *page.Total || len(page.Items) > 100 || offset+len(page.Items) > *page.Total {
			return nil, ErrResponse
		}
		expected = *page.Total
		for _, item := range page.Items {
			if !validInventoryID(item.ID) || seen[item.ID] {
				return nil, ErrResponse
			}
			seen[item.ID] = true
		}
		result = append(result, page.Items...)
		offset += len(page.Items)
		if offset == expected {
			return result, nil
		}
		if len(page.Items) == 0 {
			return nil, ErrResponse
		}
	}
}

func (client *Client) syncPlexPages(ctx context.Context, path string, query url.Values) ([]plexInventoryItem, error) {
	result := []plexInventoryItem{}
	seen := map[string]bool{}
	expected := -1
	for offset := 0; ; {
		query.Set("X-Plex-Container-Start", strconv.Itoa(offset))
		query.Set("X-Plex-Container-Size", "100")
		contents, err := client.plexGET(ctx, path, query)
		if err != nil {
			return nil, err
		}
		items, total, err := parsePlexInventory(contents)
		if err != nil {
			return nil, err
		}
		if total == nil || *total < 0 || *total > 100000 || expected >= 0 && expected != *total || len(items) > 100 || offset+len(items) > *total {
			return nil, ErrResponse
		}
		expected = *total
		for _, item := range items {
			if !validInventoryID(item.ID) || seen[item.ID] {
				return nil, ErrResponse
			}
			seen[item.ID] = true
		}
		result = append(result, items...)
		offset += len(items)
		if offset == expected {
			return result, nil
		}
		if len(items) == 0 {
			return nil, ErrResponse
		}
	}
}

func appendSyncEpisodes(result []SyncEpisode, seen map[[2]int]bool, season, episode, end *int) ([]SyncEpisode, error) {
	if season == nil || episode == nil || *season < 0 || *season > 10000 || *episode < 0 || *episode > 10000 {
		return nil, ErrResponse
	}
	last := *episode
	if end != nil {
		last = *end
	}
	if last < *episode || last > 10000 {
		return nil, ErrResponse
	}
	for number := *episode; number <= last; number++ {
		key := [2]int{*season, number}
		if !seen[key] {
			result = append(result, SyncEpisode{Season: *season, Episode: number})
			seen[key] = true
		}
		if len(result) > 100000 {
			return nil, ErrResponse
		}
	}
	return result, nil
}

func (client *Client) CollectSnapshot(ctx context.Context, username string, selected []string) ([]SyncItem, error) {
	if len(selected) == 0 || len(selected) > 100 {
		return nil, ErrConfiguration
	}
	libraries, err := client.SyncLibraries(ctx, username)
	if err != nil {
		return nil, err
	}
	available := map[string]SyncLibrary{}
	for _, library := range libraries {
		available[library.ID] = library
	}
	chosen := map[string]bool{}
	for _, id := range selected {
		if _, ok := available[id]; !ok || chosen[id] {
			return nil, ErrConfiguration
		}
		chosen[id] = true
	}
	var base url.URL
	user := ""
	if client.kind != "plex" {
		base, user, err = client.syncUser(ctx, username)
		if err != nil {
			return nil, err
		}
	}
	result := []SyncItem{}
	seen := map[string]bool{}
	episodeCount := 0
	for _, id := range selected {
		library := available[id]
		items := []SyncItem{}
		if client.kind == "plex" {
			kind := "1"
			if library.Type == "show" {
				kind = "2"
			}
			raw, err := client.syncPlexPages(ctx, "/library/sections/"+id+"/all", url.Values{"type": {kind}, "includeGuids": {"1"}})
			if err != nil {
				return nil, err
			}
			for _, item := range raw {
				if item.Type != library.Type {
					return nil, ErrResponse
				}
				entry := SyncItem{ID: item.ID, Library: id, Title: item.Title, Type: item.Type, Episodes: []SyncEpisode{}}
				if item.Year > 0 {
					entry.Year = strconv.Itoa(item.Year)
				}
				for _, guid := range item.Guids {
					if strings.HasPrefix(guid.ID, "tmdb://") {
						entry.TMDBID = strings.TrimPrefix(guid.ID, "tmdb://")
					}
					if strings.HasPrefix(guid.ID, "imdb://") {
						entry.IMDBID = strings.TrimPrefix(guid.ID, "imdb://")
					}
				}
				if strings.HasPrefix(item.Guid, "com.plexapp.agents.themoviedb://") {
					entry.TMDBID = strings.Split(strings.TrimPrefix(item.Guid, "com.plexapp.agents.themoviedb://"), "?")[0]
				}
				items = append(items, entry)
			}
		} else {
			endpoint := base
			endpoint.Path += "/Users/" + user + "/Items"
			kind := "Movie"
			if library.Type == "show" {
				kind = "Series"
			}
			raw, err := client.syncJSONPages(ctx, endpoint, url.Values{"ParentId": {id}, "Recursive": {"true"}, "IncludeItemTypes": {kind}, "Fields": {"ProviderIds,OriginalTitle,Path,ProductionYear"}, "SortBy": {"SortName"}, "SortOrder": {"Ascending"}, "IsVirtualItem": {"false"}})
			if err != nil {
				return nil, err
			}
			for _, item := range raw {
				if item.Type != kind || item.Virtual {
					return nil, ErrResponse
				}
				entry := SyncItem{ID: item.ID, Library: id, Title: item.Name, OriginalTitle: item.OriginalTitle, Type: library.Type, TMDBID: item.Providers["Tmdb"], IMDBID: item.Providers["Imdb"], Path: item.Path, Episodes: []SyncEpisode{}}
				if item.Year > 0 {
					entry.Year = strconv.Itoa(item.Year)
				}
				items = append(items, entry)
			}
		}
		for _, item := range items {
			if seen[item.ID] || item.Title == "" {
				return nil, ErrResponse
			}
			seen[item.ID] = true
			if item.Type == "show" {
				episodeSeen := map[[2]int]bool{}
				if client.kind == "plex" {
					raw, err := client.syncPlexPages(ctx, "/library/metadata/"+item.ID+"/allLeaves", url.Values{})
					if err != nil {
						return nil, err
					}
					for _, ep := range raw {
						if ep.Type != "episode" {
							return nil, ErrResponse
						}
						item.Episodes, err = appendSyncEpisodes(item.Episodes, episodeSeen, ep.Season, ep.Episode, ep.End)
						if err != nil {
							return nil, err
						}
					}
				} else {
					endpoint := base
					endpoint.Path += "/Shows/" + item.ID + "/Episodes"
					raw, err := client.syncJSONPages(ctx, endpoint, url.Values{"UserId": {user}, "IsMissing": {"false"}, "IsVirtualItem": {"false"}})
					if err != nil {
						return nil, err
					}
					for _, ep := range raw {
						if ep.Type != "Episode" || ep.Virtual {
							return nil, ErrResponse
						}
						item.Episodes, err = appendSyncEpisodes(item.Episodes, episodeSeen, ep.Season, ep.Episode, ep.End)
						if err != nil {
							return nil, err
						}
					}
				}
				sort.Slice(item.Episodes, func(i, j int) bool {
					a, b := item.Episodes[i], item.Episodes[j]
					return a.Season < b.Season || a.Season == b.Season && a.Episode < b.Episode
				})
			}
			episodeCount += len(item.Episodes)
			result = append(result, item)
			if len(result) > 100000 || episodeCount > 1000000 {
				return nil, ErrResponse
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
