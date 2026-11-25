package douban

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestInterestGridPaginationDateAndUserDedup(t *testing.T) {
	now := time.Date(2026, 11, 25, 23, 0, 0, 0, time.UTC)
	calls := 0
	c := Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "movie.douban.com" || r.Header.Get("Cookie") != "a=fixture" || r.URL.Query().Get("sort") != "time" {
			t.Fatal(r.URL)
		}
		calls++
		row := func(id int, date string) string {
			return fmt.Sprintf(`<div class="item"><div class="info"><li class="title"><a href="https://movie.douban.com/subject/%d/">题</a></li><span class="date">%s</span></div></div>`, id, date)
		}
		grid := "<div class='grid-view'>"
		if r.URL.Path == "/people/a/wish" && r.URL.Query().Get("start") == "0" {
			for i := 1; i <= 15; i++ {
				grid += row(i, "2026-11-24")
			}
		} else {
			grid += row(1, "2026-11-24") + row(50, "2026-11-23") + row(51, "2026-11-22")
		}
		return reply(r, grid+"</div>"), nil
	})}
	list := InterestList{Users: []string{"a", "b"}, Types: []string{"wish"}, Days: 2, Cookie: "a=fixture"}
	items, err := c.Interests(t.Context(), list, now, func(context.Context) error { return nil })
	if err != nil || len(items) != 15 || items[0] != "1" || items[14] != "15" || calls != 3 {
		t.Fatal(items, calls, err)
	}
}

func TestInterestsRecentForeignLinksAndSubjectFallback(t *testing.T) {
	calls := 0
	c := Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "movie.douban.com" && r.Header.Get("Cookie") != "" {
			t.Fatal("credential disclosed")
		}
		switch r.URL.Path {
		case "/feed/people/person/interests":
			rows := ""
			for _, pair := range [][]string{{"想看A", "https://movie.douban.com/subject/11/"}, {"在看B", "https://movie.douban.com/subject/12/"}, {"看过C", "https://movie.douban.com/subject/13/"}, {"想看D", "https://movie.douban.com.other/subject/14/"}} {
				rows += "<item><title>" + pair[0] + "</title><link>" + pair[1] + "</link><pubDate>Wed, 25 Nov 2026 10:00:00 GMT</pubDate></item>"
			}
			return reply(r, "<rss><channel>"+rows+"</channel></rss>"), nil
		case "/api/v2/movie/11", "/api/v2/tv/11":
			return reply(r, `{"localized_message":"unavailable"}`), nil
		case "/subject/11/":
			if r.Header.Get("Cookie") != "a=b" {
				t.Fatal(r.Header)
			}
			return reply(r, `<div id="content"><span property="v:itemreviewed">Title</span><span class="year">(2020)</span><div id="info"><span>集数:</span>10</div></div>`), nil
		default:
			t.Fatal(r.URL)
			return nil, nil
		}
	})}
	list := InterestList{Users: []string{"person"}, Types: []string{"do", "wish"}, Recent: true, Cookie: "a=b"}
	items, err := c.Interests(t.Context(), list, time.Now(), func(context.Context) error { return nil })
	if err != nil || !slices.Equal(items, []string{"12", "11"}) {
		t.Fatal(items, err)
	}
	s, err := c.InterestSubject(t.Context(), "11", list.Cookie)
	if err != nil || s.Title != "Title" || s.Kind != "tv" || s.Year != "2020" || calls != 4 {
		t.Fatal(s, err, calls)
	}
	if _, _, err := parseGrid([]byte("<h1>login required</h1>"), "wish"); err == nil {
		t.Fatal("login accepted")
	}
	if _, err := parseInterests([]byte("<html></html>")); err == nil {
		t.Fatal("invalid feed accepted")
	}
	list.Users = []string{"../other"}
	if _, err := c.Interests(t.Context(), list, time.Now(), func(context.Context) error { return nil }); err == nil || calls != 4 {
		t.Fatal(err, calls)
	}
	_, _, err = parseGrid([]byte(strings.Repeat(" ", 4)), "wish")
	if err == nil {
		t.Fatal("missing list")
	}
}
