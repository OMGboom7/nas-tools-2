// Package douban reproduces the readonly Frodo protocol already used by the
// legacy backend. It never accepts user credentials or arbitrary upstream URLs.
package douban

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var ErrResponse = errors.New("Douban metadata unavailable or inconsistent")

const projectKey = "054022eaeae0b00e0fc068c0c0a2102a"
const signingKey = "bf7dddc7c9cfe6f7"

type Client struct{ Transport http.RoundTripper }
type Subject struct{ ID, Title, Original, Year, IMDb, Kind string }
type Candidate struct{ ID, Title, Year, Kind string }
type Celebrity struct {
	ID    json.RawMessage `json:"id"`
	Name  string          `json:"name"`
	Latin string          `json:"latin_name"`
	Role  string          `json:"character"`
}
type People struct {
	Directors []Celebrity     `json:"directors"`
	Actors    []Celebrity     `json:"actors"`
	ID        json.RawMessage `json:"id"`
}

func ID(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		s = n.String()
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != s {
		return ""
	}
	return s
}
func year(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return ""
		}
		s = n.String()
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1800 || v > 2200 {
		return ""
	}
	return strconv.Itoa(v)
}
func valid(kind, id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return (kind == "movie" || kind == "tv") && err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}
func (c Client) get(ctx context.Context, resource string, q url.Values, out any) error {
	if ctx.Err() != nil {
		return ErrResponse
	}
	u := url.URL{Scheme: "https", Host: "frodo.douban.com", Path: "/api/v2" + resource}
	ts := time.Now().Format("20060102")
	mac := hmac.New(sha1.New, []byte(signingKey))
	_, _ = mac.Write([]byte("GET&" + url.QueryEscape(u.Path) + "&" + ts))
	if q == nil {
		q = url.Values{}
	}
	q.Set("apiKey", projectKey)
	q.Set("os_rom", "android")
	q.Set("_ts", ts)
	q.Set("_sig", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	u.RawQuery = q.Encode()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ErrResponse
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "MicroMessenger/")
	r.Header.Set("Referer", "https://servicewechat.com/wx2f9b06c1de1ccfca/91/page-frame.html")
	client := http.Client{Transport: c.Transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(r)
	if err != nil {
		return ErrResponse
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ErrResponse
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 || ctx.Err() != nil {
		return ErrResponse
	}
	var envelope struct {
		Error string `json:"localized_message"`
	}
	if json.Unmarshal(b, &envelope) != nil || envelope.Error != "" || json.Unmarshal(b, out) != nil {
		return ErrResponse
	}
	return nil
}
func (c Client) Subject(ctx context.Context, kind, id string) (Subject, error) {
	s := Subject{}
	if !valid(kind, id) {
		return s, ErrResponse
	}
	var p struct {
		ID       json.RawMessage `json:"id"`
		Title    string          `json:"title"`
		Original string          `json:"original_title"`
		Year     json.RawMessage `json:"year"`
		IMDb     string          `json:"imdb"`
		Kind     string          `json:"type"`
	}
	if c.get(ctx, "/"+kind+"/"+id, nil, &p) != nil || ID(p.ID) != id || strings.TrimSpace(p.Title) == "" || p.Kind != "" && p.Kind != kind {
		return s, ErrResponse
	}
	s = Subject{ID: id, Title: strings.TrimSpace(p.Title), Original: strings.TrimSpace(p.Original), Year: year(p.Year), IMDb: p.IMDb, Kind: kind}
	if s.Original == "" {
		s.Original = s.Title
	}
	return s, nil
}
func (c Client) Search(ctx context.Context, query string) ([]Candidate, error) {
	if strings.TrimSpace(query) == "" || len(query) > 512 || strings.ContainsFunc(query, unicode.IsControl) {
		return nil, ErrResponse
	}
	var p struct {
		Total *int `json:"total"`
		More  bool `json:"has_more"`
		Items []struct {
			ID     json.RawMessage `json:"target_id"`
			Kind   string          `json:"target_type"`
			Target struct {
				ID    json.RawMessage `json:"id"`
				Title string          `json:"title"`
				Year  json.RawMessage `json:"year"`
				Kind  string          `json:"type"`
			} `json:"target"`
		} `json:"items"`
	}
	if c.get(ctx, "/search/movie", url.Values{"q": {query}, "start": {"0"}, "count": {"100"}}, &p) != nil || p.Items == nil || len(p.Items) > 100 || p.More || p.Total != nil && (*p.Total < 0 || *p.Total > len(p.Items)) {
		return nil, ErrResponse
	}
	out := []Candidate{}
	for _, v := range p.Items {
		id := ID(v.ID)
		if id == "" {
			continue
		}
		if len(v.Target.ID) > 0 && ID(v.Target.ID) != id {
			continue
		}
		kind := v.Kind
		if kind == "" {
			kind = v.Target.Kind
		}
		if kind != "" && v.Target.Kind != "" && kind != v.Target.Kind {
			continue
		}
		out = append(out, Candidate{ID: id, Title: v.Target.Title, Year: year(v.Target.Year), Kind: kind})
	}
	return out, nil
}
func (c Client) People(ctx context.Context, kind, id string) (People, error) {
	p := People{}
	if !valid(kind, id) {
		return p, ErrResponse
	}
	if c.get(ctx, "/"+kind+"/"+id+"/celebrities", nil, &p) != nil || p.Directors == nil || p.Actors == nil || len(p.Directors) > 1024 || len(p.Actors) > 2048 || len(p.ID) > 0 && ID(p.ID) != id {
		return People{}, ErrResponse
	}
	return p, nil
}
