package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/siteconfig"
	"golang.org/x/net/html/charset"
)

const siteFeedLimit = 4 << 20

var errSiteFeed = errors.New("site feed unavailable")
var errExpiredSiteFeed = errors.New("site RSS link expired")
var titleBracket = regexp.MustCompile(`\[(.*?)\]`)
var titleBracketSpan = regexp.MustCompile(`\[(.*)\]`)

// KeepFRDS moves the release title out of the bracketed catalogue tags.
func siteFeedTitle(host, title string) string {
	if !strings.EqualFold(strings.TrimPrefix(strings.ToLower(host), "www."), "pt.keepfrds.com") {
		return title
	}
	match := titleBracketSpan.FindStringSubmatchIndex(title)
	if match == nil {
		return title
	}
	if match[0] == 0 {
		matches := titleBracket.FindAllStringSubmatchIndex(title, -1)
		if len(matches) < 2 {
			return title
		}
		match = matches[len(matches)-1]
	}
	return title[match[2]:match[3]] + " " + strings.TrimSpace(strings.ReplaceAll(title, title[match[0]:match[1]], ""))
}

type siteFeedArticle struct {
	rawTitle    string
	Title       string  `json:"title"`
	Enclosure   string  `json:"enclosure"`
	Description string  `json:"description"`
	Link        *string `json:"link"`
	Size        int64   `json:"size"`
	PubDate     string  `json:"pubdate"`
}

func siteFeedExpired(body []byte) bool {
	switch string(bytes.TrimSpace(body)) {
	case "RSS 链接已过期, 您需要获得一个新的!", "RSS 链接已过期，您需要获得一个新的！", "RSS Link has expired, You need to get a new one!":
		return true
	}
	return false
}

func parseClassicSiteFeed(body []byte, host string) ([]siteFeedArticle, error) {
	if siteFeedExpired(body) {
		return nil, errExpiredSiteFeed
	}
	items, err := readSiteFeedItems(body, false)
	if err != nil {
		return nil, err
	}
	out := make([]siteFeedArticle, 0, len(items))
	for _, item := range items {
		title := strings.TrimSpace(item.Title)
		page := strings.TrimSpace(item.Link)
		enclosure := strings.TrimSpace(item.Enclosure.URL)
		link := &page
		if enclosure == "" {
			enclosure = page
			link = nil
		}
		if title == "" || enclosure == "" {
			continue
		}
		n, err := strconv.ParseUint(item.Enclosure.Length, 10, 63)
		if err != nil {
			n = 0
		}
		out = append(out, siteFeedArticle{rawTitle: title, Title: siteFeedTitle(host, title), Enclosure: enclosure, Link: link, Description: item.Description, Size: int64(n), PubDate: item.PubDate})
	}
	return out, nil
}

func validSiteFeedURL(u *url.URL) bool {
	return u != nil && u.Hostname() != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil
}

func fetchSiteFeed(ctx context.Context, address string, proxy bool, app map[string]any) ([]siteFeedArticle, error) {
	target, err := url.Parse(address)
	if err != nil || !validSiteFeedURL(target) {
		return nil, errSiteFeed
	}
	client := http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !validSiteFeedURL(req.URL) {
			return errSiteFeed
		}
		// The referrer may contain the original feed passkey.
		req.Header.Del("Referer")
		return nil
	}}
	if proxy {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		proxies := objectValue(app["proxies"])
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			host := req.URL.Hostname()
			if strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback() {
				return nil, nil
			}
			raw := text(proxies[req.URL.Scheme])
			if raw == "" {
				return http.ProxyFromEnvironment(req)
			}
			return siteconfig.ProxyURL(raw)
		}
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, errSiteFeed
	}
	req.Header.Set("User-Agent", defaultString(text(app["user_agent"]), defaultSiteUserAgent))
	resp, err := client.Do(req)
	if err != nil {
		return nil, errSiteFeed
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errSiteFeed
	}
	data, err := readSiteFeedBody(resp.Body)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	// An XML declaration is handled by the decoder. Some sites specify
	// the encoding only in their HTTP header, including plain expiry messages.
	decl, _, _ := bytes.Cut(bytes.TrimSpace(data), []byte("?>"))
	_, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if label := params["charset"]; label != "" && !strings.EqualFold(label, "utf-8") && !(bytes.HasPrefix(decl, []byte("<?xml")) && bytes.Contains(decl, []byte("encoding"))) {
		reader, err := charset.NewReaderLabel(label, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		data, err = readSiteFeedBody(reader)
		if err != nil {
			return nil, err
		}
	}
	return parseClassicSiteFeed(data, target.Hostname())
}

func readSiteFeedBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, siteFeedLimit+1))
	if err != nil || len(data) > siteFeedLimit {
		return nil, errSiteFeed
	}
	return data, nil
}

// Resolve processed enclosures in one read snapshot, leaving the brush task's
// independent cache and the order of unprocessed entries intact.
func filterRecordedArticles(ctx context.Context, path string, articles []siteFeedArticle) ([]siteFeedArticle, error) {
	if len(articles) == 0 {
		return articles, nil
	}
	service := subscriptionService{databasePath: (config.Application{Path: path}).UserDatabasePath()}
	db, err := service.openNativeSubscriptionDatabase(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	keys := make([]string, len(articles))
	for i, item := range articles {
		keys[i] = item.Enclosure
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT value FROM json_each(?) WHERE EXISTS(SELECT 1 FROM RSS_TORRENTS WHERE ENCLOSURE=value)`, string(b))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		seen[k] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := articles[:0]
	for _, item := range articles {
		if !seen[item.Enclosure] {
			result = append(result, item)
		}
	}
	return result, nil
}

// The response stays on the local helper pipe; feed URLs and enclosure keys
// must never appear in diagnostic output.
func SiteFeedCommand(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	var q struct {
		URL                    string
		Proxy, ExcludeRecorded bool
	}
	input := &io.LimitedReader{R: in, N: 64 << 10}
	d := json.NewDecoder(input)
	d.DisallowUnknownFields()
	if d.Decode(&q) != nil || d.Decode(new(any)) != io.EOF || input.N == 0 {
		return errSiteFeed
	}
	cfg, err := config.NewStore(path).Snapshot()
	if err != nil {
		return err
	}
	items, err := fetchSiteFeed(ctx, q.URL, q.Proxy, objectValue(cfg["app"]))
	if errors.Is(err, errExpiredSiteFeed) {
		return json.NewEncoder(out).Encode(nil)
	}
	if err != nil {
		return err
	}
	if q.ExcludeRecorded {
		items, err = filterRecordedArticles(ctx, path, items)
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(items)
}
