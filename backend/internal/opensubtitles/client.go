package opensubtitles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/browserruntime"
	"github.com/0xforee/nas-tools/backend/internal/bus"
)

type Query struct {
	Movie             bool
	IMDb, Name, Year  string
	Seasons, Episodes []int
}

func (q Query) Valid() bool {
	if !Text(q.Name, 600) || len(q.Year) > 4 || strings.Trim(q.Year, "0123456789") != "" || len(q.Seasons) > 100 || len(q.Episodes) > 100 {
		return false
	}
	if q.IMDb != "" && (!strings.HasPrefix(q.IMDb, "tt") || len(q.IMDb) < 3 || len(q.IMDb) > 21 || strings.Trim(q.IMDb[2:], "0123456789") != "") {
		return false
	}
	if !q.Movie && q.IMDb == "" {
		return false
	}
	for _, set := range [][]int{q.Seasons, q.Episodes} {
		seen := map[int]bool{}
		for _, n := range set {
			if n < 0 || n > 9999 || seen[n] {
				return false
			}
			seen[n] = true
		}
	}
	return !q.Movie || len(q.Seasons) == 0 && len(q.Episodes) == 0
}

type Candidate struct {
	ID, Title, Description, Link, FileName string
	Season, Episode                        int
}
type Session struct {
	Token, Host          string
	Expires, CachedUntil time.Time
}

func (s Session) Valid() bool {
	return Text(s.Token, 16000) && (s.Host == "api.opensubtitles.com" || s.Host == "vip-api.opensubtitles.com") && time.Now().Before(s.Expires)
}

// Step persists an intent before any network bytes. Finish records only a
// response digest, never credentials or signed links. Neither API nor caller
// may retry an operation whose intent was saved.
type Step func(context.Context, string, string) (finish func(string, string) error, err error)
type Client struct {
	Config    Config
	Transport http.RoundTripper
	Step      Step
	Gate      func(context.Context, string, string) error
	Session   Session
	Jar       http.CookieJar
	Renderer  browserruntime.Renderer
	Guard     func(context.Context) error
}

func Digest(data []byte) string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func (c *Client) call(ctx context.Context, kind, method, address string, body any, authenticated bool, limit int64) ([]byte, http.Header, int, error) {
	if c.Step == nil || limit <= 0 || limit > MaxDownload {
		return nil, nil, 0, ErrState
	}
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Port() != "" {
		return nil, nil, 0, ErrConfig
	}
	if c.Gate != nil {
		if e = c.Gate(ctx, u.Host, kind); e != nil {
			return nil, nil, 0, e
		}
	}
	var payload []byte
	if body != nil {
		payload, e = json.Marshal(body)
		if e != nil {
			return nil, nil, 0, ErrConfig
		}
	}
	request, e := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(payload))
	if e != nil {
		return nil, nil, 0, ErrConfig
	}
	request.GetBody = nil
	request.Close = true
	agent := c.Config.UserAgent
	if agent == "" {
		agent = DefaultAgent
	}
	request.Header.Set("User-Agent", agent)
	request.Header.Set("Accept", "application/json,text/html,application/zip,application/octet-stream;q=0.9")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		if c.Config.Provider != REST || u.Host != "api.opensubtitles.com" && u.Host != "vip-api.opensubtitles.com" {
			return nil, nil, 0, ErrConfig
		}
		request.Header.Set("Api-Key", c.Config.APIKey)
		if c.Session.Valid() {
			request.Header.Set("Authorization", "Bearer "+c.Session.Token)
		}
	}
	if c.Config.Provider == Legacy && c.Jar == nil {
		c.Jar, _ = cookiejar.New(nil)
		seed, _ := url.Parse("https://www.opensubtitles.org")
		h := http.Header{}
		h.Set("Cookie", c.Config.Cookie)
		cookies := (&http.Request{Header: h}).Cookies()
		if len(cookies) > 128 {
			return nil, nil, 0, ErrConfig
		}
		c.Jar.SetCookies(seed, cookies)
	}
	finish, e := c.Step(ctx, kind, bus.Hash([]any{method, address, payload}))
	if e != nil {
		return nil, nil, 0, e
	}
	if finish == nil {
		return nil, nil, 0, ErrState
	}
	transport := c.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if t, ok := transport.(*http.Transport); ok {
		// Never let the standard transport repeat a quota-consuming GET after a
		// reused connection fails. TLS verification and the supplied dialer remain.
		clone := t.Clone()
		clone.DisableKeepAlives = true
		clone.ForceAttemptHTTP2 = false
		clone.MaxResponseHeaderBytes = 64 << 10
		clone.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		transport = clone
	}
	client := &http.Client{Transport: transport, Jar: c.Jar, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(request)
	if e != nil {
		_ = finish("uncertain", "")
		return nil, nil, 0, ErrResponse
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if e != nil || int64(len(raw)) > limit {
		_ = finish("uncertain", "")
		return nil, nil, response.StatusCode, ErrResponse
	}
	state := "received"
	if response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 406 || response.StatusCode == 429 {
		state = "denied"
	}
	if e = finish(state, Digest(raw)); e != nil {
		return nil, nil, response.StatusCode, ErrState
	}
	if state == "denied" {
		return nil, response.Header, response.StatusCode, ErrQuota
	}
	return raw, response.Header, response.StatusCode, nil
}
func (c *Client) Login(ctx context.Context) (Session, error) {
	if c.Config.Provider != REST {
		return Session{}, ErrConfig
	}
	if c.Session.Valid() {
		return c.Session, nil
	}
	raw, _, status, e := c.call(ctx, "login", "POST", "https://api.opensubtitles.com/api/v1/login", map[string]string{"username": c.Config.Username, "password": c.Config.Password}, true, 2<<20)
	if e != nil {
		return Session{}, e
	}
	if status != 200 {
		return Session{}, ErrResponse
	}
	var result struct {
		Token  string `json:"token"`
		Host   string `json:"base_url"`
		Status int    `json:"status"`
	}
	if Decode(string(raw), &result) != nil || !Text(result.Token, 16000) || result.Status != 200 || result.Host != "api.opensubtitles.com" && result.Host != "vip-api.opensubtitles.com" {
		return Session{}, ErrResponse
	}
	expiry := time.Now().Add(15 * time.Minute)
	if parts := strings.Split(result.Token, "."); len(parts) == 3 {
		if raw, e := base64.RawURLEncoding.DecodeString(parts[1]); e == nil && len(raw) < 16000 {
			var claims struct {
				Expires int64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Expires > time.Now().Unix()+30 {
				expiry = time.Unix(claims.Expires, 0).Add(-15 * time.Second)
				if max := time.Now().Add(24 * time.Hour); expiry.After(max) {
					expiry = max
				}
			}
		}
	}
	c.Session = Session{Token: result.Token, Host: result.Host, Expires: expiry, CachedUntil: time.Now().Add(15 * time.Minute)}
	return c.Session, nil
}
func (c *Client) Search(ctx context.Context, q Query) ([]Candidate, error) {
	if !q.Valid() {
		return nil, ErrConfig
	}
	if c.Config.Provider == Legacy {
		return c.searchLegacy(ctx, q)
	}
	if c.Config.Provider != REST {
		return nil, ErrConfig
	}
	if _, e := c.Login(ctx); e != nil {
		return nil, e
	}
	queries := []url.Values{}
	base := url.Values{"languages": {"zh-cn,zh-tw"}}
	if q.Movie {
		base.Set("type", "movie")
		if q.IMDb != "" {
			base.Set("imdb_id", trimID(q.IMDb))
		} else {
			base.Set("query", strings.ToLower(q.Name))
			if q.Year != "" {
				base.Set("year", q.Year)
			}
		}
		queries = append(queries, base)
	} else {
		base.Set("parent_imdb_id", trimID(q.IMDb))
		base.Set("type", "episode")
		seasons := q.Seasons
		if len(seasons) == 0 {
			seasons = []int{-1}
		}
		episodes := q.Episodes
		if len(episodes) == 0 {
			episodes = []int{-1}
		}
		if len(seasons)*len(episodes) > 100 {
			return nil, ErrLimit
		}
		for _, season := range seasons {
			for _, episode := range episodes {
				v := url.Values{}
				for k, val := range base {
					v[k] = append([]string{}, val...)
				}
				if season >= 0 {
					v.Set("season_number", strconv.Itoa(season))
				}
				if episode >= 0 {
					v.Set("episode_number", strconv.Itoa(episode))
				}
				queries = append(queries, v)
			}
		}
	}
	out := []Candidate{}
	seen := map[string]bool{}
	for _, v := range queries {
		rows, e := c.searchREST(ctx, q, v)
		if e != nil {
			return nil, e
		}
		for _, row := range rows {
			if !seen[row.ID] {
				seen[row.ID] = true
				out = append(out, row)
			}
			if len(out) >= 100 {
				return out, nil
			}
		}
	}
	return out, nil
}
func trimID(s string) string {
	s = strings.TrimPrefix(s, "tt")
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}
func contains(set []int, n int) bool {
	if len(set) == 0 {
		return true
	}
	for _, v := range set {
		if v == n {
			return true
		}
	}
	return false
}
func number(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, _ := strconv.ParseInt(x.String(), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}
func str(v any) string            { s, _ := v.(string); return s }
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func array(v any) []any           { a, _ := v.([]any); return a }
func (c *Client) searchREST(ctx context.Context, q Query, v url.Values) ([]Candidate, error) {
	address := "https://" + c.Session.Host + "/api/v1/subtitles?" + v.Encode()
	var raw []byte
	for redirect := 0; ; redirect++ {
		data, h, status, e := c.call(ctx, "search", "GET", address, nil, true, 4<<20)
		if e != nil {
			return nil, e
		}
		if status == 200 {
			raw = data
			break
		}
		if status != 301 && status != 302 && status != 307 && status != 308 || redirect >= 2 {
			return nil, ErrResponse
		}
		old, _ := url.Parse(address)
		next, e := old.Parse(h.Get("Location"))
		if e != nil || next.Scheme != "https" || next.Host != old.Host || next.Path != old.Path || next.User != nil || next.Fragment != "" || next.Query().Encode() != v.Encode() {
			return nil, ErrResponse
		}
		address = next.String()
	}
	var result map[string]any
	if Decode(string(raw), &result) != nil || result == nil {
		return nil, ErrResponse
	}
	rows, ok := result["data"].([]any)
	if !ok || len(rows) > 1000 {
		return nil, ErrResponse
	}
	out := []Candidate{}
	for _, value := range rows {
		row := object(value)
		a := object(row["attributes"])
		feature := object(a["feature_details"])
		if a == nil || feature == nil {
			return nil, ErrResponse
		}
		language := str(a["language"])
		if language != "zh-cn" && language != "zh-tw" {
			continue
		}
		season, episode := int(number(feature["season_number"])), int(number(feature["episode_number"]))
		if !q.Movie && (!contains(q.Seasons, season) || !contains(q.Episodes, episode)) {
			continue
		}
		if q.Movie {
			if str(feature["feature_type"]) != "Movie" {
				continue
			}
			if q.IMDb != "" && number(feature["imdb_id"]) > 0 && strconv.FormatInt(number(feature["imdb_id"]), 10) != trimID(q.IMDb) {
				continue
			}
			if q.IMDb == "" && (str(feature["title"]) != q.Name || q.Year != "" && strconv.FormatInt(number(feature["year"]), 10) != q.Year) {
				continue
			}
		} else if str(feature["feature_type"]) != "Episode" {
			continue
		}
		for _, value := range array(a["files"]) {
			file := object(value)
			id := number(file["file_id"])
			name := str(file["file_name"])
			if id <= 0 || id > 2147483647 || !Text(name, 2048) || Extension(name) == "" && strings.ToLower(pathExtension(name)) != ".zip" {
				continue
			}
			out = append(out, Candidate{ID: strconv.FormatInt(id, 10), Title: str(feature["title"]), Description: str(a["release"]), FileName: name, Season: season, Episode: episode})
			if len(out) >= 100 {
				return out, nil
			}
		}
	}
	return out, nil
}
func pathExtension(s string) string {
	at := strings.LastIndex(s, ".")
	if at < 0 {
		return ""
	}
	return s[at:]
}
func (c *Client) Download(ctx context.Context, candidate Candidate) ([]File, error) {
	if c.Config.Provider == Legacy {
		if !legacyURL(candidate.Link) {
			return nil, ErrConfig
		}
		address := candidate.Link
		for n := 0; n < 4; n++ {
			raw, h, status, e := c.call(ctx, "archive", "GET", address, nil, false, MaxDownload)
			if e != nil {
				return nil, e
			}
			if status == 200 {
				return Archive(raw)
			}
			if status != 301 && status != 302 && status != 303 && status != 307 && status != 308 {
				return nil, ErrResponse
			}
			before, _ := url.Parse(address)
			next, e := before.Parse(h.Get("Location"))
			if e != nil || !legacyURL(next.String()) {
				return nil, ErrResponse
			}
			address = next.String()
		}
		return nil, ErrResponse
	}
	if c.Config.Provider != REST || !c.Session.Valid() {
		return nil, ErrConfig
	}
	id, e := strconv.ParseInt(candidate.ID, 10, 32)
	if e != nil || id <= 0 {
		return nil, ErrConfig
	}
	raw, _, status, e := c.call(ctx, "download-grant", "POST", "https://"+c.Session.Host+"/api/v1/download", map[string]int64{"file_id": id}, true, 2<<20)
	if e != nil {
		return nil, e
	}
	if status != 200 {
		return nil, ErrResponse
	}
	var grant struct {
		Link string `json:"link"`
		Name string `json:"file_name"`
	}
	if Decode(string(raw), &grant) != nil || !downloadURL(grant.Link) || !Text(grant.Name, 2048) {
		return nil, ErrResponse
	}
	address := grant.Link
	for redirect := 0; ; redirect++ {
		data, h, code, err := c.call(ctx, "subtitle", "GET", address, nil, false, MaxDownload)
		if err != nil {
			return nil, err
		}
		if code == 200 {
			raw = data
			break
		}
		if code != 301 && code != 302 && code != 303 && code != 307 && code != 308 || redirect >= 3 {
			return nil, ErrResponse
		}
		before, _ := url.Parse(address)
		next, err := before.Parse(h.Get("Location"))
		if err != nil || !downloadURL(next.String()) {
			return nil, ErrResponse
		}
		address = next.String()
	}
	if bytes.HasPrefix(raw, []byte{'P', 'K', 3, 4}) {
		return Archive(raw)
	}
	ext := Extension(grant.Name)
	if ext == "" || !SubtitleBytes(raw) {
		return nil, ErrResponse
	}
	return []File{{Extension: ext, Data: raw}}, nil
}
func legacyURL(address string) bool {
	u, e := url.Parse(address)
	return e == nil && u.Scheme == "https" && (u.Host == "www.opensubtitles.org" || u.Host == "dl.opensubtitles.org") && u.User == nil && u.Fragment == "" && len(address) <= 8192
}
func downloadURL(address string) bool {
	u, e := url.Parse(address)
	return e == nil && u.Scheme == "https" && (u.Host == "www.opensubtitles.com" || u.Host == "dl.opensubtitles.com") && strings.HasPrefix(u.Path, "/download/") && u.User == nil && u.Fragment == "" && len(address) <= 16000
}
