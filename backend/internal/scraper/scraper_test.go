package scraper

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

const movie = `{"id":100,"title":"Movie <&>","original_title":"Original","release_date":"2026-01-02","overview":"plot ]]> & text","vote_average":8.1,"poster_path":"/poster.png","external_ids":{"imdb_id":"tt123","tvdb_id":9},"credits":{"cast":[{"id":2,"name":"Actor","known_for_department":"Acting","order":0,"character":"Role","profile_path":"/actor.png"}],"crew":[{"id":3,"name":"Director","job":"Director"}]},"genres":[{"name":"剧情"}],"releases":{"countries":[{"iso_3166_1":"US","certification":"PG"}]}}`
const television = `{"id":200,"name":"Show","original_name":"Original Show","first_air_date":"2025-01-01","external_ids":{"tvdb_id":300},"poster_path":"/poster.png"}`
const season = `{"id":201,"season_number":0,"air_date":"2025-01-01","poster_path":"/season.png","episodes":[{"id":202,"show_id":200,"season_number":0,"episode_number":1,"name":"First","still_path":"/still.png"},{"id":203,"show_id":200,"season_number":0,"episode_number":2,"name":"Second","crew":[{"id":3,"name":"Director","known_for_department":"Directing"}],"guest_stars":[{"id":2,"name":"Guest","known_for_department":"Acting"}]}]}`

func scraperClient(t *testing.T) (Client, *int) {
	t.Helper()
	calls := 0
	c := Client{Host: "tmdb.test", Key: "private-key", Language: "zh", ImageBase: "https://images.test/base"}
	c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" {
			t.Fatal("metadata mutated upstream")
		}
		if r.URL.Host == "tmdb.test" {
			if r.URL.Query().Get("api_key") != "private-key" {
				t.Fatal("key missing")
			}
			switch r.URL.Path {
			case "/3/movie/100":
				return response(r, movie), nil
			case "/3/tv/200":
				return response(r, television), nil
			case "/3/tv/200/season/0":
				return response(r, season), nil
			}
		}
		if r.URL.Host == "webservice.fanart.tv" {
			if r.URL.Query().Get("api_key") == "private-key" {
				t.Fatal("TMDB credential leaked")
			}
			return response(r, `{"tmdb_id":"100","name":"Movie","hdmovielogo":[{"url":"http://assets.fanart.tv/fanart/movie/logo.png"}]}`), nil
		}
		if r.URL.Host == "images.test" || r.URL.Host == "assets.fanart.tv" {
			if r.URL.RawQuery != "" {
				t.Fatal("image credential leaked")
			}
			var b bytes.Buffer
			png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)))
			return response(r, b.String()), nil
		}
		t.Fatal("unexpected or untrusted upstream", r.URL)
		return nil, errors.New("unreachable")
	})
	return c, &calls
}
func TestMovieNFOEscapesXMLIdentitiesCreditsAndArtwork(t *testing.T) {
	c, calls := scraperClient(t)
	o, _, err := Parse(`{"scraper_nfo":{"movie":{"basic":true,"credits":true}},"scraper_pic":{"movie":{"poster":true,"logo":true}}}`)
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Build(t.Context(), Input{Kind: "movie", ID: "100", Stem: "Movie"}, o, time.Unix(0, 0))
	if err != nil || len(p.Artifacts) != 3 || len(p.Warnings) != 0 {
		t.Fatal(p, err)
	}
	b := p.Artifacts[0].Data
	var doc struct {
		Title string `xml:"title"`
		Plot  string `xml:"plot"`
		IDs   []struct {
			Kind    string `xml:"type,attr"`
			Default string `xml:"default,attr"`
		} `xml:"uniqueid"`
	}
	if xml.Unmarshal(b, &doc) != nil || doc.Title != "Movie <&>" || doc.Plot != "plot ]]> & text" || !strings.Contains(string(b), "<order>0</order>") || len(doc.IDs) != 3 || doc.IDs[0].Default != "false" {
		t.Fatal(string(b), doc)
	}
	for _, a := range p.Artifacts[1:] {
		if _, err := c.Download(t.Context(), a.URL); err != nil {
			t.Fatal(err)
		}
	}
	if *calls != 4 {
		t.Fatal(*calls)
	}
}
func TestTelevisionSeasonAndEveryEpisodeInRangeAreGenerated(t *testing.T) {
	c, _ := scraperClient(t)
	o, _, _ := Parse(`{"scraper_nfo":{"tv":{"basic":true,"season_basic":true,"episode_basic":true,"episode_credits":true}},"scraper_pic":{"tv":{"episode_thumb":true}}}`)
	p, err := c.Build(t.Context(), Input{Kind: "tv", ID: "200", Stem: "Show.S00E01-E02", Season: 0, First: 1, Last: 2}, o, time.Now())
	if err != nil || len(p.Artifacts) != 4 {
		t.Fatal(p, err)
	}
	if p.Artifacts[0].Scope != "show" || p.Artifacts[0].Name != "tvshow.nfo" || p.Artifacts[1].Name != "season.nfo" || strings.Count(string(p.Artifacts[2].Data), "<episodedetails>") != 2 || !strings.Contains(string(p.Artifacts[2].Data), "<name>Guest</name>") {
		t.Fatal(p)
	}
	if _, err := c.Build(t.Context(), Input{Kind: "tv", ID: "200", Stem: "Show", Season: 0, First: 1, Last: 3}, o, time.Now()); err == nil {
		t.Fatal("invented missing episode")
	}
}
func TestBadImagesRedirectsAndMetadataNeverBecomeSidecars(t *testing.T) {
	c, calls := scraperClient(t)
	for _, address := range []string{"https://evil.test/a.png", "https://images.test/base/t/p/original/a.png?api_key=key", "https://images.test/other/a.png", "http://assets.fanart.tv/fanart/a.png", "https://images.test/base/t/p/original/../a.png"} {
		if _, err := c.Download(t.Context(), address); err == nil {
			t.Fatal(address)
		}
	}
	if *calls != 0 {
		t.Fatal("untrusted host contacted")
	}
	for _, mode := range []string{"html", "redirect", "oversize", "lost", "wrong-id"} {
		c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			switch mode {
			case "lost":
				return nil, errors.New("private-key diagnostic")
			case "oversize":
				return response(r, strings.Repeat("x", (16<<20)+1)), nil
			case "wrong-id":
				return response(r, `{"id":101,"title":"Other"}`), nil
			case "redirect":
				v := response(r, "")
				v.StatusCode = 302
				v.Header.Set("Location", "https://evil.test/a")
				return v, nil
			default:
				return response(r, "<html>not image</html>"), nil
			}
		})
		var err error
		if mode == "wrong-id" {
			_, err = c.Build(t.Context(), Input{Kind: "movie", ID: "100", Stem: "Movie"}, Options{}, time.Now())
		} else {
			_, err = c.Download(t.Context(), "https://images.test/base/t/p/original/a.png")
		}
		if err == nil || strings.Contains(err.Error(), "private-key") {
			t.Fatal(mode, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.tmdbCancelled(ctx); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}

// Exercise request cancellation through the same non-retrying request path.
func (c Client) tmdbCancelled(ctx context.Context) ([]byte, error) {
	return c.read(ctx, "https://tmdb.test/3/movie/100", 100)
}
func TestOptionsKeepFalseFlagsAndReportUnsupportedCapabilities(t *testing.T) {
	o, w, err := Parse(`{"scraper_nfo":{"movie":{"basic":false,"future":true}},"unknown":{"secret":"not returned"}}`)
	if err != nil || o.NFO["movie"]["basic"] || len(w) == 0 {
		t.Fatal(o, w, err)
	}
	for _, raw := range []string{"invalid", `{"scraper_nfo":{"movie":{"basic":1}}}`} {
		if _, _, err := Parse(raw); err == nil {
			t.Fatal(raw)
		}
	}
	if (Options{NFO: map[string]Flags{"other": {}}}).ValidatePatch() == nil {
		t.Fatal("unknown patch accepted")
	}
	c, _ := scraperClient(t)
	base := c.Transport
	c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "frodo.douban.com" {
			return response(r, `{"items":[]}`), nil
		}
		if strings.HasPrefix(r.URL.Path, "/3/person/") {
			id := strings.TrimPrefix(r.URL.Path, "/3/person/")
			return response(r, `{"id":`+id+`,"also_known_as":[]}`), nil
		}
		return base.RoundTrip(r)
	})
	o, _, _ = Parse(`{"scraper_nfo":{"movie":{"basic":true,"credits":true,"credits_chinese":true}}}`)
	p, err := c.Build(t.Context(), Input{Kind: "movie", ID: "100", Stem: "Movie"}, o, time.Now())
	if err != nil || len(p.Warnings) == 0 {
		t.Fatal("missing Chinese names silently accepted", err)
	}
}

func TestMissingCreditsAreNotReportedAsVerifiedEmpty(t *testing.T) {
	c, _ := scraperClient(t)
	c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) { return response(r, `{"id":100,"title":"Movie"}`), nil })
	o, _, _ := Parse(`{"scraper_nfo":{"movie":{"basic":true,"credits":true}}}`)
	p, err := c.Build(t.Context(), Input{Kind: "movie", ID: "100", Stem: "Movie"}, o, time.Now())
	if err != nil || len(p.Artifacts) != 1 || len(p.Warnings) == 0 {
		t.Fatal(p, err)
	}
}

func TestMalformedStillDoesNotEnableLocalThumbnailFallback(t *testing.T) {
	c, _ := scraperClient(t)
	base := c.Transport
	c.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/3/tv/200/season/0" {
			return response(r, strings.Replace(season, "/still.png", "https://attacker.test/image.jpg", 1)), nil
		}
		return base.RoundTrip(r)
	})
	o, _, _ := Parse(`{"scraper_pic":{"tv":{"episode_thumb":true,"episode_thumb_ffmpeg":true}}}`)
	p, err := c.Build(t.Context(), Input{Kind: "tv", ID: "200", Stem: "Show", Season: 0, First: 1, Last: 1}, o, time.Now())
	if err != nil || len(p.Artifacts) != 0 || len(p.Warnings) == 0 {
		t.Fatal("invalid remote metadata caused local decoding", p, err)
	}
}
