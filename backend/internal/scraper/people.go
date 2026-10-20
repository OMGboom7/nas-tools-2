package scraper

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0xforee/nas-tools/backend/internal/douban"
	"github.com/longbridgeapp/opencc"
	"golang.org/x/text/unicode/norm"
)

var chineseConverter struct {
	once  sync.Once
	mu    sync.Mutex
	value *opencc.OpenCC
	err   error
}

func simplified(s string) (string, bool) {
	if s == "" || len(s) > 1024 || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return "", false
	}
	chineseConverter.once.Do(func() { chineseConverter.value, chineseConverter.err = opencc.New("t2s") })
	if chineseConverter.err != nil {
		return "", false
	}
	chineseConverter.mu.Lock()
	out, err := chineseConverter.value.Convert(s)
	chineseConverter.mu.Unlock()
	return out, err == nil
}
func chineseName(s string) (string, bool) {
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		return "", false
	}
	s, ok := simplified(s)
	if !ok {
		return "", false
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s), true
}
func titleKey(s string) string {
	v, ok := simplified(s)
	if !ok {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, v)
}

// Compare whole name tokens, permitting reversed given/family names but never
// matching Li inside Liam or Ann inside Deanna. Chinese aliases use exact text.
func personKey(s string) string {
	if v, ok := chineseName(s); ok {
		return "zh:" + v
	}
	if len(s) > 1024 || strings.ContainsFunc(s, unicode.IsControl) {
		return ""
	}
	s = norm.NFKD.String(strings.ToLower(s))
	tokens := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.Is(unicode.Mn, r) })
	for i, t := range tokens {
		tokens[i] = strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Mn, r) {
				return -1
			}
			return r
		}, t)
	}
	if len(tokens) == 0 || len(tokens) == 1 && utf8.RuneCountInString(tokens[0]) < 3 {
		return ""
	}
	sort.Strings(tokens)
	return "latin:" + strings.Join(tokens, " ")
}
func matchCelebrity(aliases []string, people []douban.Celebrity) (douban.Celebrity, bool) {
	keys := map[string]bool{}
	for _, s := range aliases {
		if key := personKey(s); key != "" {
			keys[key] = true
		}
	}
	var selected douban.Celebrity
	foundID := ""
	for _, p := range people {
		id := douban.ID(p.ID)
		if id == "" {
			continue
		}
		if _, ok := chineseName(p.Name); !ok {
			continue
		}
		if !keys[personKey(p.Latin)] && !keys[personKey(p.Name)] {
			continue
		}
		if foundID != "" && (foundID != id || titleKey(selected.Name) != titleKey(p.Name) || selected.Role != p.Role) {
			return douban.Celebrity{}, false
		}
		selected, foundID = p, id
	}
	return selected, foundID != ""
}
func sameMediaTitle(candidate string, d Detail, kind string) bool {
	key := titleKey(candidate)
	if key == "" {
		return false
	}
	a, b := d.Title, d.OriginalTitle
	if kind == "tv" {
		a, b = d.Name, d.OriginalName
	}
	return key == titleKey(a) || key == titleKey(b)
}
func sameMediaYear(candidate string, d Detail, kind string) bool {
	date := d.Date
	if kind == "tv" {
		date = d.FirstDate
	}
	expected := year(date)
	if expected == "" || candidate == "" {
		return false
	}
	a, e := strconv.Atoi(candidate)
	b, e2 := strconv.Atoi(expected)
	if e != nil || e2 != nil {
		return false
	}
	if kind == "movie" {
		return a >= b-1 && a <= b+1
	}
	return a == b
}
func (c Client) doubanPeople(ctx context.Context, d Detail, kind string) (douban.People, bool) {
	client := douban.Client{Transport: c.Transport}
	title := d.Title
	if kind == "tv" {
		title = d.Name
	}
	items, err := client.Search(ctx, title)
	if err != nil {
		return douban.People{}, false
	}
	id := ""
	for _, v := range items {
		if v.Kind != "" && v.Kind != kind || !sameMediaTitle(v.Title, d, kind) || !sameMediaYear(v.Year, d, kind) {
			continue
		}
		if id != "" && id != v.ID {
			return douban.People{}, false
		}
		id = v.ID
	}
	if id == "" {
		return douban.People{}, false
	}
	s, err := client.Subject(ctx, kind, id)
	if err != nil || !sameMediaTitle(s.Title, d, kind) && !sameMediaTitle(s.Original, d, kind) || !sameMediaYear(s.Year, d, kind) {
		return douban.People{}, false
	}
	imdb := d.External.IMDb
	if imdb == "" {
		imdb = d.IMDb
	}
	if imdb != "" && s.IMDb != "" && s.IMDb != imdb {
		return douban.People{}, false
	}
	p, err := client.People(ctx, kind, id)
	return p, err == nil
}
func (c Client) localizePeople(ctx context.Context, d *Detail, kind string) []string {
	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	people, haveDouban := c.doubanPeople(ctx, *d, kind)
	cache := map[int64][]string{}
	requests, missing := 0, 0
	aliases := func(p Person) []string {
		if v, ok := cache[p.ID]; ok {
			return append(append([]string{}, v...), p.Name)
		}
		v := []string{}
		if p.ID > 0 && requests < 128 && ctx.Err() == nil {
			requests++
			var person struct {
				ID      int64    `json:"id"`
				Name    string   `json:"name"`
				Aliases []string `json:"also_known_as"`
			}
			if c.tmdb(ctx, "/person/"+strconv.FormatInt(p.ID, 10), "", &person) == nil && person.ID == p.ID && len(person.Aliases) <= 256 {
				for _, s := range person.Aliases {
					if len(s) <= 512 {
						v = append(v, s)
					}
				}
				v = append(v, person.Name)
			}
		}
		cache[p.ID] = v
		return append(append([]string{}, v...), p.Name)
	}
	convert := func(p Person, candidates []douban.Celebrity, actor bool) Person {
		if ctx.Err() != nil {
			if name, ok := chineseName(p.Name); ok {
				p.Name = name
			} else {
				missing++
			}
			return p
		}
		if p.ID <= 0 {
			missing++
			return p
		}
		list := []string{p.Name}
		match, matched := matchCelebrity(list, candidates)
		if !matched {
			if _, ok := chineseName(p.Name); !ok || haveDouban {
				list = aliases(p)
				match, matched = matchCelebrity(list, candidates)
			}
		}
		if matched {
			if name, ok := chineseName(match.Name); ok {
				p.Name = name
				role := strings.TrimSpace(match.Role)
				if actor && (strings.HasPrefix(role, "饰") || strings.HasPrefix(role, "飾")) {
					role = strings.TrimSpace(string([]rune(role)[1:]))
					if role != "" && role != "演员" && role != "演員" && len(role) <= 1024 {
						p.Character = role
					}
				}
				return p
			}
		}
		for _, s := range list {
			if name, ok := chineseName(s); ok {
				p.Name = name
				return p
			}
		}
		missing++
		return p
	}
	for i, p := range d.Credits.Crew {
		if p.Job == "Director" {
			d.Credits.Crew[i] = convert(p, people.Directors, false)
		}
	}
	for i, p := range d.Credits.Cast {
		if p.Department == "Acting" {
			d.Credits.Cast[i] = convert(p, people.Actors, true)
		}
	}
	if missing > 0 {
		return []string{"Chinese names unavailable or ambiguous for " + strconv.Itoa(missing) + " cast/crew entries; original TMDB names retained"}
	}
	return nil
}
