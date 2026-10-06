package mediaserver

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

type ResumeItem struct {
	LibraryItem
	Percent float64
}

func (client *Client) Resume(ctx context.Context, username, playHost string, limit int) ([]ResumeItem, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrConfiguration
	}
	if client.kind == "plex" {
		return client.plexResume(ctx, playHost, limit)
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
	resumeURL := base
	resumeURL.Path += "/Users/" + url.PathEscape(userID) + "/Items/Resume"
	resumeURL.RawQuery = url.Values{"Limit": {strconv.Itoa(limit)}, "MediaTypes": {"Video"}}.Encode()
	var result struct {
		Items []struct {
			ID                    string   `json:"Id"`
			Name                  string   `json:"Name"`
			Type                  string   `json:"Type"`
			SeriesName            string   `json:"SeriesName"`
			SeriesID              string   `json:"SeriesId"`
			Season                int      `json:"ParentIndexNumber"`
			Episode               int      `json:"IndexNumber"`
			BackdropTags          []string `json:"BackdropImageTags"`
			SeriesPrimaryImageTag string   `json:"SeriesPrimaryImageTag"`
			UserData              struct {
				PlayedPercentage float64 `json:"PlayedPercentage"`
			} `json:"UserData"`
		} `json:"Items"`
	}
	if err := client.readJSON(ctx, resumeURL.String(), &result); err != nil {
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
	items := make([]ResumeItem, 0, len(result.Items))
	for _, raw := range result.Items {
		if raw.ID == "" || raw.Type != "Movie" && raw.Type != "Episode" {
			continue
		}
		kind, title := "电影", raw.Name
		if raw.Type == "Episode" {
			kind = "电视剧"
			if raw.Season == 1 {
				title = raw.SeriesName + " 第" + strconv.Itoa(raw.Episode) + "集"
			} else {
				title = raw.SeriesName + " 第" + strconv.Itoa(raw.Season) + "季第" + strconv.Itoa(raw.Episode) + "集"
			}
		}
		link := playBase + "item?id=" + url.QueryEscape(raw.ID) + "&context=home&serverId=" + url.QueryEscape(info.ID)
		if client.kind == "jellyfin" {
			link = playBase + "details?id=" + url.QueryEscape(raw.ID) + "&serverId=" + url.QueryEscape(info.ID)
		}
		imageID := raw.ID
		imageType, imageTag := "Primary", ""
		if client.kind == "emby" && raw.Type == "Episode" {
			if raw.SeriesID != "" {
				imageID = raw.SeriesID
			}
			if raw.SeriesPrimaryImageTag != "" {
				imageType, imageTag = "Backdrop", raw.SeriesPrimaryImageTag
			}
		} else if len(raw.BackdropTags) > 0 && raw.BackdropTags[0] != "" {
			imageType, imageTag = "Backdrop", raw.BackdropTags[0]
		}
		imageURL := base
		imageURL.Path += "/Items/" + url.PathEscape(imageID) + "/Images/" + imageType
		if imageTag != "" {
			imageURL.RawQuery = url.Values{"tag": {imageTag}, "fillWidth": {"666"}}.Encode()
		}
		items = append(items, ResumeItem{LibraryItem: LibraryItem{ID: raw.ID, Name: title, Type: kind, Image: imageURL.String(), Link: link}, Percent: raw.UserData.PlayedPercentage})
		if len(items) == limit {
			break
		}
	}
	return items, nil
}
