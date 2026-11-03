package sitestats

import (
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

func contains(s string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}
func nodes(page, selector string) []*html.Node {
	root, _ := html.Parse(strings.NewReader(page))
	if root == nil {
		return nil
	}
	return query(root, selector)
}
func query(root *html.Node, selector string) []*html.Node {
	if root == nil {
		return nil
	}
	s, e := cascadia.Compile(selector)
	if e != nil {
		return nil
	}
	return cascadia.QueryAll(root, s)
}
func attr(n *html.Node, key string) string {
	if n != nil {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
	}
	return ""
}
func text(n *html.Node) string {
	if n == nil || n.Type == html.CommentNode {
		return ""
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.ElementNode && contains("|script|style|head|noscript|", "|"+n.Data+"|") {
		return ""
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(text(c))
		if c.Type == html.ElementNode {
			b.WriteByte(' ')
		}
	}
	return strings.TrimSpace(b.String())
}
func first(page, selector string) string {
	ns := nodes(page, selector)
	if len(ns) == 0 {
		return ""
	}
	return text(ns[0])
}
func firstAttr(page, selector, key string) string {
	ns := nodes(page, selector)
	if len(ns) == 0 {
		return ""
	}
	return attr(ns[0], key)
}
func capture(s, pattern string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}
func number(s string) float64 {
	s = capture(s, `[-+]?([\d,]+(?:\.\d+)?)`)
	v, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0
	}
	return v
}
func integer(v any) int64 {
	s := ""
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	case nil:
		return 0
	default:
		return 0
	}
	i, e := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if e != nil || i < 0 {
		return 0
	}
	return i
}

func validIntegers(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		s := str(m[k])
		if !regexp.MustCompile(`^\d+$`).MatchString(s) {
			return false
		}
		if _, e := strconv.ParseInt(s, 10, 64); e != nil {
			return false
		}
	}
	return true
}
func addSeed(s *Snapshot, peers, bytes int64) error {
	if peers < 0 || bytes < 0 || bytes > math.MaxInt64-s.SeedingSize {
		return ErrResponse
	}
	if len(s.Seeds) >= MaxSeeds {
		return ErrPage
	}
	s.Seeds = append(s.Seeds, [2]int64{peers, bytes})
	s.Seeding++
	s.SeedingSize += bytes
	return nil
}

var sizePattern = regexp.MustCompile(`(?i)([\d,]+(?:\.\d+)?)\s*([KMGTPE]?)(?:i?B)?`)

func size(s string) (int64, bool) {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if len(m) < 3 {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(strings.ReplaceAll(m[1], ",", ""))
	if !ok {
		return 0, false
	}
	p := strings.Index(" KMGTPE", strings.ToUpper(m[2]))
	if m[2] == "" {
		p = 0
	}
	if p < 0 {
		return 0, false
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), uint(10*p))))
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if !n.IsInt64() {
		return 0, false
	}
	return n.Int64(), true
}
func date(s string) string {
	s = strings.TrimSpace(strings.Split(s, " (")[0])
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05.000Z07:00", time.RFC3339, "2006-01-02", "02 Jan 2006", "2006/01/02 15:04:05"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.Format("2006-01-02 15:04:05")
		}
	}
	return s
}
func nextSibling(n *html.Node) *html.Node {
	if n == nil {
		return nil
	}
	for n = n.NextSibling; n != nil; n = n.NextSibling {
		if n.Type == html.ElementNode {
			return n
		}
	}
	return nil
}
func valueNode(page string, labels ...string) *html.Node {
	for _, n := range nodes(page, "td, th, li, span, dt, em") {
		v := strings.Trim(strings.TrimSpace(text(n)), ":：")
		for _, label := range labels {
			if strings.EqualFold(v, label) {
				if n.Data == "em" {
					return n.Parent
				}
				return nextSibling(n)
			}
		}
	}
	return nil
}
func label(page string, labels ...string) string { return text(valueNode(page, labels...)) }
func traffic(s *Snapshot, page string) {
	flat := first(page, "body")
	if flat == "" {
		flat = textFromFragment(page)
	}
	for _, pair := range []struct {
		labels string
		dest   *int64
		found  *bool
	}{{`(?:(?:^|[^总])(?:上传量?|上傳量?)|Uploaded|Upload)`, &s.Upload, &s.uploadFound}, {`(?:(?:^|[^总子影力])(?:下载量?|下載量?)|Downloaded|Download)`, &s.Download, &s.downloadFound}} {
		v := capture(flat, `(?i)`+pair.labels+`\s*[:：]?\s*([\d,.]+\s*[KMGTPE]?i?B)`)
		if v != "" {
			if n, ok := size(v); ok {
				*pair.dest = n
				*pair.found = true
			}
		}
	}
	if v := capture(flat, `(?:分享率|Ratio)\s*[:：]?\s*([\d,.]+)`); v != "" {
		s.Ratio = number(v)
	}
	if v := capture(flat, `(?:下载中|Torrents leeching)\D*(\d+)`); v != "" {
		s.Leeching = int64(number(v))
	}
}
func textFromFragment(page string) string {
	root, _ := html.Parse(strings.NewReader(page))
	return text(root)
}
func assignSize(s *Snapshot, upload bool, value string) {
	if n, ok := size(value); ok {
		if upload {
			s.Upload = n
			s.uploadFound = true
		} else {
			s.Download = n
			s.downloadFound = true
		}
	}
}
func decode(data []byte) (map[string]any, error) {
	var v map[string]any
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.UseNumber()
	if e := d.Decode(&v); e != nil || v == nil {
		return nil, ErrResponse
	}
	return v, nil
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	}
	return ""
}
func list(v any) []any { a, _ := v.([]any); return a }
