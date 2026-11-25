package builtinindexer

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MTeamFeedOrigin accepts existing sign-in URLs while keeping the API key
// bound to the same supported M-Team domains as the native search provider.
func MTeamFeedOrigin(signURL string) (string, bool) {
	u, err := url.Parse(signURL)
	if err != nil || u.User != nil {
		return "", false
	}
	origin := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	_, _, err = mteamBase(origin)
	return origin, err == nil
}

type MTeamFeedItem struct {
	freeUntil   time.Time
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Link        string `xml:"link"`
	Author      string `xml:"author"`
	PubDate     string `xml:"pubDate"`
	GUID        struct {
		Permalink bool   `xml:"isPermaLink,attr"`
		Value     string `xml:",chardata"`
	} `xml:"guid"`
	Enclosure struct {
		Length int64  `xml:"length,attr"`
		Type   string `xml:"type,attr"`
		URL    string `xml:"url,attr"`
	} `xml:"enclosure"`
	Category string `xml:"category"`
}

func FetchMTeamFeedCategory(ctx context.Context, origin, key, ua, mode string, minimumBytes int64, now time.Time, transport http.RoundTripper) ([]MTeamFeedItem, error) {
	switch mode {
	case "adult", "normal", "movie", "tvshow":
	default:
		return nil, ErrConfig
	}
	front, api, err := mteamBase(origin)
	if err != nil || minimumBytes < 0 {
		return nil, ErrConfig
	}
	body, _ := json.Marshal(map[string]any{"pageNumber": 1, "pageSize": 100, "keyword": "", "visible": 1, "mode": mode})
	data, err := mteamRequest(ctx, api, "/api/torrent/search", key, ua, "application/json", body, transport)
	if err != nil {
		return nil, err
	}
	var page struct {
		Data []struct {
			ID, Size, Category json.RawMessage
			Name, CreatedDate  string
			Status             *struct {
				Discount       string
				MallSingleFree json.RawMessage
			}
		}
	}
	if json.Unmarshal(data, &page) != nil || page.Data == nil || len(page.Data) > 100 {
		return nil, ErrResponse
	}
	var items []MTeamFeedItem
	zone := time.FixedZone("CST", 8*3600)
	for _, row := range page.Data {
		size, err := mteamInteger(row.Size, true)
		if err != nil {
			return nil, err
		}
		if *size <= minimumBytes {
			continue
		}
		if row.Status == nil {
			continue
		}
		var freeUntil time.Time
		if row.Status.Discount != "FREE" && row.Status.Discount != "_2X_FREE" {
			var free struct{ EndDate string }
			if json.Unmarshal(row.Status.MallSingleFree, &free) != nil || free.EndDate == "" {
				continue
			}
			end, err := time.ParseInLocation(time.DateTime, free.EndDate, now.Location())
			if err != nil || now.After(end) {
				continue
			}
			freeUntil = end
		}
		id, err := mteamInteger(row.ID, true)
		if err != nil || *id < 1 || strings.TrimSpace(row.Name) == "" || len(row.Name) > 4096 {
			return nil, ErrResponse
		}
		date, err := time.ParseInLocation(time.DateTime, row.CreatedDate, zone)
		if err != nil {
			return nil, ErrResponse
		}
		item := MTeamFeedItem{freeUntil: freeUntil, Title: row.Name, Link: front.JoinPath("detail", strconv.FormatInt(*id, 10)).String(), Author: "anonymous@kp.m-team.cc (anonymous)", PubDate: date.Format(time.RFC1123Z)}
		if len(row.Category) > 0 && string(row.Category) != "null" && json.Unmarshal(row.Category, &item.Category) != nil {
			category, err := mteamInteger(row.Category, true)
			if err != nil {
				return nil, err
			}
			item.Category = strconv.FormatInt(*category, 10)
		}
		item.Enclosure.Length = *size
		item.Enclosure.Type = "application/x-bittorrent"
		item.Enclosure.URL = item.Link
		item.GUID.Value = item.Link
		items = append(items, item)
	}
	return items, nil
}

func EncodeMTeamFeed(items []MTeamFeedItem, now time.Time) ([]byte, error) {
	const title = "M-Team - TP Torrents"
	const link = "https://kp.m-team.cc"
	var rss struct {
		XMLName xml.Name `xml:"rss"`
		Version string   `xml:"version,attr"`
		Channel struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			Description string `xml:"description"`
			Image       struct {
				Title string `xml:"title"`
				Link  string `xml:"link"`
				URL   string `xml:"url"`
			} `xml:"image"`
			Language  string          `xml:"language"`
			Copyright string          `xml:"copyright"`
			Editor    string          `xml:"managingEditor"`
			Webmaster string          `xml:"webMaster"`
			Generator string          `xml:"generator"`
			TTL       int             `xml:"ttl"`
			Built     string          `xml:"lastBuildDate"`
			Items     []MTeamFeedItem `xml:"item"`
		} `xml:"channel"`
	}
	rss.Version = "2.0"
	c := &rss.Channel
	c.Title, c.Link, c.Description = title, link, "Latest torrents from M-Team - TP - Working"
	c.Image.Title, c.Image.Link, c.Image.URL = title, link, link+"/pic/rss_logo.jpg"
	c.Language = "zh-tw"
	c.Copyright = "Copyright (c) M-Team - TP 2012-2023, all rights reserved"
	c.Editor, c.Webmaster = "webmaster@m-team.cc (M-Team - TP Admin)", "webmaster@m-team.cc (M-Team - TP Webmaster)"
	c.Generator, c.TTL, c.Built = "NexusPHP RSS Generator", 60, now.Format(time.RFC1123Z)
	// A timed discount can expire while other categories are being fetched.
	seen := map[string]bool{}
	for _, item := range items {
		if (!item.freeUntil.IsZero() && now.After(item.freeUntil)) || seen[item.Link] {
			continue
		}
		seen[item.Link] = true
		c.Items = append(c.Items, item)
	}
	b, err := xml.Marshal(rss)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), b...), nil
}
