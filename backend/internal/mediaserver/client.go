package mediaserver

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrConfiguration  = errors.New("invalid media server configuration")
	ErrAuthentication = errors.New("media server authentication failed")
	ErrConnection     = errors.New("media server connection failed")
	ErrResponse       = errors.New("invalid media server response")
)

type Client struct {
	kind, credential string
	endpoint         *url.URL
	http             *http.Client
}

type Counts struct {
	Movies   int64
	Series   int64
	Episodes int64
	Songs    int64
	Users    int64
}

func New(kind, host, credential string, transport http.RoundTripper) (*Client, error) {
	kind, host, credential = strings.ToLower(strings.TrimSpace(kind)), strings.TrimSpace(host), strings.TrimSpace(credential)
	if !map[string]bool{"emby": true, "jellyfin": true, "plex": true}[kind] || host == "" || credential == "" {
		return nil, ErrConfiguration
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	endpoint, err := url.Parse(host)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, ErrConfiguration
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if kind == "emby" {
		endpoint.Path += "/emby/System/Info"
	} else if kind == "jellyfin" {
		endpoint.Path += "/System/Info"
	} else if endpoint.Path == "" {
		endpoint.Path = "/"
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{kind: kind, endpoint: endpoint, credential: credential, http: &http.Client{
		Transport: transport, Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (client *Client) Check(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.endpoint.String(), nil)
	if err != nil {
		return ErrConfiguration
	}
	request.Header.Set("Accept", "application/json")
	if client.kind == "plex" {
		request.Header.Set("X-Plex-Token", client.credential)
		request.Header.Set("X-Plex-Client-Identifier", "nas-tools-go")
		request.Header.Set("X-Plex-Product", "NAS Tools")
		request.Header.Set("X-Plex-Version", "1")
	} else {
		request.Header.Set("X-Emby-Token", client.credential)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return ErrResponse
	}
	const limit = 1 << 20
	contents, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(contents) > limit {
		return ErrResponse
	}
	if client.kind != "plex" {
		var info struct {
			ID string `json:"Id"`
		}
		if json.Unmarshal(contents, &info) != nil || strings.TrimSpace(info.ID) == "" {
			return ErrResponse
		}
		return nil
	}
	var jsonInfo struct {
		MediaContainer struct {
			MachineIdentifier string `json:"machineIdentifier"`
		} `json:"MediaContainer"`
	}
	if json.Unmarshal(contents, &jsonInfo) == nil && strings.TrimSpace(jsonInfo.MediaContainer.MachineIdentifier) != "" {
		return nil
	}
	var xmlInfo struct {
		MachineIdentifier string `xml:"machineIdentifier,attr"`
	}
	if xml.Unmarshal(contents, &xmlInfo) != nil || strings.TrimSpace(xmlInfo.MachineIdentifier) == "" {
		return ErrResponse
	}
	return nil
}

func (client *Client) Counts(ctx context.Context) (Counts, error) {
	if client.kind == "plex" {
		return client.plexCounts(ctx)
	}
	base := *client.endpoint
	if client.kind == "emby" {
		base.Path = strings.TrimSuffix(base.Path, "/emby/System/Info") + "/emby"
	} else {
		base.Path = strings.TrimSuffix(base.Path, "/System/Info")
	}
	countsURL := base
	countsURL.Path += "/Items/Counts"
	var media struct {
		MovieCount   int64 `json:"MovieCount"`
		SeriesCount  int64 `json:"SeriesCount"`
		EpisodeCount int64 `json:"EpisodeCount"`
		SongCount    int64 `json:"SongCount"`
	}
	if err := client.readJSON(ctx, countsURL.String(), &media); err != nil {
		return Counts{}, err
	}
	usersURL := base
	if client.kind == "emby" {
		usersURL.Path += "/Users/Query"
		var users struct {
			TotalRecordCount int64 `json:"TotalRecordCount"`
		}
		if err := client.readJSON(ctx, usersURL.String(), &users); err != nil {
			return Counts{}, err
		}
		return Counts{Movies: media.MovieCount, Series: media.SeriesCount, Episodes: media.EpisodeCount, Songs: media.SongCount, Users: users.TotalRecordCount}, nil
	}
	usersURL.Path += "/Users"
	var users []json.RawMessage
	if err := client.readJSON(ctx, usersURL.String(), &users); err != nil {
		return Counts{}, err
	}
	return Counts{Movies: media.MovieCount, Series: media.SeriesCount, Episodes: media.EpisodeCount, Songs: media.SongCount, Users: int64(len(users))}, nil
}

type plexSection struct{ Key, Type string }

func (client *Client) plexCounts(ctx context.Context) (Counts, error) {
	contents, err := client.plexGET(ctx, "/library/sections", nil)
	if err != nil {
		return Counts{}, err
	}
	sections, err := parsePlexSections(contents)
	if err != nil {
		return Counts{}, err
	}
	counts := Counts{Users: 1}
	for _, section := range sections {
		if section.Type != "movie" && section.Type != "show" && section.Type != "artist" {
			continue
		}
		query := url.Values{"includeCollections": {"0"}, "X-Plex-Container-Start": {"0"}, "X-Plex-Container-Size": {"0"}}
		amount, err := client.plexSectionCount(ctx, section.Key, query)
		if err != nil {
			return Counts{}, err
		}
		switch section.Type {
		case "movie":
			counts.Movies += amount
		case "show":
			counts.Series += amount
			query.Set("includeCollections", "1")
			query.Set("type", "4")
			amount, err = client.plexSectionCount(ctx, section.Key, query)
			if err != nil {
				return Counts{}, err
			}
			counts.Episodes += amount
		case "artist":
			counts.Songs += amount
		}
	}
	return counts, nil
}

func (client *Client) plexSectionCount(ctx context.Context, key string, query url.Values) (int64, error) {
	if len(key) == 0 || len(key) > 12 {
		return 0, ErrResponse
	}
	for _, character := range key {
		if character < '0' || character > '9' {
			return 0, ErrResponse
		}
	}
	contents, err := client.plexGET(ctx, "/library/sections/"+key+"/all", query)
	if err != nil {
		return 0, err
	}
	if len(contents) == 0 {
		return 0, ErrResponse
	}
	if contents[0] == '{' {
		var result struct {
			MediaContainer struct {
				TotalSize *int64 `json:"totalSize"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(contents, &result) != nil || result.MediaContainer.TotalSize == nil || *result.MediaContainer.TotalSize < 0 {
			return 0, ErrResponse
		}
		return *result.MediaContainer.TotalSize, nil
	}
	var result struct {
		TotalSize string `xml:"totalSize,attr"`
	}
	if xml.Unmarshal(contents, &result) != nil {
		return 0, ErrResponse
	}
	amount, err := strconv.ParseInt(result.TotalSize, 10, 64)
	if err != nil || amount < 0 {
		return 0, ErrResponse
	}
	return amount, nil
}

func parsePlexSections(contents []byte) ([]plexSection, error) {
	if len(contents) == 0 {
		return nil, ErrResponse
	}
	sections := []plexSection{}
	if contents[0] == '{' {
		var result struct {
			MediaContainer struct {
				Directory []struct {
					Key  any    `json:"key"`
					Type string `json:"type"`
				} `json:"Directory"`
			} `json:"MediaContainer"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(contents)))
		decoder.UseNumber()
		if decoder.Decode(&result) != nil {
			return nil, ErrResponse
		}
		for _, section := range result.MediaContainer.Directory {
			sections = append(sections, plexSection{Key: strings.TrimSpace(strings.Trim(fmt.Sprint(section.Key), `"`)), Type: section.Type})
		}
		return sections, nil
	}
	var result struct {
		Directory []struct {
			Key  string `xml:"key,attr"`
			Type string `xml:"type,attr"`
		} `xml:"Directory"`
	}
	if xml.Unmarshal(contents, &result) != nil {
		return nil, ErrResponse
	}
	for _, section := range result.Directory {
		sections = append(sections, plexSection{Key: section.Key, Type: section.Type})
	}
	return sections, nil
}

func (client *Client) plexGET(ctx context.Context, path string, query url.Values) ([]byte, error) {
	endpoint := *client.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + path
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, ErrConfiguration
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Plex-Token", client.credential)
	request.Header.Set("X-Plex-Client-Identifier", "nas-tools-go")
	request.Header.Set("X-Plex-Product", "NAS Tools")
	request.Header.Set("X-Plex-Version", "1")
	response, err := client.http.Do(request)
	if err != nil {
		return nil, ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return nil, ErrResponse
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(contents) > 1<<20 {
		return nil, ErrResponse
	}
	return contents, nil
}

func (client *Client) readJSON(ctx context.Context, endpoint string, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrConfiguration
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Emby-Token", client.credential)
	response, err := client.http.Do(request)
	if err != nil {
		return ErrConnection
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrAuthentication
	}
	if response.StatusCode != http.StatusOK {
		return ErrResponse
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(contents) > 1<<20 || json.Unmarshal(contents, result) != nil {
		return ErrResponse
	}
	return nil
}
