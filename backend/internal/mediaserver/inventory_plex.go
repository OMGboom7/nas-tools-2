package mediaserver

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/url"
	"strconv"
	"strings"
)

type plexInventoryItem struct {
	ID      string `json:"ratingKey" xml:"ratingKey,attr"`
	Title   string `json:"title" xml:"title,attr"`
	Type    string `json:"type" xml:"type,attr"`
	Year    int    `json:"year" xml:"year,attr"`
	Season  *int   `json:"parentIndex" xml:"parentIndex,attr"`
	Episode *int   `json:"index" xml:"index,attr"`
	End     *int   `json:"indexEnd" xml:"indexEnd,attr"`
	Guid    string `json:"guid" xml:"guid,attr"`
	Guids   []struct {
		ID string `json:"id" xml:"id,attr"`
	} `json:"Guid" xml:"Guid"`
}

func parsePlexInventory(contents []byte) ([]plexInventoryItem, *int, error) {
	contents = []byte(strings.TrimSpace(string(contents)))
	if len(contents) == 0 {
		return nil, nil, ErrResponse
	}
	if contents[0] == '{' {
		var payload struct {
			Container *struct {
				Items []plexInventoryItem `json:"Metadata"`
				Size  *int                `json:"size"`
				Total *int                `json:"totalSize"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(contents, &payload) != nil || payload.Container == nil {
			return nil, nil, ErrResponse
		}
		container := payload.Container
		if container.Items == nil && (container.Size == nil || *container.Size != 0) {
			return nil, nil, ErrResponse
		}
		if container.Size != nil && *container.Size != len(container.Items) {
			return nil, nil, ErrResponse
		}
		return container.Items, container.Total, nil
	}
	var container struct {
		XMLName     xml.Name
		Size        *int                `xml:"size,attr"`
		Total       *int                `xml:"totalSize,attr"`
		Directories []plexInventoryItem `xml:"Directory"`
		Videos      []plexInventoryItem `xml:"Video"`
	}
	if xml.Unmarshal(contents, &container) != nil || container.XMLName.Local != "MediaContainer" {
		return nil, nil, ErrResponse
	}
	items := append(container.Directories, container.Videos...)
	if len(items) == 0 && (container.Size == nil || *container.Size != 0) {
		return nil, nil, ErrResponse
	}
	if container.Size != nil && *container.Size != len(items) {
		return nil, nil, ErrResponse
	}
	return items, container.Total, nil
}

func (client *Client) plexInventoryItems(ctx context.Context, path string, query url.Values) ([]plexInventoryItem, error) {
	result := []plexInventoryItem{}
	for offset := 0; offset < 10000; {
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
		if len(items) > 100 || total != nil && (*total < 0 || *total > 10000 || *total < offset+len(items)) {
			return nil, ErrResponse
		}
		result = append(result, items...)
		offset += len(items)
		if total != nil && offset == *total {
			return result, nil
		}
		if len(items) == 0 {
			if total != nil && offset < *total {
				return nil, ErrResponse
			}
			return result, nil
		}
		if total == nil && len(items) < 100 {
			return result, nil
		}
	}
	return nil, ErrResponse
}

func (client *Client) plexInventory(ctx context.Context, identity Identity) (Inventory, error) {
	result := Inventory{ItemIDs: []string{}, Episodes: map[int]map[int]bool{}}
	contents, err := client.plexGET(ctx, "/library/sections", nil)
	if err != nil {
		return result, err
	}
	// Validate the envelope before the shared parser: an error object is not
	// evidence that the server has no libraries.
	if strings.HasPrefix(strings.TrimSpace(string(contents)), "{") {
		var envelope struct {
			Container *struct {
				Directories []json.RawMessage `json:"Directory"`
				Size        *int              `json:"size"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(contents, &envelope) != nil || envelope.Container == nil || envelope.Container.Directories == nil && (envelope.Container.Size == nil || *envelope.Container.Size != 0) {
			return result, ErrResponse
		}
	} else {
		var envelope struct {
			XMLName     xml.Name
			Size        *int       `xml:"size,attr"`
			Directories []struct{} `xml:"Directory"`
		}
		if xml.Unmarshal(contents, &envelope) != nil || envelope.XMLName.Local != "MediaContainer" || len(envelope.Directories) == 0 && (envelope.Size == nil || *envelope.Size != 0) {
			return result, ErrResponse
		}
	}
	sections, err := parsePlexSections(contents)
	if err != nil {
		return result, err
	}
	if len(sections) > 100 {
		return result, ErrResponse
	}
	kind, typeNumber := "movie", "1"
	if identity.TV {
		kind, typeNumber = "show", "2"
	}
	for _, section := range sections {
		if section.Type != kind {
			continue
		}
		if !validInventoryID(section.Key) {
			return result, ErrResponse
		}
		query := url.Values{"type": {typeNumber}, "title": {identity.Title}, "includeGuids": {"1"}}
		if identity.Year != "" {
			query.Set("year", identity.Year)
		}
		items, err := client.plexInventoryItems(ctx, "/library/sections/"+section.Key+"/all", query)
		if err != nil {
			return Inventory{}, err
		}
		for _, item := range items {
			if item.Type != kind || item.Title != identity.Title || identity.Year != "" && strconv.Itoa(item.Year) != identity.Year {
				continue
			}
			providerID := ""
			for _, guid := range item.Guids {
				if strings.HasPrefix(guid.ID, "tmdb://") {
					providerID = strings.TrimPrefix(guid.ID, "tmdb://")
				}
			}
			if strings.HasPrefix(item.Guid, "com.plexapp.agents.themoviedb://") {
				providerID = strings.Split(strings.TrimPrefix(item.Guid, "com.plexapp.agents.themoviedb://"), "?")[0]
			}
			if identity.TMDBID != "" && providerID != "" && providerID != identity.TMDBID {
				continue
			}
			if !validInventoryID(item.ID) {
				return Inventory{}, ErrResponse
			}
			result.ItemIDs = append(result.ItemIDs, item.ID)
			if !identity.TV {
				continue
			}
			episodes, err := client.plexInventoryItems(ctx, "/library/metadata/"+item.ID+"/allLeaves", url.Values{})
			if err != nil {
				return Inventory{}, err
			}
			for _, episode := range episodes {
				if episode.Type != "episode" || episode.Season == nil || episode.Episode == nil || *episode.Season < 0 || *episode.Episode < 0 {
					return Inventory{}, ErrResponse
				}
				end := *episode.Episode
				if episode.End != nil {
					end = *episode.End
				}
				if end < *episode.Episode || end > 10000 {
					return Inventory{}, ErrResponse
				}
				if result.Episodes[*episode.Season] == nil {
					result.Episodes[*episode.Season] = map[int]bool{}
				}
				for number := *episode.Episode; number <= end; number++ {
					result.Episodes[*episode.Season][number] = true
				}
			}
		}
	}
	return result, nil
}
