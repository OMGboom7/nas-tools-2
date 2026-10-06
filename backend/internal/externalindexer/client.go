package externalindexer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrConfig = errors.New("invalid external indexer configuration")
var ErrResponse = errors.New("external indexer response is unavailable or invalid")

type Config struct{ Kind, Host, APIKey, Password string }
type Indexer struct {
	ID, Name, Domain, RemoteID, Kind string
	Public                           bool
}

const maxResponse = 2 << 20

func validText(text string, limit int) bool {
	return text != "" && len(text) <= limit && !strings.ContainsFunc(text, unicode.IsControl)
}

func baseURL(cfg Config) (*url.URL, error) {
	if cfg.Kind != "Jackett" && cfg.Kind != "Prowlarr" || !validText(cfg.APIKey, 4096) || len(cfg.Password) > 4096 || strings.ContainsFunc(cfg.Password, unicode.IsControl) {
		return nil, ErrConfig
	}
	raw := strings.TrimSpace(cfg.Host)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || len(raw) > 2048 {
		return nil, ErrConfig
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func endpoint(base *url.URL, path string) *url.URL {
	next := *base
	next.Path += path
	next.RawPath = ""
	return &next
}

// Administrator-selected endpoints may be local services. Never follow
// redirects or put credentials in error messages, response DTOs, or URLs.
func Discover(ctx context.Context, cfg Config, transport http.RoundTripper) ([]Indexer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err := baseURL(cfg)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Transport: transport, Jar: jar, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if cfg.Kind == "Jackett" && cfg.Password != "" {
		login := endpoint(base, "/UI/Dashboard")
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, login.String(), strings.NewReader(url.Values{"password": {cfg.Password}}.Encode()))
		if err != nil {
			return nil, ErrConfig
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("X-Api-Key", cfg.APIKey)
		response, err := client.Do(r)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrResponse
		}
		readBytes, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponse+1))
		response.Body.Close()
		if readErr != nil || readBytes > maxResponse || response.StatusCode < 200 || response.StatusCode >= 400 {
			return nil, ErrResponse
		}
		if response.StatusCode >= 300 {
			next, err := response.Location()
			if err != nil || next.Scheme != base.Scheme || next.Host != base.Host || next.User != nil {
				return nil, ErrResponse
			}
		}
	}
	path := "/api/v1/indexerstats"
	if cfg.Kind == "Jackett" {
		path = "/api/v2.0/indexers"
	}
	u := endpoint(base, path)
	if cfg.Kind == "Jackett" {
		u.RawQuery = "configured=true"
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrConfig
	}
	r.Header.Set("X-Api-Key", cfg.APIKey)
	r.Header.Set("Accept", "application/json")
	response, err := client.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrResponse
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse || !utf8.Valid(body) {
		return nil, ErrResponse
	}
	items := []Indexer{}
	if cfg.Kind == "Jackett" {
		var rows []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		}
		if json.Unmarshal(body, &rows) != nil || rows == nil || len(rows) > 1024 {
			return nil, ErrResponse
		}
		for _, row := range rows {
			if !validText(row.ID, 128) || row.ID == "." || row.ID == ".." || !validText(row.Name, 256) {
				return nil, ErrResponse
			}
			for _, c := range row.ID {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
					return nil, ErrResponse
				}
			}
			if row.Type != "public" && row.Type != "private" && row.Type != "semi-private" {
				return nil, ErrResponse
			}
			items = append(items, Indexer{ID: row.ID + "-jackett", Name: row.Name + "(Jackett)", Domain: endpoint(base, "/api/v2.0/indexers/"+row.ID+"/results/torznab/").String(), RemoteID: row.ID, Kind: cfg.Kind, Public: row.Type == "public"})
		}
	} else {
		var document struct {
			Indexers []struct {
				ID   int64  `json:"indexerId"`
				Name string `json:"indexerName"`
			} `json:"indexers"`
		}
		if json.Unmarshal(body, &document) != nil || document.Indexers == nil || len(document.Indexers) > 1024 {
			return nil, ErrResponse
		}
		for _, row := range document.Indexers {
			if row.ID <= 0 || !validText(row.Name, 256) {
				return nil, ErrResponse
			}
			id := strconv.FormatInt(row.ID, 10)
			items = append(items, Indexer{ID: row.Name + "-prowlarr", Name: row.Name + "(Prowlarr)", Domain: endpoint(base, "/api/v1/indexer/"+id).String(), RemoteID: id, Kind: cfg.Kind, Public: true})
		}
	}
	seen := map[string]bool{}
	remote := map[string]bool{}
	for _, item := range items {
		if seen[item.ID] || remote[item.RemoteID] {
			return nil, ErrResponse
		}
		seen[item.ID] = true
		remote[item.RemoteID] = true
	}
	return items, nil
}
