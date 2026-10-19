package scraper

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Person struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Character  string `json:"character"`
	Order      *int   `json:"order"`
	Job        string `json:"job"`
	Department string `json:"known_for_department"`
	Profile    string `json:"profile_path"`
}
type Credits struct {
	Cast []Person `json:"cast"`
	Crew []Person `json:"crew"`
}
type Detail struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	Date          string  `json:"release_date"`
	FirstDate     string  `json:"first_air_date"`
	Overview      string  `json:"overview"`
	Vote          float64 `json:"vote_average"`
	IMDb          string  `json:"imdb_id"`
	External      struct {
		TVDB int64  `json:"tvdb_id"`
		IMDb string `json:"imdb_id"`
	} `json:"external_ids"`
	Genres []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Credits  Credits `json:"credits"`
	Releases struct {
		Countries []struct {
			Country       string `json:"iso_3166_1"`
			Certification string `json:"certification"`
		} `json:"countries"`
	} `json:"releases"`
	Poster   string `json:"poster_path"`
	Backdrop string `json:"backdrop_path"`
}
type Episode struct {
	ID       int64    `json:"id"`
	Number   *int     `json:"episode_number"`
	Season   *int     `json:"season_number"`
	Show     *int64   `json:"show_id"`
	Name     string   `json:"name"`
	Overview string   `json:"overview"`
	Date     string   `json:"air_date"`
	Vote     float64  `json:"vote_average"`
	Still    string   `json:"still_path"`
	Crew     []Person `json:"crew"`
	Guests   []Person `json:"guest_stars"`
}
type Season struct {
	ID       int64     `json:"id"`
	Number   *int      `json:"season_number"`
	Name     string    `json:"name"`
	Overview string    `json:"overview"`
	Date     string    `json:"air_date"`
	Poster   string    `json:"poster_path"`
	Episodes []Episode `json:"episodes"`
}

type node struct {
	name, value string
	attrs       []xml.Attr
	children    []node
}

func textNode(name string, value any) node { return node{name: name, value: fmt.Sprint(value)} }
func attr(name, value string) xml.Attr     { return xml.Attr{Name: xml.Name{Local: name}, Value: value} }
func (n node) encode(e *xml.Encoder) error {
	s := xml.StartElement{Name: xml.Name{Local: n.name}, Attr: n.attrs}
	if err := e.EncodeToken(s); err != nil {
		return err
	}
	if len(n.children) == 0 {
		if err := e.EncodeToken(xml.CharData(n.value)); err != nil {
			return err
		}
	}
	for _, child := range n.children {
		if err := child.encode(e); err != nil {
			return err
		}
	}
	return e.EncodeToken(s.End())
}
func xmlFile(root node) ([]byte, error) {
	b := bytes.NewBufferString(xml.Header)
	e := xml.NewEncoder(b)
	e.Indent("", "  ")
	if err := root.encode(e); err != nil {
		return nil, ErrMetadata
	}
	if e.Flush() != nil || b.Len() > 2<<20 {
		return nil, ErrMetadata
	}
	return append(b.Bytes(), '\n'), nil
}
func year(s string) string {
	if len(s) >= 4 {
		return s[:4]
	}
	return ""
}
func identities(id int64, imdb string, tvdb int64) []node {
	defaultTMDB := "true"
	if imdb != "" {
		defaultTMDB = "false"
	}
	u := textNode("uniqueid", id)
	u.attrs = []xml.Attr{attr("type", "tmdb"), attr("default", defaultTMDB)}
	out := []node{textNode("tmdbid", id), u}
	for _, v := range []struct{ kind, value string }{{"imdb", imdb}, {"tvdb", strconv.FormatInt(tvdb, 10)}} {
		if v.value == "" || v.value == "0" {
			continue
		}
		u := textNode("uniqueid", v.value)
		u.attrs = []xml.Attr{attr("type", v.kind)}
		if v.kind == "imdb" {
			u.attrs = append(u.attrs, attr("default", "true"))
		}
		out = append(out, textNode(v.kind+"id", v.value), u)
	}
	return out
}
func people(c Credits, image func(string) string, episode bool) []node {
	out := []node{}
	for _, p := range c.Crew {
		if p.Job != "Director" && (!episode || p.Department != "Directing") {
			continue
		}
		n := textNode("director", p.Name)
		n.attrs = []xml.Attr{attr("tmdbid", strconv.FormatInt(p.ID, 10))}
		out = append(out, n)
	}
	for _, p := range c.Cast {
		if p.Department != "Acting" {
			continue
		}
		n := node{name: "actor", children: []node{textNode("name", p.Name), textNode("type", "Actor"), textNode("tmdbid", p.ID)}}
		if !episode {
			n.children = append(n.children, textNode("role", p.Character), textNode("thumb", image(p.Profile)), textNode("profile", "https://www.themoviedb.org/person/"+strconv.FormatInt(p.ID, 10)))
			if p.Order != nil {
				n.children = append(n.children, textNode("order", *p.Order))
			}
		}
		out = append(out, n)
	}
	return out
}
func NFO(d Detail, kind string, flags Flags, image func(string) string, now time.Time) ([]byte, error) {
	root := node{name: "movie"}
	title, original, date := d.Title, d.OriginalTitle, d.Date
	if kind == "tv" {
		root.name = "tvshow"
		title, original, date = d.Name, d.OriginalName, d.FirstDate
	} else if kind != "movie" {
		return nil, ErrMetadata
	}
	if d.ID <= 0 || strings.TrimSpace(title) == "" || d.Vote < 0 || d.Vote > 10 {
		return nil, ErrMetadata
	}
	if flags["basic"] {
		imdb := d.External.IMDb
		if imdb == "" {
			imdb = d.IMDb
		}
		root.children = append(root.children, textNode("dateadded", now.Format("2006-01-02 15:04:05")))
		root.children = append(root.children, identities(d.ID, imdb, d.External.TVDB)...)
		root.children = append(root.children, textNode("plot", d.Overview), textNode("outline", d.Overview), textNode("title", title), textNode("originaltitle", original), textNode("premiered", date), textNode("year", year(date)), textNode("rating", d.Vote))
		for _, g := range d.Genres {
			root.children = append(root.children, textNode("genre", g.Name))
		}
		for _, r := range d.Releases.Countries {
			if r.Country == "US" && strings.TrimSpace(r.Certification) != "" {
				root.children = append(root.children, textNode("mpaa", r.Certification))
				break
			}
		}
		if kind == "tv" {
			root.children = append(root.children, textNode("season", -1), textNode("episode", -1))
		}
	}
	if flags["credits"] {
		root.children = append(root.children, people(d.Credits, image, false)...)
	}
	return xmlFile(root)
}
func SeasonNFO(s Season, now time.Time) ([]byte, error) {
	if s.ID <= 0 || s.Number == nil {
		return nil, ErrMetadata
	}
	return xmlFile(node{name: "season", children: []node{textNode("dateadded", now.Format("2006-01-02 15:04:05")), textNode("plot", s.Overview), textNode("outline", s.Overview), textNode("title", fmt.Sprintf("季 %d", *s.Number)), textNode("premiered", s.Date), textNode("releasedate", s.Date), textNode("year", year(s.Date)), textNode("seasonnumber", *s.Number)}})
}

// Multi-episode media retain every verified episode, rather than silently
// producing metadata only for the first episode in an approved range.
func EpisodeNFO(episodes []Episode, flags Flags, now time.Time) ([]byte, error) {
	out := []byte(xml.Header)
	for _, ep := range episodes {
		if ep.ID <= 0 || ep.Number == nil || ep.Season == nil || ep.Vote < 0 || ep.Vote > 10 {
			return nil, ErrMetadata
		}
		n := node{name: "episodedetails"}
		if flags["episode_basic"] {
			n.children = append(n.children, textNode("dateadded", now.Format("2006-01-02 15:04:05")))
			n.children = append(n.children, identities(ep.ID, "", 0)...)
			n.children = append(n.children, textNode("title", ep.Name), textNode("plot", ep.Overview), textNode("outline", ep.Overview), textNode("aired", ep.Date), textNode("year", year(ep.Date)), textNode("season", *ep.Season), textNode("episode", *ep.Number), textNode("rating", ep.Vote))
		}
		if flags["episode_credits"] {
			n.children = append(n.children, people(Credits{Cast: ep.Guests, Crew: ep.Crew}, func(string) string { return "" }, true)...)
		}
		b, err := xmlFile(n)
		if err != nil {
			return nil, err
		}
		out = append(out, b[len(xml.Header):]...)
	}
	if len(episodes) == 0 || len(out) > 2<<20 {
		return nil, ErrMetadata
	}
	return out, nil
}
