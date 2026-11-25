package searchresults

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Rows arrive in result order. The classic page iterates the object keys in
// that order for titles, quality groups and releases tied on the same season.
type ordered[T any] struct {
	keys   []string
	values map[string]T
}

func (m *ordered[T]) entry(key string, create func() T) T {
	if v, ok := m.values[key]; ok {
		return v
	}
	if m.values == nil {
		m.values = make(map[string]T)
	}
	v := create()
	m.keys = append(m.keys, key)
	m.values[key] = v
	return v
}

func (m *ordered[T]) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(m.values[k])
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type qualityGroup struct {
	Info     row               `json:"group_info"`
	Total    int               `json:"group_total"`
	Variants ordered[*variant] `json:"group_torrents"`
}

type variant struct {
	Info     row   `json:"unique_info"`
	Torrents []row `json:"torrent_list"`
}

type promotion struct {
	Value string `json:"value"`
	Name  string `json:"name"`
}

type facets struct {
	Sites   []any       `json:"site"`
	Free    []promotion `json:"free"`
	Teams   []string    `json:"releasegroup"`
	Pixels  []string    `json:"respix"`
	Videos  []string    `json:"video"`
	Seasons []string    `json:"season"`
}

type titleGroup struct {
	header  row
	filters facets
	seasons ordered[*ordered[*qualityGroup]]
}

var seasonEpisode = regexp.MustCompile(`(?i)(s[0-9]{1,2})[^0-9]*?(e)`)

func seasonSortKey(se string) (string, string) {
	parts := strings.Fields(seasonEpisode.ReplaceAllString(se, "${1} ${2}"))
	if len(parts) > 1 {
		return parts[0], parts[1]
	}
	return "Z" + strings.Join(parts, ""), "ZZZ"
}

func (t *titleGroup) finish() {
	sort.SliceStable(t.seasons.keys, func(i, j int) bool {
		a, b := seasonSortKey(t.seasons.keys[i])
		c, d := seasonSortKey(t.seasons.keys[j])
		if a != c {
			return a > c
		}
		return b > d
	})
	entries := make([][2]any, 0, len(t.seasons.keys))
	for _, se := range t.seasons.keys {
		entries = append(entries, [2]any{se, t.seasons.values[se]})
	}
	sort.Sort(sort.Reverse(sort.StringSlice(t.filters.Seasons)))
	sort.Slice(t.filters.Teams, func(i, j int) bool {
		a, b := t.filters.Teams[i], t.filters.Teams[j]
		return a != "未知" && (b == "未知" || a < b)
	})
	t.header["torrent_dict"] = entries
	t.header["filter"] = t.filters
}

func (t *titleGroup) MarshalJSON() ([]byte, error) { return json.Marshal(t.header) }
func addFacet[T comparable](list *[]T, value T) {
	if !slices.Contains(*list, value) {
		*list = append(*list, value)
	}
}

func compactKey(parts ...string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || strings.ContainsRune("-.@|", r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.Join(parts, "_"))
}

func displayText(v any) string {
	if v == nil {
		return "None"
	}
	return fmt.Sprint(v)
}

func storedFactor(v any) any {
	if n, ok := v.(float64); ok {
		// Keep both the JSON number and the classic page's string filter key
		// compatible with Python's float representation.
		format := byte('f')
		if magnitude := math.Abs(n); magnitude != 0 && (magnitude < 1e-4 || magnitude >= 1e16) {
			format = 'e'
		}
		text := strconv.FormatFloat(n, format, -1, 64)
		if !strings.ContainsAny(text, ".eE") {
			text += ".0"
		}
		return json.Number(text)
	}
	return v
}

var promotionNames = map[string]string{"1.0 1.0": "普通", "1.0 0.0": "免费", "2.0 1.0": "2X", "2.0 0.0": "2X免费", "1.0 0.5": "50%", "2.0 0.5": "2X 50%", "1.0 0.7": "70%", "1.0 0.3": "30%"}

func describePromotion(ul, dl any) promotion {
	value := displayText(ul) + " " + displayText(dl)
	name := promotionNames[value]
	if name == "" {
		name = "未知"
	}
	return promotion{value, name}
}

func grouped(rows []row) row {
	titles := new(ordered[*titleGroup])
	for _, item := range rows {
		var labels map[string]string
		if raw, ok := item["RES_TYPE"].(string); ok && raw != "" {
			if json.Unmarshal([]byte(raw), &labels) != nil {
				continue
			}
		}
		pix, codec, source, effect := labels["respix"], labels["video_encode"], labels["restype"], labels["reseffect"]
		title := displayText(item["TITLE"])
		if year, _ := item["YEAR"].(string); year != "" {
			title += " (" + year + ")"
		}
		kind, _ := item["TYPE"].(string)
		t := titles.entry(title, func() *titleGroup {
			var kindName any
			switch kind {
			case "MOV":
				kindName = "电影"
			case "TV":
				kindName = "电视剧"
			case "ANI":
				kindName = "动漫"
			}
			return &titleGroup{
				header: row{
					"key": item["ID"], "title": item["TITLE"], "year": item["YEAR"],
					"type_key": kind, "type": kindName, "vote": item["VOTE"], "tmdbid": item["TMDBID"],
					"image": item["IMAGE"], "backdrop": item["IMAGE"], "poster": item["POSTER"],
					"overview": item["OVERVIEW"], "fav": 0, "rssid": nil,
				},
				filters: facets{Videos: []string{}, Seasons: []string{}},
			}
		})
		se, _ := item["ES_STRING"].(string)
		if se == "" || kind == "MOV" {
			se = "MOV"
		}
		groups := t.seasons.entry(se, func() *ordered[*qualityGroup] { return new(ordered[*qualityGroup]) })
		group := groups.entry(compactKey(pix, source), func() *qualityGroup { return &qualityGroup{Info: row{"respix": pix, "restype": source}} })
		key := compactKey(pix, source, codec, effect, displayText(item["SIZE"]), displayText(item["OTHERINFO"]))
		v := group.Variants.entry(key, func() *variant {
			return &variant{Info: row{"video_encode": codec, "size": item["SIZE"], "reseffect": effect, "releasegroup": item["OTHERINFO"]}}
		})
		tags := []string{}
		for _, tag := range strings.Split(displayText(item["NOTE"]), "|") {
			switch tag {
			case "官方", "官组", "中字", "国语", "特效", "特效字幕":
				tags = append(tags, tag)
			}
		}
		ul, dl := item["UPLOAD_VOLUME_FACTOR"], item["DOWNLOAD_VOLUME_FACTOR"]
		v.Torrents = append(v.Torrents, row{
			"id": item["ID"], "seeders": item["SEEDERS"], "enclosure": item["ENCLOSURE"],
			"site": item["SITE"], "torrent_name": item["TORRENT_NAME"], "description": item["DESCRIPTION"],
			"pageurl": item["PAGEURL"], "uploadvalue": ul, "downloadvalue": dl, "size": item["SIZE"],
			"respix": pix, "restype": source, "reseffect": effect, "releasegroup": item["OTHERINFO"],
			"video_encode": codec, "labels": tags,
		})
		group.Total++
		addFacet(&t.filters.Sites, item["SITE"])
		addFacet(&t.filters.Free, describePromotion(ul, dl))
		team := displayText(item["OTHERINFO"])
		if item["OTHERINFO"] == nil {
			team = "未知"
		}
		addFacet(&t.filters.Teams, team)
		if pix == "" {
			pix = "未知分辨率"
		}
		addFacet(&t.filters.Pixels, pix)
		if codec != "" {
			addFacet(&t.filters.Videos, codec)
		}
		if se != "MOV" && se != "TV" {
			parts := strings.Fields(se)
			if len(parts) > 0 {
				addFacet(&t.filters.Seasons, parts[0])
			}
		}
	}
	for _, title := range titles.values {
		title.finish()
	}
	return row{"code": 0, "total": len(rows), "result": titles}
}
