package sitesignin

import (
	"crypto/rand"
	"math/big"
	"regexp"
	"strings"

	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

func nodes(page, selector string) []*html.Node {
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		return nil
	}
	matcher, err := cascadia.Compile(selector)
	if err != nil {
		return nil
	}
	return cascadia.QueryAll(doc, matcher)
}
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func nodeText(n *html.Node) string {
	if n == nil {
		return ""
	}
	if n.Type == html.TextNode {
		return n.Data
	}
	if n.Type == html.CommentNode { return "" }
	if n.Type == html.ElementNode {
		switch n.Data { case "script", "style", "template", "noscript", "head": return "" }
		for _,a:=range n.Attr { if a.Key=="hidden" {return ""};if a.Key=="style" {style:=strings.ToLower(strings.Join(strings.Fields(a.Val),""));if strings.Contains(style,"display:none")||strings.Contains(style,"visibility:hidden"){return ""}} }
	}
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(nodeText(c))
	}
	return b.String()
}
func pageText(page string) string { doc,err:=html.Parse(strings.NewReader(page));if err!=nil{return ""};return nodeText(doc) }
func firstAttr(page, selector, key string) string {
	v := nodes(page, selector)
	if len(v) == 0 {
		return ""
	}
	return attr(v[0], key)
}
func matches(page string, patterns ...string) bool {
	for _, p := range patterns {
		if regexp.MustCompile(p).MatchString(page) {
			return true
		}
	}
	return false
}
func capture(page, pattern string) string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(page)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}
func randomIndex(size int) (int, error) {
	if size <= 0 {
		return 0, ErrResponse
	}
	n, e := rand.Int(rand.Reader, big.NewInt(int64(size)))
	if e != nil {
		return 0, e
	}
	return int(n.Int64()), nil
}
func choices(page, selector string) []Choice {
	out := []Choice{}
	seen := map[string]bool{}
	for _, n := range nodes(page, selector) {
		value := attr(n, "value")
		if value == "" || len(value) > 256 || seen[value] {
			continue
		}
		label := ""
		for c := n.NextSibling; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && (c.Data == "input" || c.Data == "br") {
				break
			}
			label += nodeText(c)
		}
		if id := attr(n, "id"); id != "" {
			for _, l := range nodes(page, "label") {
				if attr(l, "for") == id {
					label = nodeText(l)
					break
				}
			}
		}
		label = strings.TrimSpace(label)
		if len(label) > 2000 {
			continue
		}
		seen[value] = true
		out = append(out, Choice{value, label})
		if len(out) > 64 {
			return nil
		}
	}
	return out
}
