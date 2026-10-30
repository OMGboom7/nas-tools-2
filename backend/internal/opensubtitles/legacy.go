package opensubtitles

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

var seasonNumber = regexp.MustCompile(`(?i)^Season\s+([0-9]{1,4})$`)

func textContent(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.Data == "br" {
			b.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}
func attr(n *html.Node, name string) string {
	if n != nil {
		for _, a := range n.Attr {
			if a.Key == name {
				return a.Val
			}
		}
	}
	return ""
}
func (c *Client) searchLegacy(ctx context.Context, q Query) ([]Candidate, error) {
	address := "https://www.opensubtitles.org/zh/search/"
	if q.IMDb != "" {
		address += "imdbid-" + strings.TrimPrefix(q.IMDb, "tt") + "/sublanguageid-chi"
	} else {
		address += "moviename-" + url.PathEscape(strings.TrimSpace(q.Name+" "+q.Year)) + "/sublanguageid-chi"
	}
	var raw []byte
	for redirect := 0; ; redirect++ {
		data, h, status, e := c.call(ctx, "search", "GET", address, nil, false, 4<<20)
		if e != nil {
			return nil, e
		}
		if status == 200 {
			raw = data
			break
		}
		if status != 301 && status != 302 && status != 303 && status != 307 && status != 308 || redirect >= 3 {
			return nil, ErrChallenge
		}
		before, _ := url.Parse(address)
		next, e := before.Parse(h.Get("Location"))
		if e != nil || !legacyURL(next.String()) {
			return nil, ErrChallenge
		}
		address = next.String()
	}
	return ParseLegacy(string(raw), q)
}
func ParseLegacy(page string, q Query) ([]Candidate, error) {
	if !q.Valid() || len(page) > 4<<20 {
		return nil, ErrConfig
	}
	doc, e := html.Parse(strings.NewReader(page))
	if e != nil {
		return nil, ErrChallenge
	}
	table := cascadia.Query(doc, cascadia.MustCompile("#search_results"))
	if table == nil {
		return nil, ErrChallenge
	}
	rows := cascadia.QueryAll(table, cascadia.MustCompile("tbody > tr:not([style])"))
	if len(rows) > 1000 {
		return nil, ErrLimit
	}
	out := []Candidate{}
	season := 0
	haveSeason := false
	for _, tr := range rows {
		heading := textContent(cascadia.Query(tr, cascadia.MustCompile(`span[id^="season-"] > a > b`)))
		if heading != "" {
			match := seasonNumber.FindStringSubmatch(heading)
			if len(match) != 2 {
				return nil, ErrResponse
			}
			season, _ = strconv.Atoi(match[1])
			haveSeason = true
			continue
		}
		ep := textContent(cascadia.Query(tr, cascadia.MustCompile(`span[itemprop="episodeNumber"]`)))
		episode := 0
		if ep != "" {
			var e error
			episode, e = strconv.Atoi(ep)
			if e != nil || episode < 0 || episode > 9999 {
				return nil, ErrResponse
			}
		}
		title := textContent(cascadia.Query(tr, cascadia.MustCompile("strong > a.bnone")))
		description := ""
		var link *html.Node
		if !haveSeason {
			description = textContent(cascadia.Query(tr, cascadia.MustCompile("td:nth-child(1)")))
			parts := strings.Split(description, "\n")
			if len(parts) > 1 {
				description = parts[1]
			}
			link = cascadia.Query(tr, cascadia.MustCompile("td:nth-child(5) > a"))
		} else {
			description = textContent(cascadia.Query(tr, cascadia.MustCompile(`span[itemprop="name"]`)))
			link = cascadia.Query(tr, cascadia.MustCompile(`a[href^="/download/"]`))
		}
		href := attr(link, "href")
		if href == "" {
			continue
		}
		base, _ := url.Parse("https://www.opensubtitles.org")
		u, e := base.Parse(href)
		if e != nil || !legacyURL(u.String()) {
			return nil, ErrResponse
		}
		if q.IMDb == "" && title != q.Name+" ("+q.Year+")" {
			continue
		}
		if haveSeason && !contains(q.Seasons, season) || ep != "" && !contains(q.Episodes, episode) {
			continue
		}
		if len(description) > 2048 || len(title) > 2048 {
			return nil, ErrLimit
		}
		out = append(out, Candidate{ID: Digest([]byte(u.String())), Title: title, Description: description, Link: u.String(), Season: season, Episode: episode})
		if len(out) >= 100 {
			break
		}
	}
	return out, nil
}
