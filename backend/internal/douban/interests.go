package douban

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

var subjectPath = regexp.MustCompile(`^/subject/([1-9][0-9]*)/?$`)

type InterestList struct {
	Users, Types []string
	Recent       bool
	Days         int
	Cookie       string
}

func (l InterestList) Valid() bool {
	if len(l.Users) == 0 || len(l.Types) == 0 || l.Days < 0 || len(l.Cookie) > 16384 || strings.ContainsAny(l.Cookie, "\r\n\x00") {
		return false
	}
	for _, u := range l.Users {
		if u == "" || u == "." || u == ".." || strings.ContainsAny(u, "/?#%\\") || strings.IndexFunc(u, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			return false
		}
	}
	for _, t := range l.Types {
		if t != "wish" && t != "do" && t != "collect" {
			return false
		}
	}
	return true
}

func (c Client) web(ctx context.Context, u url.URL, cookie string) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, ErrResponse
	}
	r.Header.Set("User-Agent", "Mozilla/5.0 (compatible; NAS-Tools)")
	if cookie != "" && u.Host == "movie.douban.com" {
		r.Header.Set("Cookie", cookie)
	}
	client := http.Client{Transport: c.Transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		return nil, ErrResponse
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, ErrResponse
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil || len(b) > 4<<20 {
		return nil, ErrResponse
	}
	return b, nil
}

func subjectLink(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.User != nil || u.Host != "movie.douban.com" || u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	m := subjectPath.FindStringSubmatch(u.Path)
	if m == nil || !valid("movie", m[1]) {
		return ""
	}
	return m[1]
}

func attribute(n *html.Node, key string) string {
	if n != nil {
		for _, v := range n.Attr {
			if v.Key == key {
				return v.Val
			}
		}
	}
	return ""
}
func nodeText(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	for node := range n.Descendants() {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
	}
	return strings.TrimSpace(b.String())
}

var gridSelector = cascadia.MustCompile(".grid-view")
var rowSelector = cascadia.MustCompile(".item")
var titleLink = cascadia.MustCompile(".info .title a[href]")
var dateSelector = cascadia.MustCompile(".info .date")
var nextPage = cascadia.MustCompile(".paginator .next a[href]")

type mark struct{ id, date, kind string }

func parseGrid(b []byte, kind string) ([]mark, bool, error) {
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return nil, false, err
	}
	grid := cascadia.Query(doc, gridSelector)
	if grid == nil {
		return nil, false, ErrResponse
	}
	items := []mark{}
	for _, node := range cascadia.QueryAll(grid, rowSelector) {
		link := attribute(cascadia.Query(node, titleLink), "href")
		if strings.HasPrefix(link, "/subject/") {
			link = "https://movie.douban.com" + link
		}
		id := subjectLink(link)
		date := nodeText(cascadia.Query(node, dateSelector))
		if id == "" {
			return nil, false, ErrResponse
		}
		items = append(items, mark{id, date, kind})
	}
	return items, cascadia.Query(doc, nextPage) != nil, nil
}

func parseInterests(b []byte) ([]mark, error) {
	var feed struct {
		XMLName xml.Name `xml:"rss"`
		Channel *struct {
			Items []struct {
				Title string `xml:"title"`
				Link  string `xml:"link"`
				Date  string `xml:"pubDate"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if xml.Unmarshal(b, &feed) != nil || feed.Channel == nil {
		return nil, ErrResponse
	}
	items := []mark{}
	for _, v := range feed.Channel.Items {
		id := subjectLink(v.Link)
		if id == "" {
			continue
		}
		kind := "collect"
		if strings.HasPrefix(v.Title, "想看") {
			kind = "wish"
		} else if strings.HasPrefix(v.Title, "在看") {
			kind = "do"
		}
		date, err := mail.ParseDate(v.Date)
		if err != nil {
			return nil, ErrResponse
		}
		items = append(items, mark{id, date.Format("2006-01-02"), kind})
	}
	return items, nil
}

// Interests preserves user/type order and returns each subject once. wait is
// called before every upstream page so a cancelled run stops during pacing.
func (c Client) Interests(ctx context.Context, l InterestList, now time.Time, wait func(context.Context) error) ([]string, error) {
	if !l.Valid() {
		return nil, ErrResponse
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var ids []string
	seen := map[string]bool{}
	accept := func(m mark) (bool, error) {
		date, err := time.Parse("2006-01-02", m.date)
		if err != nil {
			return false, ErrResponse
		}
		if l.Days > 0 && (today.Unix()-date.Unix())/86400 >= int64(l.Days) {
			return false, nil
		}
		if !seen[m.id] {
			ids = append(ids, m.id)
			seen[m.id] = true
		}
		return true, nil
	}
	for _, user := range l.Users {
		if l.Recent {
			if err := wait(ctx); err != nil {
				return nil, err
			}
			b, err := c.web(ctx, url.URL{Scheme: "https", Host: "www.douban.com", Path: "/feed/people/" + user + "/interests"}, "")
			if err != nil {
				return nil, err
			}
			items, err := parseInterests(b)
			if err != nil {
				return nil, err
			}
			for _, kind := range l.Types {
				for _, v := range items {
					if v.kind == kind {
						if _, err := accept(v); err != nil {
							return nil, err
						}
					}
				}
			}
			continue
		}
		for _, kind := range l.Types {
			pageIDs := map[string]bool{}
			for offset := 0; ; offset += 15 {
				if err := wait(ctx); err != nil {
					return nil, err
				}
				q := url.Values{"start": {strconv.Itoa(offset)}, "sort": {"time"}, "rating": {"all"}, "filter": {"all"}, "mode": {"grid"}}
				b, err := c.web(ctx, url.URL{Scheme: "https", Host: "movie.douban.com", Path: "/people/" + user + "/" + kind, RawQuery: q.Encode()}, l.Cookie)
				if err != nil {
					return nil, err
				}
				items, more, err := parseGrid(b, kind)
				if err != nil {
					return nil, err
				}
				// Older pages sometimes have no next link even when full.
				stop := false
				fresh := false
				for _, v := range items {
					if !pageIDs[v.id] {
						pageIDs[v.id] = true
						fresh = true
					}
					if v.date == "" {
						stop = true
						break
					}
					ok, err := accept(v)
					if err != nil {
						return nil, err
					}
					if !ok {
						stop = true
						break
					}
				}
				if stop || !more && len(items) < 15 {
					break
				}
				if !fresh || len(items) > 15 {
					return nil, ErrResponse
				}
			}
		}
	}
	return ids, ctx.Err()
}

var detailTitle = cascadia.MustCompile("[property='v:itemreviewed']")
var detailYear = cascadia.MustCompile("#content .year")
var detailRating = cascadia.MustCompile("[property='v:average']")
var detailPoster = cascadia.MustCompile("#mainpic img")
var detailInfo = cascadia.MustCompile("#info")

func (c Client) InterestSubject(ctx context.Context, id, cookie string) (Subject, error) {
	if !valid("movie", id) {
		return Subject{}, ErrResponse
	}
	for _, kind := range []string{"movie", "tv"} {
		s, err := c.Subject(ctx, kind, id)
		if err == nil && !slices.Contains([]string{"未知电影", "未知电视剧"}, s.Title) {
			return s, nil
		}
	}
	b, err := c.web(ctx, url.URL{Scheme: "https", Host: "movie.douban.com", Path: "/subject/" + id + "/"}, cookie)
	if err != nil {
		return Subject{}, err
	}
	doc, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return Subject{}, ErrResponse
	}
	title := nodeText(cascadia.Query(doc, detailTitle))
	if title == "" {
		return Subject{}, ErrResponse
	}
	kind := "movie"
	info := nodeText(cascadia.Query(doc, detailInfo))
	if strings.Contains(info, "集数:") || strings.Contains(info, "集数：") || strings.Contains(info, "季数:") {
		kind = "tv"
	}
	y := strings.Trim(nodeText(cascadia.Query(doc, detailYear)), "() ")
	return Subject{ID: id, Title: title, Original: title, Year: y, Kind: kind, Rating: nodeText(cascadia.Query(doc, detailRating)), Image: attribute(cascadia.Query(doc, detailPoster), "src")}, nil
}
