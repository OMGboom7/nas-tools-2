package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Retains the bundled project credential already used by config.py. No user
// token, configured TMDB key or local media path is sent to Fanart.
const legacyFanartKey = "d2d31f9ecabea050fc7d68aa3146015f"

type Client struct {
	Host, Key, Language, ImageBase string
	Transport                      http.RoundTripper
}
type Input struct {
	Kind, ID, Stem      string
	Season, First, Last int
}
type Artifact struct {
	Scope, Name, URL string
	Data             []byte
	Thumbnail        bool
}
type Plan struct {
	Artifacts []Artifact
	Warnings  []string
}

func (c Client) valid() bool {
	h, err := url.Parse("https://" + c.Host)
	b, err2 := url.Parse(c.ImageBase)
	return err == nil && err2 == nil && h.Hostname() != "" && !strings.ContainsAny(c.Host, "/?#@\\") && !strings.Contains(c.Host, "..") && c.Key != "" && len(c.Key) <= 4096 && !strings.ContainsFunc(c.Key, unicode.IsControl) && len(c.Language) > 0 && len(c.Language) <= 32 && !strings.ContainsFunc(c.Language, unicode.IsControl) && b.Hostname() != "" && (b.Scheme == "https" || b.Scheme == "http") && b.User == nil && b.RawQuery == "" && b.Fragment == ""
}
func (c Client) read(ctx context.Context, address string, max int64) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ErrMetadata
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, ErrMetadata
	}
	r.Header.Set("Accept", "application/json, image/jpeg, image/png")
	client := http.Client{Transport: c.Transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		return nil, ErrMetadata
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, ErrMetadata
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil || int64(len(b)) > max || ctx.Err() != nil {
		return nil, ErrMetadata
	}
	return b, nil
}
func (c Client) tmdb(ctx context.Context, resource, appendTo string, out any) error {
	if !c.valid() {
		return ErrConfig
	}
	u := url.URL{Scheme: "https", Host: c.Host, Path: "/3" + resource}
	q := url.Values{"api_key": {c.Key}, "language": {c.Language}}
	if appendTo != "" {
		q.Set("append_to_response", appendTo)
	}
	u.RawQuery = q.Encode()
	b, err := c.read(ctx, u.String(), 2<<20)
	if err != nil {
		return err
	}
	if json.Unmarshal(b, out) != nil {
		return ErrMetadata
	}
	return nil
}
func canonicalID(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}
func (c Client) Image(file string) string {
	if !c.valid() || !strings.HasPrefix(file, "/") || strings.HasPrefix(file, "//") || path.Clean(file) != file || strings.ContainsAny(file, "?#\\") || strings.ContainsFunc(file, unicode.IsControl) {
		return ""
	}
	u, err := url.Parse(strings.TrimRight(c.ImageBase, "/") + "/t/p/original" + file)
	if err != nil {
		return ""
	}
	return u.String()
}
func (c Client) Download(ctx context.Context, address string) ([]byte, error) {
	u, err := url.Parse(address)
	base, err2 := url.Parse(c.ImageBase)
	if err != nil || err2 != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, ErrMetadata
	}
	trusted := u.Scheme == base.Scheme && u.Host == base.Host && strings.HasPrefix(u.Path, strings.TrimRight(base.Path, "/")+"/t/p/original/")
	if !trusted && !(u.Scheme == "https" && u.Host == "assets.fanart.tv" && strings.HasPrefix(u.Path, "/fanart/")) {
		return nil, ErrMetadata
	}
	if strings.Contains(u.EscapedPath(), "%") || path.Clean(u.Path) != u.Path {
		return nil, ErrMetadata
	}
	b, err := c.read(ctx, u.String(), 16<<20)
	if err != nil {
		return nil, err
	}
	if err := ValidateImage(b, strings.ToLower(path.Ext(u.Path))); err != nil {
		return nil, err
	}
	return b, nil
}
func ValidateImage(b []byte, ext string) error {
	if len(b) == 0 || len(b) > 16<<20 {
		return ErrMetadata
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 20000 || cfg.Height > 20000 || int64(cfg.Width)*int64(cfg.Height) > 80_000_000 || format != "jpeg" && format != "png" || format == "jpeg" && ext != ".jpg" && ext != ".jpeg" || format == "png" && ext != ".png" {
		return ErrMetadata
	}
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(b))
	if err != nil || decodedFormat != format || decoded.Bounds().Dx() != cfg.Width || decoded.Bounds().Dy() != cfg.Height {
		return ErrMetadata
	}
	return nil
}

func (c Client) Build(ctx context.Context, in Input, o Options, now time.Time) (Plan, error) {
	plan := Plan{Artifacts: []Artifact{}, Warnings: []string{}}
	if !canonicalID(in.ID) || in.Kind != "movie" && in.Kind != "tv" || in.Stem == "" || path.Base(in.Stem) != in.Stem || in.Stem == "." || strings.ContainsAny(in.Stem, "\\\x00") {
		return plan, ErrMetadata
	}
	var d Detail
	appendTo := "credits,external_ids,releases"
	if in.Kind == "tv" {
		appendTo = "credits,external_ids"
	}
	if err := c.tmdb(ctx, "/"+in.Kind+"/"+in.ID, appendTo, &d); err != nil || strconv.FormatInt(d.ID, 10) != in.ID {
		return plan, ErrMetadata
	}
	n, p := o.NFO[in.Kind], o.Pictures[in.Kind]
	addNFO := func(scope, name string, b []byte, err error) error {
		if err != nil {
			return err
		}
		plan.Artifacts = append(plan.Artifacts, Artifact{Scope: scope, Name: name, Data: b})
		return nil
	}
	if n["basic"] || n["credits"] {
		if n["credits"] && (d.Credits.Cast == nil || d.Credits.Crew == nil) {
			plan.Warnings = append(plan.Warnings, "complete cast/crew metadata unavailable; missing credits were not treated as verified empty")
		}
		if n["credits"] && n["credits_chinese"] {
			plan.Warnings = append(plan.Warnings, c.localizePeople(ctx, &d, in.Kind)...)
		}
		scope, name := "media", in.Stem+".nfo"
		if in.Kind == "tv" {
			scope, name = "show", "tvshow.nfo"
		}
		b, err := NFO(d, in.Kind, n, c.Image, now)
		if err = addNFO(scope, name, b, err); err != nil {
			return plan, err
		}
	}
	var season Season
	selected := []Episode{}
	if in.Kind == "tv" && (n["season_basic"] || n["episode_basic"] || n["episode_credits"] || p["season_poster"] || p["season_banner"] || p["season_thumb"] || p["episode_thumb"]) {
		if in.Season < 0 || in.Season > 99 || in.First < 0 || in.Last < in.First || in.Last-in.First > 999 {
			return plan, ErrMetadata
		}
		if err := c.tmdb(ctx, "/tv/"+in.ID+"/season/"+strconv.Itoa(in.Season), "", &season); err != nil || season.ID <= 0 || season.Number == nil || *season.Number != in.Season || season.Episodes == nil || len(season.Episodes) > 10000 {
			return plan, ErrMetadata
		}
		seen := map[int]bool{}
		for _, ep := range season.Episodes {
			if ep.ID <= 0 || ep.Number == nil || ep.Season == nil || *ep.Season != in.Season || seen[*ep.Number] || ep.Show != nil && strconv.FormatInt(*ep.Show, 10) != in.ID {
				return plan, ErrMetadata
			}
			seen[*ep.Number] = true
		}
		for number := in.First; number <= in.Last; number++ {
			found := false
			for _, ep := range season.Episodes {
				if *ep.Number == number {
					selected = append(selected, ep)
					found = true
					break
				}
			}
			if !found {
				return plan, ErrMetadata
			}
		}
		if n["season_basic"] {
			b, err := SeasonNFO(season, now)
			if err = addNFO("media", "season.nfo", b, err); err != nil {
				return plan, err
			}
		}
		if n["episode_basic"] || n["episode_credits"] {
			if n["episode_credits"] {
				for _, ep := range selected {
					if ep.Crew == nil || ep.Guests == nil {
						plan.Warnings = append(plan.Warnings, "complete episode credits unavailable; missing people were not invented")
						break
					}
				}
			}
			b, err := EpisodeNFO(selected, n, now)
			if err = addNFO("media", in.Stem+".nfo", b, err); err != nil {
				return plan, err
			}
		}
	}
	var fanart map[string]json.RawMessage
	fanartRead := false
	fan := func(key string, seasonal bool) string {
		if !fanartRead {
			fanartRead = true
			resource, id := "movies", in.ID
			if in.Kind == "tv" {
				resource, id = "tv", strconv.FormatInt(d.External.TVDB, 10)
			}
			if canonicalID(id) {
				u := url.URL{Scheme: "https", Host: "webservice.fanart.tv", Path: "/v3/" + resource + "/" + id, RawQuery: url.Values{"api_key": {legacyFanartKey}}.Encode()}
				if b, err := c.read(ctx, u.String(), 2<<20); err == nil && json.Unmarshal(b, &fanart) == nil {
					identityKey := "tmdb_id"
					if resource == "tv" {
						identityKey = "thetvdb_id"
					}
					var returned string
					if json.Unmarshal(fanart[identityKey], &returned) != nil || returned != id {
						fanart = nil
					}
				}
			}
		}
		var images []struct {
			URL    string `json:"url"`
			Season string `json:"season"`
		}
		if json.Unmarshal(fanart[key], &images) != nil {
			return ""
		}
		for _, im := range images {
			if !seasonal || im.Season == strconv.Itoa(in.Season) {
				if strings.HasPrefix(im.URL, "http://assets.fanart.tv/") {
					return "https://" + strings.TrimPrefix(im.URL, "http://")
				}
				return im.URL
			}
		}
		return ""
	}
	addImage := func(scope, name, address string) {
		if address == "" {
			plan.Warnings = append(plan.Warnings, name+" image unavailable; no image invented")
			return
		}
		u, err := url.Parse(address)
		if err != nil {
			plan.Warnings = append(plan.Warnings, name+" image address rejected")
			return
		}
		ext := strings.ToLower(path.Ext(u.Path))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
			plan.Warnings = append(plan.Warnings, name+" image format unavailable")
			return
		}
		plan.Artifacts = append(plan.Artifacts, Artifact{Scope: scope, Name: name + ext, URL: address})
	}
	scope, prefix := "media", "movie"
	if in.Kind == "tv" {
		scope, prefix = "show", "tv"
	}
	if p["poster"] {
		address := c.Image(d.Poster)
		if address == "" {
			address = fan(prefix+"poster", false)
		}
		addImage(scope, "poster", address)
	}
	if p["backdrop"] {
		address := fan(prefix+"thumb", false)
		if address == "" {
			address = c.Image(d.Backdrop)
		}
		addImage(scope, "fanart", address)
	}
	for _, spec := range []struct{ flag, movie, tv, name string }{{"background", "moviebackground", "showbackground", "background"}, {"logo", "hdmovielogo", "hdtvlogo", "logo"}, {"disc", "moviedisc", "", "disc"}, {"clearart", "", "hdclearart", "clearart"}, {"banner", "moviebanner", "tvbanner", "banner"}, {"thumb", "moviethumb", "tvthumb", "thumb"}} {
		if p[spec.flag] {
			key := spec.movie
			if in.Kind == "tv" {
				key = spec.tv
			}
			if key != "" {
				addImage(scope, spec.name, fan(key, false))
			}
		}
	}
	if in.Kind == "tv" {
		for _, spec := range []struct{ flag, key, name string }{{"season_poster", "seasonposter", "poster"}, {"season_banner", "seasonbanner", "banner"}, {"season_thumb", "seasonthumb", "landscape"}} {
			if p[spec.flag] {
				address := fan(spec.key, true)
				if address == "" && spec.flag == "season_poster" {
					address = c.Image(season.Poster)
				}
				addImage("show", fmtSeason(in.Season)+"-"+spec.name, address)
			}
		}
		if p["episode_thumb"] {
			address := ""
			rawStill := ""
			if len(selected) > 0 {
				rawStill = selected[0].Still
				address = c.Image(rawStill)
			}
			if rawStill != "" && address == "" {
				plan.Warnings = append(plan.Warnings, "episode image path rejected; malformed metadata was not treated as permission to decode video")
			} else if address == "" && p["episode_thumb_ffmpeg"] {
				plan.Artifacts = append(plan.Artifacts, Artifact{Scope: "media", Name: in.Stem + "-thumb.jpg", Thumbnail: true})
			} else {
				addImage("media", in.Stem+"-thumb", address)
			}
		}
	}
	return plan, nil
}
func fmtSeason(n int) string {
	s := strconv.Itoa(n)
	if len(s) < 2 {
		s = "0" + s
	}
	return "season" + s
}
