package mediaserver

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

type plexResumeMetadata struct {
	Key, Type, Title, GrandparentTitle, Index, ParentIndex string
	Art, ParentArt, GrandparentArt                         string
	ViewOffset, Duration                                   float64
}

func (client *Client) plexResume(ctx context.Context, playHost string, limit int) ([]ResumeItem, error) {
	query := url.Values{"X-Plex-Container-Start": {"0"}, "X-Plex-Container-Size": {strconv.Itoa(limit)}}
	contents, err := client.plexGET(ctx, "/hubs/continueWatching/items", query)
	if err != nil {
		return nil, err
	}
	metadata, err := parsePlexResumeMetadata(contents)
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
	items := make([]ResumeItem, 0, len(metadata))
	for _, item := range metadata {
		if item.Key == "" {
			continue
		}
		kind, name := "电影", item.Title
		if item.Type != "movie" {
			kind = "电视剧"
			if item.GrandparentTitle != "" && item.Index != "" {
				if item.ParentIndex == "1" {
					name = item.GrandparentTitle + " 第" + item.Index + "集"
				} else if item.ParentIndex != "" {
					name = item.GrandparentTitle + " 第" + item.ParentIndex + "季第" + item.Index + "集"
				}
			}
		}
		imagePath := item.Art
		if imagePath == "" {
			imagePath = item.ParentArt
		}
		if imagePath == "" {
			imagePath = item.GrandparentArt
		}
		image := ""
		if strings.HasPrefix(imagePath, "/") && !strings.HasPrefix(imagePath, "//") {
			endpoint := *client.endpoint
			endpoint.Path = strings.TrimRight(endpoint.Path, "/") + imagePath
			image = endpoint.String()
		}
		percent := float64(0)
		if item.ViewOffset > 0 && item.Duration > 0 {
			percent = item.ViewOffset / item.Duration * 100
		}
		link := playBase + "#!/server/" + url.PathEscape(machineID) + "/details?key=" + url.QueryEscape(item.Key)
		items = append(items, ResumeItem{LibraryItem: LibraryItem{ID: item.Key, Name: name, Type: kind, Image: image, Link: link}, Percent: percent})
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func parsePlexResumeMetadata(contents []byte) ([]plexResumeMetadata, error) {
	if len(contents) == 0 {
		return nil, ErrResponse
	}
	items := []plexResumeMetadata{}
	if contents[0] == '{' {
		var result struct {
			MediaContainer struct {
				Metadata []struct {
					Key              string `json:"key"`
					Type             string `json:"type"`
					Title            string `json:"title"`
					GrandparentTitle string `json:"grandparentTitle"`
					Index            any    `json:"index"`
					ParentIndex      any    `json:"parentIndex"`
					Art              string `json:"art"`
					ParentArt        string `json:"parentArt"`
					GrandparentArt   string `json:"grandparentArt"`
					ViewOffset       any    `json:"viewOffset"`
					Duration         any    `json:"duration"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(contents)))
		decoder.UseNumber()
		if decoder.Decode(&result) != nil {
			return nil, ErrResponse
		}
		for _, raw := range result.MediaContainer.Metadata {
			items = append(items, plexResumeMetadata{
				Key: raw.Key, Type: raw.Type, Title: raw.Title, GrandparentTitle: raw.GrandparentTitle,
				Index: plexValueText(raw.Index), ParentIndex: plexValueText(raw.ParentIndex),
				Art: raw.Art, ParentArt: raw.ParentArt, GrandparentArt: raw.GrandparentArt,
				ViewOffset: plexValueNumber(raw.ViewOffset), Duration: plexValueNumber(raw.Duration),
			})
		}
		return items, nil
	}
	var result struct {
		Metadata []struct {
			Key              string `xml:"key,attr"`
			Type             string `xml:"type,attr"`
			Title            string `xml:"title,attr"`
			GrandparentTitle string `xml:"grandparentTitle,attr"`
			Index            string `xml:"index,attr"`
			ParentIndex      string `xml:"parentIndex,attr"`
			Art              string `xml:"art,attr"`
			ParentArt        string `xml:"parentArt,attr"`
			GrandparentArt   string `xml:"grandparentArt,attr"`
			ViewOffset       string `xml:"viewOffset,attr"`
			Duration         string `xml:"duration,attr"`
		} `xml:"Metadata"`
	}
	if xml.Unmarshal(contents, &result) != nil {
		return nil, ErrResponse
	}
	for _, raw := range result.Metadata {
		items = append(items, plexResumeMetadata{
			Key: raw.Key, Type: raw.Type, Title: raw.Title, GrandparentTitle: raw.GrandparentTitle,
			Index: raw.Index, ParentIndex: raw.ParentIndex, Art: raw.Art,
			ParentArt: raw.ParentArt, GrandparentArt: raw.GrandparentArt,
			ViewOffset: plexValueNumber(raw.ViewOffset), Duration: plexValueNumber(raw.Duration),
		})
	}
	return items, nil
}

func plexValueText(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func plexValueNumber(value any) float64 {
	amount, _ := strconv.ParseFloat(plexValueText(value), 64)
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0
	}
	return amount
}
