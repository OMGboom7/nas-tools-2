package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/net/html"
)

const bingStrongSelectors = "#sp_requery strong, #sp_recourse strong, #tile_link_cn strong, .b_ad .ad_esltitle~div strong, h2 strong, .b_caption p strong, .b_snippetBigText strong, .recommendationsTableTitle+.b_slideexp strong, .recommendationsTableTitle+table strong, .recommendationsTableTitle+ul strong, .pageRecoContainer .b_module_expansion_control strong, .pageRecoContainer .b_title>strong, .b_rs strong, .b_rrsr strong, #dict_ans strong, .b_listnav>.b_ans_stamp>strong, #b_content #ans_nws .na_cnt strong, .adltwrnmsg strong"

var (
	tmdbResultAnchors = cascadia.MustCompile("a[data-id][href]")
	bingHighlights    = cascadia.MustCompile(bingStrongSelectors)
	baiduHighlights   = cascadia.MustCompile("em")
	bingEntity        = cascadia.MustCompile("aside h2.b_entityTitle")
	bingMovie         = cascadia.MustCompile(`aside div[data-feedbk-ids="Movie"]`)
	entityYear        = regexp.MustCompile(`\s*\([0-9]{4}\)$`)
	subtitleGroup     = regexp.MustCompile(`^[\pL\pN_]+字幕[组社]?`)
	tmdbPagePath      = regexp.MustCompile(`^/(movie|tv)/([1-9][0-9]*)(-[^/]+)?/?$`)
)

func titleWebPage(ctx context.Context, transport http.RoundTripper, agent, address string, args url.Values) (*html.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"?"+args.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "text/html")
	client := http.Client{Timeout: 5 * time.Second, Transport: transport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if next.URL.Scheme != "https" || next.URL.Host != via[0].URL.Host || next.URL.User != nil || len(via) > 4 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("public title search unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("public title search returned a non-success status")
	}
	const limit = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(body) > limit {
		return nil, errors.New("public title search response unavailable or too large")
	}
	return html.Parse(bytes.NewReader(body))
}

func anchorValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// Track provider identities rather than slug spelling. Never follow a link from
// the results page. The authenticated detail request uses the configured API host.
func singleWebIdentity(doc *html.Node, tvOnly bool) (kind, id string) {
	for _, node := range cascadia.QueryAll(doc, tmdbResultAnchors) {
		if tvOnly && anchorValue(node, "data-media-type") != "tv" {
			continue
		}
		href := anchorValue(node, "href")
		u, err := url.Parse(href)
		if err != nil || u.IsAbs() || u.Host != "" {
			continue
		}
		if !strings.HasPrefix(u.Path, "/movie/") && !strings.HasPrefix(u.Path, "/tv/") {
			continue
		}
		fields := tmdbPagePath.FindStringSubmatch(u.Path)
		if fields == nil || !numericTMDBID(fields[2]) || tvOnly && fields[1] != "tv" {
			return "", ""
		}
		if value := anchorValue(node, "data-id"); numericTMDBID(value) && value != fields[2] {
			return "", ""
		}
		if id != "" && (id != fields[2] || kind != fields[1]) {
			return "", ""
		}
		kind, id = fields[1], fields[2]
	}
	return kind, id
}

var keywordNoise = []string{"HRHDTVAC3264", "人人影视制作", "www66ystv", "www6vhaotv", "西班牙语", "未删减版", "人人影视", "无删减版", "完全版", "未删减", "字幕组", "完成版", "中字", "韩语", "双字", "中英", "日语", "双语", "国粤", "HD", "BD", "中日", "粤语", "法语", "国语", "英语", "德意"}

type keywordScore struct {
	name  string
	score float64
}
type titleKeywords struct {
	order  []*keywordScore
	byName map[string]*keywordScore
	movie  bool
}

func (scores *titleKeywords) add(name string, amount float64) {
	if s := scores.byName[name]; s != nil {
		s.score += amount
		return
	}
	s := &keywordScore{name, amount}
	scores.byName[name] = s
	scores.order = append(scores.order, s)
}
func highlightWeight(count, index int) float64 {
	switch {
	case index < 2:
		return 10
	case count < 5:
		return 2
	case count < 10 && index < count/2:
		return 2
	case count < 10:
		return 1
	case index < count/4:
		return 3
	case index < count/2:
		return 2
	default:
		return 0.5
	}
}
func firstText(n *html.Node) string {
	if n != nil && n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
		return n.FirstChild.Data
	}
	return ""
}
func (scores *titleKeywords) collect(ctx context.Context, doc *html.Node, title string, bing bool) error {
	selector := baiduHighlights
	if bing {
		selector = bingHighlights
	}
	highlights := []string{}
	words := strings.Split(title, "")
	remainingComparisons := 16 << 20
	for i, n := range cascadia.QueryAll(doc, selector) {
		if i >= 4096 {
			return errors.New("too many title search highlights")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s := firstText(n)
		if s == "" || len(s) > 2048 {
			continue
		}
		candidate := strings.Split(s, "")
		// The best possible ratio can be computed from the input lengths.
		if 10*min(len(words), len(candidate)) <= len(words)+len(candidate) {
			continue
		}
		remainingComparisons -= len(words) * len(candidate)
		if remainingComparisons < 0 {
			return errors.New("title search highlights exceed comparison budget")
		}
		if difflib.NewMatcher(words, candidate).Ratio() > 0.2 {
			highlights = append(highlights, s)
		}
	}
	if bing && len(highlights) > 0 {
		if name := firstText(cascadia.Query(doc, bingEntity)); name != "" {
			scores.add(entityYear.ReplaceAllString(name, ""), 200)
			scores.movie = cascadia.Query(doc, bingMovie) != nil
		}
	}
	for i, s := range highlights {
		scores.add(strings.ToLower(s), highlightWeight(len(highlights), i))
	}
	return nil
}
func (scores *titleKeywords) winner() string {
	sort.SliceStable(scores.order, func(i, j int) bool { return scores.order[i].score > scores.order[j].score })
	if len(scores.order) == 0 {
		return ""
	}
	first := scores.order[0]
	if len(scores.order) > 1 {
		second := scores.order[1]
		if strings.Contains(second.name, first.name) && first.score < 100 && int(first.score)-int(second.score) <= 30 && strings.TrimSpace(strings.ReplaceAll(second.name, first.name, "")) != first.name {
			return second.name
		}
	}
	return first.name
}

func guessTitleKeyword(ctx context.Context, transport http.RoundTripper, agent, title string) (string, bool, error) {
	title = subtitleGroup.ReplaceAllString(title, "")
	for _, noise := range keywordNoise {
		title = strings.ReplaceAll(title, noise, " ")
	}
	if strings.TrimSpace(title) == "" {
		return "", false, nil
	}
	scores := titleKeywords{byName: map[string]*keywordScore{}}
	var incomplete error
	for _, p := range []struct {
		address string
		query   url.Values
		bing    bool
	}{
		{"https://www.cn.bing.com/search", url.Values{"q": {title}, "qs": {"n"}, "form": {"QBRE"}, "sp": {"-1"}}, true},
		{"https://www.baidu.com/s", url.Values{"wd": {title}, "ie": {"utf-8"}, "tn": {"baiduhome_pg"}}, false},
	} {
		doc, err := titleWebPage(ctx, transport, agent, p.address, p.query)
		if err == nil {
			err = scores.collect(ctx, doc, title, p.bing)
		}
		if err != nil {
			incomplete = err
		}
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
	}
	if name := scores.winner(); name != "" {
		return name, scores.movie, nil
	}
	return "", false, incomplete
}
