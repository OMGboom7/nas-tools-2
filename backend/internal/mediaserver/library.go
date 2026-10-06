package mediaserver

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type LibraryItem struct {
	ID    string
	Name  string
	Type  string
	Image string
	Link  string
}

func (client *Client) Latest(ctx context.Context, username, playHost string, limit int) ([]LibraryItem, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrConfiguration
	}
	if client.kind == "plex" {
		return client.plexLatest(ctx, playHost, limit)
	}
	base := *client.endpoint
	if client.kind == "emby" {
		base.Path = strings.TrimSuffix(base.Path, "/emby/System/Info")
	} else {
		base.Path = strings.TrimSuffix(base.Path, "/System/Info")
	}
	usersURL := base
	usersURL.Path += "/Users"
	var users []struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Policy struct {
			IsAdministrator bool `json:"IsAdministrator"`
		} `json:"Policy"`
	}
	if err := client.readJSON(ctx, usersURL.String(), &users); err != nil {
		return nil, err
	}
	userID := ""
	for _, user := range users {
		if username != "" && user.Name == username {
			userID = user.ID
			break
		}
	}
	if userID == "" {
		for _, user := range users {
			if user.Policy.IsAdministrator {
				userID = user.ID
				break
			}
		}
	}
	if userID == "" || strings.ContainsAny(userID, "/?#") {
		return nil, ErrResponse
	}
	latestURL := base
	latestURL.Path += "/Users/" + url.PathEscape(userID) + "/Items/Latest"
	latestURL.RawQuery = url.Values{"Limit": {strconv.Itoa(limit)}, "MediaTypes": {"Video"}}.Encode()
	var raw []struct {
		ID   string `json:"Id"`
		Name string `json:"Name"`
		Type string `json:"Type"`
	}
	if err := client.readJSON(ctx, latestURL.String(), &raw); err != nil {
		return nil, err
	}
	var info struct {
		ID string `json:"Id"`
	}
	if err := client.readJSON(ctx, client.endpoint.String(), &info); err != nil || info.ID == "" {
		return nil, ErrResponse
	}
	if playHost == "" {
		playHost = base.Scheme + "://" + base.Host + base.Path
	}
	if !strings.Contains(playHost, "://") {
		playHost = "http://" + playHost
	}
	playURL, err := url.Parse(playHost)
	if err != nil || playURL.Host == "" || playURL.User != nil || playURL.RawQuery != "" || playURL.Fragment != "" || playURL.Scheme != "http" && playURL.Scheme != "https" {
		return nil, ErrConfiguration
	}
	playBase := strings.TrimRight(playURL.String(), "/") + "/web/index.html#!/"
	items := make([]LibraryItem, 0, len(raw))
	for _, item := range raw {
		if item.Type != "Movie" && item.Type != "Series" || item.ID == "" {
			continue
		}
		kind := "电影"
		link := playBase + "item?id=" + url.QueryEscape(item.ID) + "&context=home&serverId=" + url.QueryEscape(info.ID)
		if client.kind == "jellyfin" {
			link = playBase + "details?id=" + url.QueryEscape(item.ID) + "&serverId=" + url.QueryEscape(info.ID)
		}
		if item.Type == "Series" {
			kind = "电视剧"
		}
		imageURL := base
		imageURL.Path += "/Items/" + url.PathEscape(item.ID) + "/Images/Primary"
		items = append(items, LibraryItem{ID: item.ID, Name: item.Name, Type: kind, Image: imageURL.String(), Link: link})
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

type plexLibraryMetadata struct {
	Key, Type, Title, ParentTitle, Index, Thumb, ParentThumb, GrandparentThumb string
}

func (client *Client) plexLatest(ctx context.Context, playHost string, limit int) ([]LibraryItem, error) {
	query := url.Values{"X-Plex-Container-Start": {"0"}, "X-Plex-Container-Size": {strconv.Itoa(limit)}}
	contents, err := client.plexGET(ctx, "/library/recentlyAdded", query)
	if err != nil {
		return nil, err
	}
	metadata, err := parsePlexLibraryMetadata(contents)
	if err != nil {
		return nil, err
	}
	identity, err := client.plexGET(ctx, "/", nil)
	if err != nil {
		return nil, err
	}
	machineID, err := parsePlexMachineID(identity)
	if err != nil {
		return nil, err
	}
	if playHost == "" {
		playHost = client.endpoint.Scheme + "://" + client.endpoint.Host + strings.TrimRight(client.endpoint.Path, "/")
	}
	if !strings.Contains(playHost, "://") {
		playHost = "http://" + playHost
	}
	playURL, err := url.Parse(playHost)
	if err != nil || playURL.Host == "" || playURL.User != nil || playURL.RawQuery != "" || playURL.Fragment != "" || playURL.Scheme != "http" && playURL.Scheme != "https" {
		return nil, ErrConfiguration
	}
	playBase := strings.TrimRight(playURL.String(), "/") + "/web/index.html"
	if strings.Contains(playURL.Hostname(), "app.plex.tv") {
		playBase = strings.TrimRight(playURL.String(), "/") + "/desktop/"
	}
	items := make([]LibraryItem, 0, len(metadata))
	for _, item := range metadata {
		if item.Key == "" {
			continue
		}
		name := item.Title
		kind := "电影"
		if item.Type != "movie" {
			kind = "电视剧"
			if item.ParentTitle != "" && item.Index != "" {
				name = item.ParentTitle + " 第" + item.Index + "季"
			}
		}
		imagePath := item.Thumb
		if imagePath == "" {
			imagePath = item.ParentThumb
		}
		if imagePath == "" {
			imagePath = item.GrandparentThumb
		}
		image := ""
		if strings.HasPrefix(imagePath, "/") && !strings.HasPrefix(imagePath, "//") {
			endpoint := *client.endpoint
			endpoint.Path = strings.TrimRight(endpoint.Path, "/") + imagePath
			image = endpoint.String()
		}
		link := playBase + "#!/server/" + url.PathEscape(machineID) + "/details?key=" + url.QueryEscape(item.Key)
		items = append(items, LibraryItem{ID: item.Key, Name: name, Type: kind, Image: image, Link: link})
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func parsePlexLibraryMetadata(contents []byte) ([]plexLibraryMetadata, error) {
	if len(contents) == 0 {
		return nil, ErrResponse
	}
	items := []plexLibraryMetadata{}
	if contents[0] == '{' {
		var result struct {
			MediaContainer struct {
				Metadata []struct {
					Key              string `json:"key"`
					Type             string `json:"type"`
					Title            string `json:"title"`
					ParentTitle      string `json:"parentTitle"`
					Index            any    `json:"index"`
					Thumb            string `json:"thumb"`
					ParentThumb      string `json:"parentThumb"`
					GrandparentThumb string `json:"grandparentThumb"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(contents)))
		decoder.UseNumber()
		if decoder.Decode(&result) != nil {
			return nil, ErrResponse
		}
		for _, raw := range result.MediaContainer.Metadata {
			index := ""
			if raw.Index != nil {
				index = fmt.Sprint(raw.Index)
			}
			items = append(items, plexLibraryMetadata{raw.Key, raw.Type, raw.Title, raw.ParentTitle, index, raw.Thumb, raw.ParentThumb, raw.GrandparentThumb})
		}
		return items, nil
	}
	var result struct {
		Metadata []struct {
			Key              string `xml:"key,attr"`
			Type             string `xml:"type,attr"`
			Title            string `xml:"title,attr"`
			ParentTitle      string `xml:"parentTitle,attr"`
			Index            string `xml:"index,attr"`
			Thumb            string `xml:"thumb,attr"`
			ParentThumb      string `xml:"parentThumb,attr"`
			GrandparentThumb string `xml:"grandparentThumb,attr"`
		} `xml:"Metadata"`
	}
	if xml.Unmarshal(contents, &result) != nil {
		return nil, ErrResponse
	}
	for _, raw := range result.Metadata {
		items = append(items, plexLibraryMetadata{raw.Key, raw.Type, raw.Title, raw.ParentTitle, raw.Index, raw.Thumb, raw.ParentThumb, raw.GrandparentThumb})
	}
	return items, nil
}

func parsePlexMachineID(contents []byte) (string, error) {
	if len(contents) == 0 {
		return "", ErrResponse
	}
	identity := ""
	if contents[0] == '{' {
		var result struct {
			MediaContainer struct {
				MachineIdentifier string `json:"machineIdentifier"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(contents, &result) != nil {
			return "", ErrResponse
		}
		identity = result.MediaContainer.MachineIdentifier
	} else {
		var result struct {
			MachineIdentifier string `xml:"machineIdentifier,attr"`
		}
		if xml.Unmarshal(contents, &result) != nil {
			return "", ErrResponse
		}
		identity = result.MachineIdentifier
	}
	if identity == "" || len(identity) > 256 || strings.ContainsAny(identity, "/?#") {
		return "", ErrResponse
	}
	return identity, nil
}
