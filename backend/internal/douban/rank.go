package douban

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html/charset"
)

const RankPlugin = "DoubanRank"

type RankSource struct{ ID, Name, Path string }

var RankSources = []RankSource{
	{"movie-ustop", "北美电影票房榜", "ustop"},
	{"movie-weekly", "一周电影口碑榜", "weekly"},
	{"movie-real-time", "实时热门榜", "weekly/subject_real_time_hotest"},
	{"show-domestic", "热门综艺", "weekly/show_domestic"},
	{"movie-hot-gaia", "热门电影", "weekly/movie_hot_gaia"},
	{"tv-hot", "热门电视剧", "weekly/tv_hot"},
	{"movie-top250", "豆瓣电影 Top 250", "weekly/movie_top250"},
}

type RankSettings struct {
	Enabled, Once    bool
	Cron             string
	Ranks, Addresses []string
	Year             int
	Vote             float64
}

func ReadRankSettings(raw string) (RankSettings, error) {
	var s RankSettings
	if raw == "" {
		return s, nil
	}
	var v map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &v) != nil || v == nil {
		return s, ErrSettings
	}
	for k, dst := range map[string]any{"enable": &s.Enabled, "onlyonce": &s.Once, "cron": &s.Cron} {
		if b := v[k]; b != nil && json.Unmarshal(b, dst) != nil {
			return s, ErrSettings
		}
	}
	for k, dst := range map[string]*[]string{"rss_addrs": &s.Addresses, "ranks": &s.Ranks} {
		b := v[k]
		if b == nil {
			continue
		}
		var list []string
		if json.Unmarshal(b, &list) != nil {
			var str string
			if json.Unmarshal(b, &str) != nil {
				return s, ErrSettings
			}
			sep := "\n"
			if k == "ranks" {
				sep = ","
			}
			list = strings.Split(str, sep)
		}
		for _, value := range list {
			value = strings.TrimSpace(value)
			if value == "" || slices.Contains(*dst, value) {
				continue
			}
			if k == "ranks" && !slices.ContainsFunc(RankSources, func(source RankSource) bool { return source.ID == value }) {
				return s, ErrSettings
			}
			if k == "rss_addrs" && !validRankURL(value) {
				return s, ErrSettings
			}
			*dst = append(*dst, value)
		}
	}
	for _, key := range []string{"vote", "release_date"} {
		b := v[key]
		if b == nil || string(b) == "null" {
			continue
		}
		var str string
		if json.Unmarshal(b, &str) != nil {
			str = string(b)
		}
		if str = strings.TrimSpace(str); str == "" {
			continue
		}
		if key == "vote" {
			n, e := strconv.ParseFloat(str, 64)
			if e != nil || math.IsNaN(n) || n < 0 || n > 10 {
				return s, ErrSettings
			}
			s.Vote = n
		} else {
			n, e := strconv.Atoi(str)
			if e != nil || n < 0 || n > 9999 {
				return s, ErrSettings
			}
			s.Year = n
		}
	}
	return s, nil
}

func validRankURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && len(raw) <= 8192 && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

func (s RankSettings) URLs() []string {
	urls := slices.Clone(s.Addresses)
	for _, id := range s.Ranks {
		for _, rank := range RankSources {
			if rank.ID == id {
				raw := "https://rsshub.app/douban/movie/" + rank.Path
				if !slices.Contains(urls, raw) {
					urls = append(urls, raw)
				}
				break
			}
		}
	}
	return urls
}

type RankEntry struct{ Title, ID string }

func (e RankEntry) Marker() string {
	id := e.ID
	if id == "" {
		// Legacy re.findall formatted an unmatched link as an empty list.
		id = "[]"
	}
	return "doubanrank: " + e.Title + " (DB:" + id + ")"
}

var rankID = regexp.MustCompile(`/([1-9][0-9]*)(?:/|$)`)

func (c Client) RankFeed(ctx context.Context, address string) ([]RankEntry, error) {
	if !validRankURL(address) {
		return nil, ErrSettings
	}
	u, _ := url.Parse(address)
	body, err := c.readWeb(ctx, *u, "", func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !validRankURL(req.URL.String()) {
			return ErrResponse
		}
		// A custom feed URL can contain a private key in its query string.
		req.Header.Del("Referer")
		return nil
	})
	if err != nil {
		return nil, err
	}
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = charset.NewReaderLabel
	var feed struct {
		XMLName xml.Name `xml:"rss"`
		Channel *struct {
			Items []struct {
				Title string `xml:"title"`
				Link  string `xml:"link"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if d.Decode(&feed) != nil || feed.Channel == nil {
		return nil, ErrResponse
	}
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, ErrResponse
		}
		if _, ok := token.(xml.Comment); ok {
			continue
		}
		if data, ok := token.(xml.CharData); !ok || len(bytes.TrimSpace(data)) != 0 {
			return nil, ErrResponse
		}
	}
	result := make([]RankEntry, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		entry := RankEntry{Title: strings.TrimSpace(item.Title)}
		if link, err := url.Parse(strings.TrimSpace(item.Link)); err == nil {
			if m := rankID.FindStringSubmatch(link.Path); len(m) == 2 && valid("movie", m[1]) {
				entry.ID = m[1]
			}
		}
		if entry.Title != "" || entry.ID != "" {
			result = append(result, entry)
		}
	}
	return result, nil
}
