package douban

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRankSourcesAndLegacyMarkers(t *testing.T) {
	s, err := ReadRankSettings(`{"ranks":["movie-ustop","movie-top250"],"rss_addrs":"https://fixture.test/feed?a=1\nhttps://rsshub.app/douban/movie/ustop","vote":"8.5","release_date":2020}`)
	if err != nil || len(s.URLs()) != 3 || s.Vote != 8.5 || s.Year != 2020 || s.URLs()[2] != "https://rsshub.app/douban/movie/weekly/movie_top250" {
		t.Fatal(s, err)
	}
	if len(RankSources) != 7 {
		t.Fatal(RankSources)
	}
	for _, bad := range []string{`{"vote":"NaN"}`, `{"release_date":"20.3"}`, `{"ranks":["missing"]}`, `{"rss_addrs":["file:///secret"]}`} {
		if _, err := ReadRankSettings(bad); err == nil {
			t.Fatal(bad)
		}
	}
	if (RankEntry{Title: "Film"}).Marker() != "doubanrank: Film (DB:[])" {
		t.Fatal("legacy title-only marker")
	}
}

type rankTransport func(*http.Request) (*http.Response, error)

func (f rankTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRankFeedRequiresRSSAndDoesNotFollowArticleLinks(t *testing.T) {
	const feed = "<rss><channel><item><title>A &amp; B</title><link>https://movie.douban.com/subject/123/</link></item><item><title>Show S02</title></item></channel></rss>"
	for _, body := range []string{feed, "<html/>", feed + "<rss/>", "<rss><channel>", "<rss/>"} {
		c := Client{Transport: rankTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "fixture.test" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
				t.Fatal(r.URL.Host)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
		})}
		rows, err := c.RankFeed(context.Background(), "https://fixture.test/private")
		if body == feed {
			if err != nil || len(rows) != 2 || rows[0] != (RankEntry{"A & B", "123"}) || rows[1].ID != "" {
				t.Fatal(rows, err)
			}
		} else if err == nil {
			t.Fatal(body)
		}
	}
}

func TestRankFeedRedirectDropsPrivateFeedAddress(t *testing.T) {
	var calls int
	c := Client{Transport: rankTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		header := http.Header{}
		code, body := http.StatusOK, "<rss><channel/></rss>"
		if calls == 1 {
			code, body = http.StatusMovedPermanently, ""
			header.Set("Location", "https://other.test/feed")
		} else if r.URL.String() != "https://other.test/feed" || r.Referer() != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("private feed address forwarded")
		}
		return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	if _, err := c.RankFeed(t.Context(), "https://first.test/rss?token=fixture"); err != nil || calls != 2 {
		t.Fatal(err, calls)
	}
}
